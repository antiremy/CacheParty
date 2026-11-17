import json
import contextlib
import io
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import refresh
from watch import download


class DownloadEvents(unittest.TestCase):
    def test_steam_event_from_any_client(self):
        for client in ("192.0.2.11", "192.0.2.12"):
            self.assertEqual(download(f'[steam] {client} - "GET /depot/1808501/chunk/hash HTTP/1.1" 200'),
                             ("steam", "depot", "1808501"))

    def test_blizzard_alias_and_service_scope(self):
        self.assertEqual(download('[blizzard] client "GET /tpr/Hero-Live-a/data/hash HTTP/1.1"'),
                         ("blizzard", "product", "hero-live-a"))
        self.assertIsNone(download('[other] client "GET /depot/1808501/chunk/hash HTTP/1.1"'))
        self.assertIsNone(download('[blizzard] client "GET /tpr/configs/data/hash HTTP/1.1"'))


class VerifiedSteamOwnership(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.source = Path(self.temp.name)
        (self.source / "steam-app-ids.json").write_text("[]")
        self.db = sqlite3.connect(":memory:")
        self.addCleanup(self.db.close)

    def lookup(self, info):
        def fetch(url, *args):
            app = url.rsplit("/", 1)[-1]
            return json.dumps({"status": "success", "data": {app: info if app == "101" else {}}}).encode()
        with patch.object(refresh, "SOURCE", self.source), patch.object(refresh, "fetch", side_effect=fetch):
            self.assertFalse(refresh.learn_steam_apps(["102"], self.db))

    def test_new_depot_resolves_without_seed_or_bulk_entry(self):
        self.lookup({"common": {"name": "A new game"}, "depots": {"102": {}}})
        self.assertEqual(self.db.execute("SELECT name, owner FROM names WHERE depot='102'").fetchone(),
                         ("A new game", "101"))

    def test_nearby_app_does_not_imply_ownership(self):
        self.lookup({"common": {"name": "Unrelated game"}, "depots": {"999": {}}})
        self.assertIsNone(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone())

    def test_shared_depot_does_not_take_downloading_games_name(self):
        self.lookup({"common": {"name": "A game"}, "depots": {"102": {"depotfromapp": "228980"}}})
        self.assertIsNone(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone())

    def test_source_outage_preserves_learned_names(self):
        self.lookup({"common": {"name": "Saved name"}, "depots": {"102": {}}})
        with patch.object(refresh, "SOURCE", self.source), patch.object(refresh, "fetch", side_effect=OSError("offline")):
            self.assertTrue(refresh.learn_steam_apps(["999"], self.db))
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone(), ("Saved name",))

    def run_lookup(self, depots, responses, now=10000):
        calls = []
        def fetch(url, *args):
            app = url.rsplit("/", 1)[-1]
            calls.append(app)
            info = responses.get(app, {})
            if isinstance(info, Exception):
                raise info
            return json.dumps({"status": "success", "data": {app: info}}).encode()
        with patch.object(refresh, "SOURCE", self.source), patch.object(refresh, "fetch", side_effect=fetch), \
                patch.object(refresh.time, "time", return_value=now), contextlib.redirect_stderr(io.StringIO()):
            errors = refresh.learn_steam_apps(depots, self.db)
        return calls, errors

    def test_shared_owner_is_queried_and_verified(self):
        calls, errors = self.run_lookup(["102"], {
            "101": {"common": {"name": "Downloading game"}, "depots": {"102": {"depotfromapp": "500"}}},
            "500": {"common": {"name": "Shared owner"}, "depots": {"102": {}}},
        })
        self.assertFalse(errors)
        self.assertIn("500", calls)
        self.assertEqual(self.db.execute("SELECT name, owner FROM names WHERE depot='102'").fetchone(), ("Shared owner", "500"))

    def test_new_downloads_get_capacity_before_fifty_stale_owners(self):
        (self.source / "steam-app-ids.json").write_text(json.dumps([str(n) for n in range(1000, 1051)]))
        calls, errors = self.run_lookup(["200000", "300000", "400000"], {
            "399990": {"common": {"name": "New game"}, "depots": {"400000": {}}},
        })
        self.assertFalse(errors)
        self.assertLessEqual(len(calls), 50)
        self.assertIn("399990", calls)
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='400000'").fetchone(), ("New game",))

    def test_deferred_owner_chain_continues_next_run(self):
        responses = {"102": {"common": {"name": "Proxy"}, "depots": {"102": {"depotfromapp": "500"}}}}
        for owner in range(500, 560):
            responses[str(owner)] = {"common": {"name": "Proxy"}, "depots": {"102": {"depotfromapp": str(owner + 1)}}}
        responses["560"] = {"common": {"name": "Actual owner"}, "depots": {"102": {}}}
        calls, errors = self.run_lookup(["102"], responses)
        self.assertFalse(errors)
        self.assertEqual(len(calls), 50)
        self.assertIsNone(self.db.execute("SELECT checked FROM retries WHERE depot='102'").fetchone())
        calls, errors = self.run_lookup(["102"], responses, now=10061)
        self.assertFalse(errors)
        self.assertIn("560", calls)
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone(), ("Actual owner",))

    def test_one_failed_depot_does_not_starve_fourth(self):
        responses = {"100": OSError("one app unavailable"),
                     "399": {"common": {"name": "Fourth game"}, "depots": {"400": {}}}}
        _, errors = self.run_lookup(["100", "200", "300", "400"], responses)
        self.assertTrue(errors)
        self.assertEqual(self.db.execute("SELECT next_check FROM retries WHERE depot='100'").fetchone(), (10600,))
        calls, errors = self.run_lookup(["100", "200", "300", "400"], responses, now=10061)
        self.assertFalse(errors)
        self.assertNotIn("100", calls)
        self.assertIn("399", calls)
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='400'").fetchone(), ("Fourth game",))

    def test_successful_refresh_removes_obsolete_ownership(self):
        self.run_lookup(["102"], {"101": {"common": {"name": "Old owner"}, "depots": {"102": {}}}})
        self.run_lookup(["102"], {"101": {"common": {"name": "Old owner"}, "depots": {}}}, now=14000)
        self.assertIsNone(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone())

    def test_transferred_owner_is_reconciled(self):
        self.run_lookup(["102"], {"101": {"common": {"name": "Old owner"}, "depots": {"102": {}}}})
        self.run_lookup(["102"], {
            "101": {"common": {"name": "Old owner"}, "depots": {"102": {"depotfromapp": "500"}}},
            "500": {"common": {"name": "New owner"}, "depots": {"102": {}}},
        }, now=14000)
        self.assertEqual(self.db.execute("SELECT name, owner FROM names WHERE depot='102'").fetchone(), ("New owner", "500"))

    def test_malformed_refresh_keeps_previous_owner(self):
        self.run_lookup(["102"], {"101": {"common": {"name": "Saved owner"}, "depots": {"102": {}}}})
        _, errors = self.run_lookup(["102"], {"101": {"common": {"name": "Bad record"}, "depots": []}}, now=14000)
        self.assertTrue(errors)
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone(), ("Saved owner",))

    def test_failed_known_owner_obeys_app_backoff_and_recovers(self):
        self.run_lookup(["102"], {"101": {"common": {"name": "Saved name"}, "depots": {"102": {}}}})
        calls, errors = self.run_lookup(["102"], {"101": OSError("offline")}, now=14000)
        self.assertTrue(errors)
        self.assertIn("101", calls)
        self.assertEqual(self.db.execute("SELECT next_check FROM app_failures WHERE id='101'").fetchone(), (14600,))
        calls, errors = self.run_lookup(["102"], {"101": OSError("offline")}, now=14061)
        self.assertFalse(errors)
        self.assertNotIn("101", calls)
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone(), ("Saved name",))
        calls, errors = self.run_lookup(["102"], {"101": {"common": {"name": "Recovered name"}, "depots": {"102": {}}}}, now=14601)
        self.assertFalse(errors)
        self.assertIn("101", calls)
        self.assertIsNone(self.db.execute("SELECT next_check FROM app_failures WHERE id='101'").fetchone())
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone(), ("Recovered name",))

    def test_new_reference_cannot_bypass_owner_failure_backoff(self):
        (self.source / "steam-app-ids.json").write_text('["500"]')
        _, errors = self.run_lookup([], {"500": OSError("offline")})
        self.assertTrue(errors)
        responses = {
            "101": {"common": {"name": "Downloading game"}, "depots": {"102": {"depotfromapp": "500"}}},
            "500": {"common": {"name": "Shared owner"}, "depots": {"102": {}}},
        }
        calls, errors = self.run_lookup(["102"], responses, now=10061)
        self.assertFalse(errors)
        self.assertIn("101", calls)
        self.assertNotIn("500", calls)
        self.assertEqual(self.db.execute("SELECT next_check FROM retries WHERE depot='102'").fetchone(), (10600,))
        calls, errors = self.run_lookup(["102"], responses, now=10601)
        self.assertFalse(errors)
        self.assertIn("500", calls)
        self.assertEqual(self.db.execute("SELECT name, owner FROM names WHERE depot='102'").fetchone(), ("Shared owner", "500"))

    def test_fifty_failed_owners_do_not_starve_healthy_fifty_first(self):
        (self.source / "steam-app-ids.json").write_text(json.dumps([str(n) for n in range(1000, 1051)]))
        responses = {str(n): OSError("offline") for n in range(1000, 1050)}
        responses["1050"] = {"common": {"name": "Healthy owner"}, "depots": {"900001": {}}}
        calls, errors = self.run_lookup([], responses)
        self.assertTrue(errors)
        self.assertEqual(len(calls), 50)
        self.assertNotIn("1050", calls)
        calls, errors = self.run_lookup([], responses, now=10061)
        self.assertFalse(errors)
        self.assertEqual(calls, ["1050"])
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='900001'").fetchone(), ("Healthy owner",))

    def remove_owner(self):
        self.run_lookup(["102"], {"101": {"common": {"name": "Old owner"}, "depots": {"102": {}}}})
        self.run_lookup(["102"], {"101": {"common": {"name": "Old owner"}, "depots": {}}}, now=14000)

    def verify_removed_publication(self, observed_ids):
        self.remove_owner()
        with contextlib.closing(sqlite3.connect(self.source / "learned.sqlite")) as learned:
            self.db.backup(learned)
        with contextlib.closing(sqlite3.connect(self.source / "steam.sqlite")) as index, index:
            index.execute("CREATE TABLE names (depot TEXT PRIMARY KEY, name TEXT, owner TEXT)")
            index.execute("INSERT INTO names VALUES ('102', 'Old bulk name', '101')")
        (self.source / "game-names.json").write_text(json.dumps({
            "steam": {"depot": {"102": "Old bootstrap name"}}, "blizzard": {"product": {}}}))
        observed = {"service": "steam", "apps": [{"kind": "depot", "id": depot} for depot in observed_ids]}
        with patch.object(refresh, "STATE", self.source), patch.object(refresh, "SOURCE", self.source), \
                patch.object(refresh, "refresh_sources"), patch.object(refresh, "fetch", return_value=json.dumps(observed).encode()), \
                patch.object(refresh, "publish_names", return_value=True) as publish, \
                patch.object(refresh.time, "time", return_value=14061), patch.object(refresh.sys, "argv", ["refresh.py"]), \
                contextlib.redirect_stdout(io.StringIO()):
            refresh.main()
        payload = publish.call_args.args[0]
        self.assertNotIn("102", payload["names"]["steam"]["depot"])
        self.assertNotIn("102", payload["steamAppIds"])
        self.assertEqual(payload["unknownSteamDepots"], observed_ids)

    def test_publication_does_not_resurrect_removed_owner_from_bulk_or_bootstrap(self):
        self.verify_removed_publication(["102"])

    def test_absent_depot_does_not_resurrect_removed_bootstrap(self):
        self.verify_removed_publication([])

    def test_verified_ownership_return_clears_its_removal(self):
        self.remove_owner()
        self.assertIsNotNone(self.db.execute("SELECT owner FROM removed_owners WHERE depot='102' AND owner='101'").fetchone())
        self.run_lookup(["102"], {"101": {"common": {"name": "Restored game"}, "depots": {"102": {}}}}, now=18000)
        self.assertIsNone(self.db.execute("SELECT owner FROM removed_owners WHERE depot='102' AND owner='101'").fetchone())
        self.assertEqual(self.db.execute("SELECT name FROM names WHERE depot='102'").fetchone(), ("Restored game",))

    def test_new_bulk_owner_is_not_blocked_by_an_old_owners_removal(self):
        self.remove_owner()
        with contextlib.closing(sqlite3.connect(":memory:")) as index, index:
            index.execute("CREATE TABLE names (depot TEXT PRIMARY KEY, name TEXT, owner TEXT)")
            index.execute("INSERT INTO names VALUES ('102', 'New bulk owner', '500')")
            names, owners = refresh.steam_labels(["102"], {"102": "Old bootstrap"}, index, self.db)
        self.assertEqual(names["102"], "New bulk owner")
        self.assertEqual(owners["102"], "500")


class Recovery(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.state = Path(directory.name)
        patcher = patch.object(refresh, "STATE", self.state)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_product_event_during_cooldown_retries_without_another_event(self):
        status = {"steam": {"next_check": 20000},
                  "blizzard": {"checked_at": 10000, "attempted_at": 10000, "next_check": 96400}}
        with patch.object(refresh, "steam_index_has_owners", return_value=True), \
                patch.object(refresh, "refresh_blizzard") as request, patch.object(refresh.time, "time", return_value=10060):
            refresh.refresh_sources(status, new_products=["Future-Product"])
        request.assert_not_called()
        # Round-trip through disk as the event worker and later timer do.
        refresh.save(self.state / "sources.json", status)
        status = json.loads((self.state / "sources.json").read_text())
        self.assertEqual(status["blizzard"]["pending_products"], ["future-product"])
        def publish_catalogue(state):
            (self.state / "blizzard.json").write_text('{"future-product":"Future Game"}')
        with patch.object(refresh, "steam_index_has_owners", return_value=True), \
                patch.object(refresh, "refresh_blizzard", side_effect=publish_catalogue) as request, \
                patch.object(refresh.time, "time", return_value=10661):
            refresh.refresh_sources(status)
        request.assert_called_once()
        self.assertEqual(status["blizzard"]["pending_products"], [])
        self.assertEqual(status["blizzard"]["next_check"], 10661 + 86400)


    def test_blizzard_failure_cooldown_applies_to_new_events(self):
        status = {"steam": {"next_check": 20000}, "blizzard": {"checked_at": 1}}
        with patch.object(refresh, "steam_index_has_owners", return_value=True), \
                patch.object(refresh, "refresh_blizzard", side_effect=OSError("offline")) as request, \
                patch.object(refresh.time, "time", return_value=10000), contextlib.redirect_stderr(io.StringIO()):
            refresh.refresh_sources(status, new_products=["future-product"])
        self.assertEqual(request.call_count, 1)
        with patch.object(refresh, "steam_index_has_owners", return_value=True), \
                patch.object(refresh, "refresh_blizzard") as request, patch.object(refresh.time, "time", return_value=10001):
            refresh.refresh_sources(status, new_products=["future-product"])
        request.assert_not_called()
        self.assertEqual(status["blizzard"]["attempted_at"], 10000)
        self.assertEqual(status["blizzard"]["next_check"], 10600)

    def test_owner_schema_upgrade_runs_without_bypassing_failure_backoff(self):
        status = {"steam": {"next_check": 20000}, "blizzard": {"next_check": 20000}}
        with patch.object(refresh, "steam_index_has_owners", return_value=False), \
                patch.object(refresh, "refresh_steam", side_effect=OSError("offline")) as request, \
                patch.object(refresh.time, "time", return_value=10000), contextlib.redirect_stderr(io.StringIO()):
            refresh.refresh_sources(status)
        self.assertEqual(request.call_count, 1)
        with patch.object(refresh, "steam_index_has_owners", return_value=False), \
                patch.object(refresh, "refresh_steam") as request, patch.object(refresh.time, "time", return_value=10001):
            refresh.refresh_sources(status)
        request.assert_not_called()





    def test_timer_recovers_unknown_blizzard_products_from_dashboard(self):
        (self.state / "game-names.json").write_text('{"steam":{"depot":{}},"blizzard":{"product":{"wow":"World of Warcraft"}}}')
        (self.state / "steam-app-ids.json").write_text('[]')
        def traffic(url, *args):
            return json.dumps({"apps": [{"kind": "product", "id": "Future-Product"},
                                       {"kind": "product", "id": "wow"}]} if url.endswith("/blizzard") else {"apps": []}).encode()
        with patch.object(refresh, "SOURCE", self.state), patch.object(refresh, "fetch", side_effect=traffic), \
                patch.object(refresh, "refresh_sources") as sources, \
                patch.object(refresh, "publish_names", return_value=False), \
                patch.object(refresh.sys, "argv", ["refresh.py"]), contextlib.redirect_stdout(io.StringIO()):
            refresh.main()
        self.assertEqual(sources.call_args.kwargs["new_products"], {"future-product"})

    def test_local_publication_repairs_missing_or_changed_file(self):
        target = self.state / "served" / "names.json"
        payload = {"names": {"steam": {"depot": {"102": "New name"}}}}
        with patch.object(refresh, "TARGET", target):
            self.assertTrue(refresh.publish_names(payload))
            self.assertEqual(json.loads(target.read_text()), payload)
            self.assertFalse(refresh.publish_names(payload))
            target.write_text("stale")
            self.assertTrue(refresh.publish_names(payload))
            target.unlink()
            self.assertTrue(refresh.publish_names(payload))


if __name__ == "__main__":
    unittest.main()
