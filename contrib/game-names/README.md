# Automatic game names

These optional Linux helpers resolve dashboard download IDs without changing
CacheParty's request handling. Python 3.9+ and GNU `tail` are sufficient; no pip
packages or API keys are needed. They run outside the cache process.

Start CacheParty with `GAME_NAMES_FILE` pointing at the helper's output. With a
container, use the host path that backs that file and the host path to its
`access.log`. For example, from the repository root:

```sh
python3 contrib/game-names/watch.py \
  --dashboard http://127.0.0.1:8080 \
  --access-log /data/logs/access.log \
  --state /data/game-names-state \
  --output /data/game-names.json
```

Run the watcher under your usual service supervisor so it restarts after a
failure. Give its user read access to the log, write access to the output's
parent directory and exclusive use of the state directory. Keep the state
outside `CACHE_ROOT`. The cache process only needs read access to the JSON.
Mount its parent directory rather than an individual file because publication
uses atomic replacement.

A previously unseen Steam depot or Blizzard product in the standard `cachelog`
format triggers a debounced lookup, whichever LAN client downloaded it. The
watcher follows log rotation, batches events, runs one resolver at a time, and
also reconciles the dashboard every minute. `cachelog-json` omits request paths,
so it uses the periodic reconciliation instead. A restart recovers Steam and Blizzard IDs
from the dashboard's saved traffic totals. Unknown Blizzard requests received
during a source cooldown are saved in state and retried without another event.
Client addresses and chunk URLs are not written to lookup state.

For a one-off refresh (or your own scheduler instead of the watcher):

```sh
python3 contrib/game-names/refresh.py \
  --dashboard http://127.0.0.1:8080 \
  --state /data/game-names-state \
  --output /data/game-names.json
```

Do not run a watcher and independent resolver with different state directories
against the same output. A shared state directory serializes publication with
an advisory lock. A source error returns a nonzero exit status after preserving
usable names; the watcher retries through its minute reconciliation.

## Sources and limits

SteamDB is the link destination, not a scraped data source. Steam data comes
from [lancache-pics](https://github.com/regix1/lancache-pics) release snapshots
(hourly checks with digest, size and freshness checks), plus the
[SteamCMD API](https://github.com/SteamCMDAPI/SteamCMDAPI) for live app metadata.
A nearby app ID is only a candidate: a name is accepted only if the app explicitly
owns the depot. Shared-depot owner references are followed and verified.
`steam-app-ids.json` may contain additional known app IDs to accelerate discovery;
it starts with Steamworks Common Redistributables.

Each run tries at most 50 live app requests, four at a time, prioritizing new
downloads. Failures back off for ten minutes; successful owners refresh after an
hour. Deferred work remains eligible on subsequent runs. Successfully removed
ownership is remembered so an older bulk snapshot cannot restore it. Names and
ownership are published only for observed depots.

Blizzard codes and CDN aliases come from the maintained
[BattleNetPrefill product catalogue](https://github.com/tpill90/battlenet-lancache-prefill/blob/master/BattleNetPrefill/TactProduct.cs)
and Blizzard's `us.version.battle.net` CDN records. They refresh daily; unseen
products can trigger a refresh at most every ten minutes, including failures.

These community sources can lag releases or be unavailable. A depot whose owner
cannot be verified stays numeric and links to its depot page; there is no promise
that every new title resolves immediately. The last usable names survive outages.
Do not edit generated JSON while the helper is running. For a fully manual
lookup, stop the helper and use the format in [Monitoring](../../docs/monitoring.md#game-names).

## Checks

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -W error::ResourceWarning -m unittest discover -s contrib/game-names -v
node internal/dashboard/app_test.cjs internal/dashboard/static/app.js
```
