# Cache repair

nginx lancache has no index, so finding a bad chunk or one game's chunks
meant `grep`ping the whole cache. CacheParty keeps one, so neither needs a walk
over chunk contents.

Both commands ask the running CacheParty to do the work through the dashboard
API, because the index lives in that process: they need `METRICS_ADDR` on
and `DASHBOARD_PASSWORD` set, and read both from the environment, as
`docker exec` provides (`-addr host:port` points them elsewhere).

## Checking for bad chunks

Bad chunks mostly repair themselves as they are requested (see
[Storage and eviction](how-it-works.md#storage-and-eviction)), and a slow
scrub checks the whole cache daily. To check everything now, from inside the
container:

```sh
docker exec <container> /cacheparty verify --dry-run   # report only
docker exec <container> /cacheparty verify             # repair
```

It reports how many chunk files it checked, orphans (files the index does
not know), missing files (indexed, not on disk) and corrupt files (wrong
size), and how much space was (or would be) freed; `error.log` names each
missing and corrupt chunk (up to 100 a run). Only sizes are checked: chunk
contents are not re-hashed, so right-sized garbage is not found.

The API is `POST /api/cache/verify` with `{"dryRun": true|false}` and the
password as a bearer token; it answers when the check is done, and only one
check runs at a time (`409` otherwise).

## Purging a game

To remove one game from the cache, name it as the dashboard lists it,
`<service>/<kind>/<id>`:

```sh
docker exec -it <container> /cacheparty purge steam/depot/990081
docker exec <container> /cacheparty purge -yes epicgames/app/Fortnite
```

It shows how many chunks and how much space will be freed, and asks before
removing anything (`-yes` skips the question; without a terminal the answer
is no). On the dashboard, with `DASHBOARD_PASSWORD` set, each game in a
service's breakdown has a **Purge** link that shows the same count and asks
for confirmation. The API is `POST /api/cache/purge` with
`{"service", "kind", "id", "confirm"}`; without `"confirm": true` it only
reports what would be removed.

### How chunks are found

Cache keys are hashes, so the index cannot be searched by URL. Instead each
object's metadata records the game it belongs to, worked out from the URL as
for the dashboard's per-game breakdown (Steam depot, Blizzard product, Epic
build name, otherwise the CDN host), and its chunks follow from its size.
Purging walks the objects in memory and needs no directory walk.

### Limits

- Steam is broken down by **depot**, not by app: a game with several depots
  (language packs, DLC) is purged one depot at a time. SteamDB lists a
  game's depots.
- Objects cached before this was recorded carry no game until they are
  requested again, so a game nobody has downloaded since upgrading is not
  found. `cacheparty verify` does not change this.
- Chunks are removed the way eviction removes them: a client reading one
  keeps its open file, and the next chunk it needs comes from the internet.
  A chunk being fetched at that moment can survive the purge. Object
  metadata goes at the next sweeps, as for any emptied object.
- Chunks stored under an earlier `CACHE_SLICE_SIZE` are not found; they age
  out as usual.
- The dashboard only lists games downloaded since `STATS_FILE` was first
  written (or deleted); the command works for any game.
