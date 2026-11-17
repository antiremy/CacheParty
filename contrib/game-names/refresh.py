#!/usr/bin/env python3
"""Resolve download events and reconcile observed IDs with cached name sources."""

import argparse
import collections
import concurrent.futures
import contextlib
import datetime as dt
import fcntl
import hashlib
import json
import os
import re
import sqlite3
import sys
import time
import urllib.request
import urllib.error
from pathlib import Path


STATE = Path("./game-names-state")
SOURCE = Path(__file__).resolve().parent
RELEASE = "https://api.github.com/repos/regix1/lancache-pics/releases/latest"
PRODUCTS = "https://raw.githubusercontent.com/tpill90/battlenet-lancache-prefill/master/BattleNetPrefill/TactProduct.cs"
TARGET = Path("./game-names.json")
DASHBOARD = "http://127.0.0.1:8080"


def fetch(url, limit, timeout=30):
    request = urllib.request.Request(url, headers={"User-Agent": "cacheparty-game-names/1"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        body = response.read(limit + 1)
    if len(body) > limit:
        raise ValueError("Source exceeds size limit")
    return body


def save(path, data):
    temp = path.with_suffix(path.suffix + ".new")
    temp.write_text(json.dumps(data, ensure_ascii=True, sort_keys=True) + "\n")
    os.replace(temp, path)


@contextlib.contextmanager
def publication_lock():
    STATE.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (STATE / "refresh.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def steam_index_has_owners():
    database = STATE / "steam.sqlite"
    if database.exists():
        with contextlib.closing(sqlite3.connect(database)) as db, db:
            return "owner" in {row[1] for row in db.execute("PRAGMA table_info(names)")}
    return False


def refresh_steam(status):
    release = json.loads(fetch(RELEASE, 2_000_000))
    if status.get("tag") == release["tag_name"] and steam_index_has_owners():
        snapshot = dt.datetime.fromisoformat(status["snapshot"].replace("Z", "+00:00")).timestamp()
        if time.time() - snapshot > 7 * 86400:
            raise ValueError("Steam dataset has not updated for seven days")
        return
    asset = next(a for a in release["assets"] if a["name"] == "pics_depot_mappings.json")
    url = asset["browser_download_url"]
    if not url.startswith("https://github.com/regix1/lancache-pics/releases/download/"):
        raise ValueError("Unexpected Steam dataset URL")
    body = fetch(url, 150_000_000, 90)
    if asset.get("digest") != "sha256:" + hashlib.sha256(body).hexdigest():
        raise ValueError("Steam dataset digest mismatch")
    data = json.loads(body)
    snapshot = data["metadata"]["lastUpdated"]
    timestamp = dt.datetime.fromisoformat(snapshot.replace("Z", "+00:00")).timestamp()
    if time.time() - timestamp > 7 * 86400 or timestamp > time.time() + 3600:
        raise ValueError("Steam dataset timestamp is not current")
    rows = []
    for depot, entry in data["depotMappings"].items():
        if not depot.isdigit():
            raise ValueError("Invalid depot ID")
        ids, names = entry["appIds"], entry["appNames"]
        owner = entry.get("ownerId")
        if len(ids) != len(names) or not ids:
            raise ValueError("Invalid Steam owner/name alignment")
        if owner not in ids:
            continue
        name = names[ids.index(owner)]
        if isinstance(name, str) and name.strip() and len(name) <= 500:
            rows.append((depot, name.strip(), str(owner)))
    # Some upstream releases contain fewer distinct keys than their metadata
    # reports. Validate the actual index and reject a major later regression.
    previous_count = status.get("mappings", 0)
    if len(rows) < max(50_000, previous_count * 0.8):
        raise ValueError("Steam dataset unexpectedly small")
    candidate = STATE / "steam.sqlite.new"
    candidate.unlink(missing_ok=True)
    with contextlib.closing(sqlite3.connect(candidate)) as db, db:
        db.execute("CREATE TABLE names (depot TEXT PRIMARY KEY, name TEXT NOT NULL, owner TEXT NOT NULL)")
        db.executemany("INSERT INTO names VALUES (?, ?, ?)", rows)
    os.replace(candidate, STATE / "steam.sqlite")
    status.update(tag=release["tag_name"], snapshot=snapshot, mappings=len(rows), digest=asset["digest"])


def learn_steam_apps(depots, db):
    """Verify candidate app ownership in live Steam metadata; never guess a name."""
    db.execute("CREATE TABLE IF NOT EXISTS names (depot TEXT PRIMARY KEY, name TEXT, owner TEXT)")
    db.execute("CREATE TABLE IF NOT EXISTS removed_owners (depot TEXT, owner TEXT, checked REAL, PRIMARY KEY (depot, owner))")
    db.execute("CREATE TABLE IF NOT EXISTS apps (id TEXT PRIMARY KEY, checked REAL)")
    db.execute("CREATE TABLE IF NOT EXISTS app_failures (id TEXT PRIMARY KEY, next_check REAL)")
    db.execute("CREATE TABLE IF NOT EXISTS retries (depot TEXT PRIMARY KEY, checked REAL, next_check REAL DEFAULT 0)")
    if "next_check" not in {row[1] for row in db.execute("PRAGMA table_info(retries)")}:
        db.execute("ALTER TABLE retries ADD COLUMN next_check REAL DEFAULT 0")
    db.execute("CREATE TABLE IF NOT EXISTS owner_refs (source TEXT, depot TEXT, owner TEXT, PRIMARY KEY (source, depot))")
    now = time.time()
    known = [row[0] for row in db.execute("SELECT owner FROM names UNION SELECT owner FROM removed_owners")]
    # Optional seed owners accelerate discovery. Discovered owners
    # join this list automatically and refresh their complete depot lists.
    seeds = json.loads((SOURCE / "steam-app-ids.json").read_text())
    cached = dict(db.execute("SELECT id, checked FROM apps"))
    failures = dict(db.execute("SELECT id, next_check FROM app_failures"))
    def request_due(app):
        return cached.get(app, 0) < now - 3600 and failures.get(app, 0) <= now
    retries = {depot: (checked, due) for depot, checked, due in db.execute("SELECT depot, checked, next_check FROM retries")}
    pending = sorted(dict.fromkeys(depot for depot in depots if retries.get(depot, (0, 0))[1] <= now),
                     key=lambda depot: retries.get(depot, (0, 0))[0])[:3]
    required = {}
    candidates = []
    for depot in pending:
        # IDs near a depot are only candidates. The returned depots must
        # explicitly contain the requested ID before any title is accepted.
        owners = [row[0] for row in db.execute("SELECT owner FROM owner_refs WHERE depot = ?", (depot,))]
        owners += [str(int(depot) - offset) for offset in range(0, 11) if int(depot) > offset]
        required[depot] = set(owners)
        candidates.extend(owners)
    # New downloads get the budget first. Successful cached requests carry
    # unfinished candidate sets forward; failed depots back off independently.
    candidates.extend(seeds + known)
    queue = collections.deque(dict.fromkeys(app for app in candidates if request_due(app)))

    def app_info(app):
        try:
            data = json.loads(fetch(f"https://api.steamcmd.net/v1/info/{app}", 10_000_000, 10))
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return app, None
            raise
        if data.get("status") != "success" or not isinstance(data.get("data"), dict):
            raise ValueError("Invalid SteamCMD API response")
        if app not in data["data"]:
            raise ValueError("SteamCMD API omitted the requested app")
        return app, data["data"][app]

    errors = {}
    attempted = set()
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        while queue and len(attempted) < 50:
            batch = {}
            while queue and len(batch) < 4 and len(attempted) < 50:
                app = queue.popleft()
                if app in attempted or not request_due(app):
                    continue
                attempted.add(app)
                batch[pool.submit(app_info, app)] = app
            for future in concurrent.futures.as_completed(batch):
                app = batch[future]
                try:
                    _, info = future.result()
                    rows, refs = [], []
                    if info not in (None, {}):
                        if not isinstance(info, dict):
                            raise ValueError("Invalid Steam app record")
                        name, entries = info.get("common", {}).get("name"), info.get("depots", {})
                        if not isinstance(name, str) or not name.strip() or len(name) > 500 or not isinstance(entries, dict):
                            raise ValueError("Invalid Steam name/depot record")
                        for depot, entry in entries.items():
                            if not depot.isdigit():
                                continue
                            if not isinstance(entry, dict):
                                raise ValueError("Invalid Steam depot record")
                            owner = str(entry.get("depotfromapp", app))
                            if not owner.isdigit():
                                raise ValueError("Invalid Steam owner ID")
                            if owner == app:
                                rows.append((depot, name.strip(), app))
                            else:
                                refs.append((app, depot, owner))
                    # Reconcile a complete successful owner response atomically.
                    # A failed fetch/validation leaves its previous mappings intact.
                    with db:
                        current = {row[0] for row in db.execute("SELECT depot FROM names WHERE owner = ?", (app,))}
                        retained = {row[0] for row in rows}
                        db.executemany("INSERT OR REPLACE INTO removed_owners VALUES (?, ?, ?)",
                                       [(depot, app, now) for depot in current - retained])
                        db.execute("DELETE FROM names WHERE owner = ?", (app,))
                        db.execute("DELETE FROM owner_refs WHERE source = ?", (app,))
                        db.executemany("INSERT OR REPLACE INTO names VALUES (?, ?, ?)", rows)
                        db.executemany("DELETE FROM removed_owners WHERE depot = ? AND owner = ?",
                                       [(depot, owner) for depot, _, owner in rows])
                        db.executemany("INSERT OR REPLACE INTO owner_refs VALUES (?, ?, ?)", refs)
                        db.execute("INSERT OR REPLACE INTO apps VALUES (?, ?)", (app, now))
                        db.execute("DELETE FROM app_failures WHERE id = ?", (app,))
                    cached[app] = now
                    failures.pop(app, None)
                    for _, depot, owner in refs:
                        if depot in required:
                            required[depot].add(owner)
                        if request_due(owner) and owner not in attempted:
                            queue.appendleft(owner)
                except Exception as error:
                    errors[app] = str(error)
                    failures[app] = now + 600
                    db.execute("INSERT OR REPLACE INTO app_failures VALUES (?, ?)", (app, now + 600))
    for depot in pending:
        candidates = required[depot]
        blocked_until = [failures[app] for app in candidates if failures.get(app, 0) > now]
        if blocked_until:
            db.execute("INSERT OR REPLACE INTO retries VALUES (?, ?, ?)", (depot, now, min(blocked_until)))
        elif all(cached.get(app, 0) >= now - 3600 for app in candidates):
            db.execute("INSERT OR REPLACE INTO retries VALUES (?, ?, ?)", (depot, now, now + 3600))
        # Deferred candidates remain eligible for the next reconciliation.
    db.commit()
    if errors:
        print("SteamCMD API: " + "; ".join(f"{app}: {error}" for app, error in errors.items()), file=sys.stderr)
    return bool(errors)


def refresh_blizzard(status):
    body = fetch(PRODUCTS, 1_000_000).decode("utf-8-sig")
    products = {}
    for code, block in re.findall(r'new TactProduct\("([a-z0-9_]+)"\)\s*\{(.*?)\};', body, re.S):
        match = re.search(r'DisplayName\s*=\s*"([^"\\]+)"', block)
        if match:
            products[code] = match[1].strip()
    if len(products) < 20:
        raise ValueError("Blizzard product source changed format")

    def aliases(item):
        code, name = item
        text = fetch(f"https://us.version.battle.net/{code}/cdns", 200_000, 10).decode()
        lines = [line for line in text.splitlines() if line and not line.startswith("#")]
        headers = [field.split("!")[0] for field in lines[0].split("|")]
        path_index = headers.index("Path")
        result = {code.lower(): name}
        for line in lines[1:]:
            path = line.split("|")[path_index]
            alias = path.rstrip("/").rsplit("/", 1)[-1].lower()
            if re.fullmatch(r"[a-z0-9_-]+", alias):
                result[alias] = name
        return result

    # Keep known aliases if a publisher endpoint is temporarily unavailable.
    previous = STATE / "blizzard.json"
    result = json.loads(previous.read_text()) if previous.exists() else {}
    errors = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        jobs = {pool.submit(aliases, item): item[0] for item in products.items()}
        for future in concurrent.futures.as_completed(jobs):
            try:
                result.update(future.result())
            except Exception as error:
                errors.append(f"{jobs[future]}: {error}")
    result.update(products)
    save(previous, result)
    status.update(products=len(products), aliases=len(result), source_sha256=hashlib.sha256(body.encode()).hexdigest())
    if errors:
        raise RuntimeError("Some Blizzard CDN aliases could not refresh: " + "; ".join(errors))


def refresh_sources(status, new_products=()):
    now = time.time()
    steam = status.setdefault("steam", {})
    if not steam_index_has_owners() and not steam.get("error"):
        steam["next_check"] = 0
    state = status.setdefault("blizzard", {})
    pending = set(state.get("pending_products", []))
    pending.update(product.lower() for product in new_products)
    state["pending_products"] = sorted(pending)
    last_attempt = state.get("attempted_at", state.get("checked_at", 0))
    if pending and now - last_attempt >= 600 and (not state.get("error") or now >= state.get("next_check", 0)):
        state["next_check"] = 0
    for source, interval, refresh in (("steam", 3600, refresh_steam), ("blizzard", 86400, refresh_blizzard)):
        state = status.setdefault(source, {})
        if time.time() < state.get("next_check", 0):
            continue
        state["attempted_at"] = time.time()
        try:
            refresh(state)
            state.update(checked_at=time.time(), next_check=time.time() + interval, error=None)
        except Exception as error:
            state.update(next_check=time.time() + 600, error=str(error))
            print(f"{source}: {error}; retaining saved names", file=sys.stderr)
    catalogue = STATE / "blizzard.json"
    if pending and catalogue.exists():
        known = json.loads(catalogue.read_text())
        status["blizzard"]["pending_products"] = sorted(product for product in pending if product not in known)


def publish_names(payload):
    """Publish under publication_lock(), repairing missing or stale web assets."""
    text = json.dumps(payload, ensure_ascii=True, sort_keys=True) + "\n"
    if TARGET.exists() and TARGET.read_text() == text:
        return False
    TARGET.parent.mkdir(parents=True, exist_ok=True)
    save(TARGET, payload)
    return True


def steam_labels(depots, bootstrap, index, learned):
    """A successful live ownership removal overrides older fallback labels."""
    labels, app_ids = dict(bootstrap), {}
    removed = {(depot, owner) for depot, owner in learned.execute("SELECT depot, owner FROM removed_owners")}
    removed_depots = {depot for depot, _ in removed}
    for depot in removed_depots:
        labels.pop(depot, None)
    has_owner = "owner" in {row[1] for row in index.execute("PRAGMA table_info(names)")}
    for depot in depots:
        row = index.execute("SELECT name" + (", owner" if has_owner else ", NULL") +
                            " FROM names WHERE depot = ?", (depot,)).fetchone()
        if row and (depot, str(row[1])) not in removed:
            # Old index schemas cannot prove that an owner was not removed.
            if row[1] is not None or depot not in removed_depots:
                labels[depot] = row[0]
                if row[1] is not None:
                    app_ids[depot] = str(row[1])
        row = learned.execute("SELECT name, owner FROM names WHERE depot = ?", (depot,)).fetchone()
        if row:
            labels[depot], app_ids[depot] = row[0], row[1]
    return labels, app_ids


def main():
    global STATE, TARGET, DASHBOARD
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", type=Path, default=STATE)
    parser.add_argument("--output", type=Path, default=TARGET)
    parser.add_argument("--dashboard", default=DASHBOARD)
    parser.add_argument("--depot", action="append", default=[])
    parser.add_argument("--product", action="append", default=[])
    args = parser.parse_args()
    STATE, TARGET, DASHBOARD = args.state, args.output, args.dashboard.rstrip("/")
    if any(not depot.isdigit() for depot in args.depot) or any(
            not re.fullmatch(r"[A-Za-z0-9_-]+", product) for product in args.product):
        parser.error("Invalid download identifier")
    with publication_lock():
        status_path = STATE / "sources.json"
        status = json.loads(status_path.read_text()) if status_path.exists() else {}
        # Reconcile product IDs too, so a watcher restart or cachelog-json does
        # not require another download event to request an unknown product.
        requested_products = set(args.product)
        known_products = json.loads((SOURCE / "game-names.json").read_text())["blizzard"]["product"]
        catalogue = STATE / "blizzard.json"
        if catalogue.exists():
            known_products.update(json.loads(catalogue.read_text()))
        try:
            products = json.loads(fetch(DASHBOARD + "/api/services/blizzard", 2_000_000))
            requested_products.update(app["id"].lower() for app in products["apps"]
                                      if app["kind"] == "product" and app["id"].lower() not in known_products)
        except (OSError, ValueError, KeyError) as error:
            print(f"Blizzard traffic reconciliation: {error}", file=sys.stderr)
        refresh_sources(status, new_products=requested_products)
        save(status_path, status)
        names = json.loads((SOURCE / "game-names.json").read_text())
        app_ids = {}
        blizzard = STATE / "blizzard.json"
        if blizzard.exists():
            remote = json.loads(blizzard.read_text())
            names["blizzard"]["product"].update(remote)
        unknown = []
        observed = json.loads(fetch(DASHBOARD + "/api/services/steam", 2_000_000))
        present = {app["id"] for app in observed["apps"] if app["kind"] == "depot"}
        observed["apps"].extend({"kind": "depot", "id": depot} for depot in args.depot if depot not in present)
        database = STATE / "steam.sqlite"
        with contextlib.closing(sqlite3.connect(STATE / "learned.sqlite")) as learned, learned:
            learned.execute("CREATE TABLE IF NOT EXISTS names (depot TEXT PRIMARY KEY, name TEXT, owner TEXT)")
            learned.execute("CREATE TABLE IF NOT EXISTS removed_owners (depot TEXT, owner TEXT, checked REAL, PRIMARY KEY (depot, owner))")
            observed_depots = [app["id"] for app in observed["apps"] if app["kind"] == "depot"]
            with contextlib.closing(sqlite3.connect(f"file:{database}?mode=ro", uri=True)
                                    if database.exists() else sqlite3.connect(":memory:")) as index, index:
                if not database.exists():
                    index.execute("CREATE TABLE names (depot TEXT PRIMARY KEY, name TEXT, owner TEXT)")
                _, app_ids = steam_labels(observed_depots, names["steam"]["depot"], index, learned)
                unresolved = [depot for depot in observed_depots if depot not in app_ids]
                unresolved = [depot for depot in args.depot if depot in unresolved] + [
                    depot for depot in unresolved if depot not in args.depot]
                api_errors = learn_steam_apps(unresolved, learned)
                names["steam"]["depot"], app_ids = steam_labels(observed_depots, names["steam"]["depot"], index, learned)
        unknown = [depot for depot in observed_depots if depot not in names["steam"]["depot"]]
        payload = {"names": names, "steamSnapshot": status.get("steam", {}).get("snapshot"),
                   "steamAppIds": app_ids, "unknownSteamDepots": unknown}
        changed = publish_names(payload)
        print(json.dumps({"published": changed, "steam_snapshot": payload["steamSnapshot"],
                          "unknown_steam_depots": unknown, "source_errors": {k: v.get("error") for k, v in status.items() if v.get("error")}}))
        if api_errors or any(value.get("error") for value in status.values()):
            sys.exit(1)


if __name__ == "__main__":
    main()
