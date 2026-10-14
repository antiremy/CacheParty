# How it works

## Listeners

- **`:80`** (`HTTP_ADDR`) caching HTTP proxy.
- **`:443`** (`SNI_ADDR`) SNI passthrough: reads the TLS ClientHello (parsed by
  `crypto/tls`), resolves the name through `UPSTREAM_DNS` (A and AAAA), and
  relays bytes in both directions. TLS is never terminated. Missing, IP-literal
  or malformed SNI is closed (logged `400`); a name that resolves to this
  machine is refused (`403`) to avoid loops. Connections go to
  `stream-access.log`.
- **`:8080`** (`METRICS_ADDR`) the [dashboard](monitoring.md#dashboard) at
  `/`, Prometheus `/metrics` and `/healthz`.

## Cache identifier

`cache_domains.json` is loaded once at startup (fetched as a GitHub tarball, or
with the `git` CLI for other hosts, since the distroless image has no `git`).
A `Valve/Steam HTTP Client 1.0` User-Agent is always `steam`; otherwise the Host
is matched against the lists (exact first, then the most specific `*.` wildcard)
and the entry's `name` is the identifier, falling back to the Host itself.
Having no domains at startup is fatal, because identifiers feed cache keys and
loading a real list later would silently split the cache.

## Cache key and chunking

- Requests are split into `CACHE_SLICE_SIZE` chunks, each fetched with its own
  `Range`. Per-object metadata (total size, `Content-Type`, `Last-Modified`) is
  learned from the first `Content-Range`.
- Chunk key: SHA-256 (first 16 bytes) of identifier, path, slice size and chunk
  index. The path excludes the query string (so signed CDN URLs share an entry)
  and is not URL-decoded. Changing `CACHE_SLICE_SIZE` starts a fresh set of
  chunks rather than reading anything at the wrong offset.
- Clients get `200` with no `Range`, and `206` with a correct `Content-Range`
  otherwise (open-ended, suffix and multi-chunk ranges work). Multi-range,
  malformed, `If-Range` and unknown-unit requests get the whole object as `200`.
- If upstream ignores `Range` and returns `200`, the body is streamed to the
  client while being cut into chunks, stopping after the last chunk needed.
- Only `200`/`206` are cached. Redirects (up to 5 hops) are followed
  internally and never cached; the original cache key is kept.
  `Expires`/`Cache-Control` are ignored, and `ETag` is stripped from every
  response.
- If an object changes upstream (total size or `Last-Modified` differs), all its
  chunks are dropped so old and new bytes are never mixed.

## Coalescing

One in-flight object per chunk key. The first client to miss starts the fetch
in its own goroutine, so any client (including the first) can disconnect and
the chunk is still committed. Joined clients read from a chunk-sized buffer as
upstream bytes arrive. The last byte is held until the commit lands, so the next
request is a hit. A test runs 50 concurrent clients on a 64 MB uncached file and
asserts one upstream request per chunk and byte-identical bodies.

## Read-ahead

Once a response has had to fetch a chunk from upstream, it also fetches the
chunks the client will need next. The window starts at 2 chunks and doubles
whenever the client reaches a chunk that is still arriving, up to 32 MiB, so a
single client on a high-latency origin is not held to a few chunks per round
trip. `CACHE_MAX_PREFETCHES` caps these fetches across all clients, and each
download's window is also capped at an even share of them (with 64 slots and 4
downloads reading ahead, 16 chunks each), so one download cannot take the slots
other clients need.

Some clients read a file forward one short range at a time instead: Battle.net
walks its archives this way, skipping the parts it does not need, with a single
request in flight. For those, CacheParty remembers where each client's last range
on each file ended. A range that starts at or after that point, at most 32 MiB
on, continues a forward scan: its response reads ahead past its own end (even
when it is a hit) into the ranges the client will ask for next, and its window
carries over to the next request. Ranges that go backwards or jump further
start over. Scans idle for 30 seconds are forgotten, and at most 4096 are
tracked. Whole-file downloads (Steam, Epic) are unaffected. A scan can fetch
the parts of the file the client skips, and up to one window past its last
range.

Each fetch in progress holds one `CACHE_SLICE_SIZE` buffer; see
[Memory](configuration.md#memory) for sizing a container's memory limit.

## Bypass rules

Proxied straight through uncached (`X-Upstream-Cache-Status: BYPASS`) when the
query contains `nocache=1`, the path starts with `/latest64`, ends with
`authrootstl.cab`, `pinrulesstl.cab` or `disallowedcertstl.cab`
(case-insensitive), matches `releaselisting_` or ends in `.version`, or is
exactly `/server-status`. Non-`GET` requests are always passed through.

Requests whose `Host` matches `BYPASS_DOMAINS` are bypassed too: nothing is read
from or written to the cache, no index entries are created, and ranges go to the
origin as sent. They are reported as `BYPASS` like the rules above. Matching
ignores case, any port and a trailing dot. `example.com` covers `example.com`
and every subdomain (`cdn.example.com`, never `notexample.com`);
`*.example.com` covers subdomains only. Entries are trimmed, empty ones are
skipped, and invalid ones (ports, paths, spaces) are logged and ignored.
`BYPASS_DOMAINS` can be edited live in the dashboard (see
[Live settings](configuration.md#live-settings)).

## Upstream fetching

- Hosts resolve through a `net.Resolver` dialing only `UPSTREAM_DNS`
  (round-robin, A and AAAA, IPv4 tried first, names queried with a trailing dot
  so search domains are never appended, 60 s TTL cache, stale answer if a
  refresh fails).
- Keep-alive pooling per host; `HTTP_PROXY`/`HTTPS_PROXY` are ignored.
- A `GET` that fails after connecting (reset, header timeout) or returns `404`
  is retried once on a different IP of the host.
- A fetch with no bytes for 2 minutes is cancelled.
- Sent upstream: `Host`, `X-Real-IP`, `X-Forwarded-For` (appended),
  `X-LanCache-Processed-By`; `If-*` is dropped and `Accept-Encoding: identity`
  is set. If upstream still sends `Content-Encoding`, the request is passed
  through uncached.
- `UPSTREAM_RATE_LIMIT` caps the combined download rate of misses, bypassed
  requests and `:443` passthrough.

## Headers and endpoints

- Loop detection: an incoming `X-LanCache-Processed-By` equal to this hostname
  returns `508`; otherwise the response carries `<hostname>[,<incoming>]`.
- `GET /lancache-heartbeat` returns `204` with the CORS and processed-by headers
  (and is not written to `access.log`).
- A request with no `Host`, or whose `Host` is an IP address (someone opening
  `http://<server-ip>/` in a browser), is never proxied: `GET /` returns a
  small landing page linking to the dashboard on `METRICS_ADDR`'s port, and
  other paths return `404`. Neither is logged or counted in metrics.
- Every response has `X-Upstream-Cache-Status` (`HIT`, `MISS`, `BYPASS`),
  `X-Upstream-Status`, `X-Upstream-Response-Time`, and
  `X-Clacks-Overhead: GNU Terry Pratchett, GNU Zoey -Crabbey- Lough` (kept as a
  memorial, as in the original).
- Every response also has `X-CacheParty-Version` with the running build's
  version. It is CacheParty's own addition; the lancache headers above are
  unchanged.
- Joined (coalesced) clients report `HIT`, like nginx lock waiters, since they
  cost no upstream traffic. The header reflects the first chunk; the log says
  `MISS` if any chunk of the response was fetched.

## Storage and eviction

- `ChunkStore` interface (`Get`, `Commit`, `Delete`, `Stat`, `Iterate`) with a v1
  file store: one file per chunk at `<root>/<h[30:32]>/<h[28:30]>/<h>`, written
  to `tmp/` and atomically renamed. A bbolt `index.db` holds per-chunk size and
  last access, and per-object metadata.
- The whole index is held in memory (75–110 bytes per chunk, depending on how
  full the hash tables are) and loaded at startup with no directory walk; hits
  never touch bbolt. Index entries hold no pointers, so the garbage collector
  never scans them however large the cache grows. Index writes are
  batched every 10 s and on clean shutdown, so `kill -9` can lose up to one
  interval of updates. That leaves an unindexed file or an old access time,
  never wrong bytes.
- There is no `fsync` on commit (nginx has none either). `Get` treats a file
  whose size differs from the index as a miss. It cannot detect right-sized
  garbage.
- A cached chunk that cannot be served is repaired on the spot: if its file is
  missing, cannot be opened (an I/O error, say), has the wrong size, or comes
  up short or fails while it is being sent, the chunk is removed from the
  index and the disk, a warning naming it goes to `error.log`, and the
  request carries on from a fresh upstream fetch, even partway through a
  response, so the client does not see the failure. Running out of file
  descriptors does not count as a bad file. Each repair is counted in
  `cacheparty_dropped_chunks_total`.
- A cached chunk is served without contacting upstream, so an upstream error
  cannot affect a hit; stale-on-error holds by construction. There is no
  revalidation.
- A sweep runs every minute: chunks idle longer than `CACHE_MAX_AGE`, then
  least-recently-used chunks until the cache is under `CACHE_DISK_SIZE` (undershot
  by 5%) and the volume has `MIN_FREE_DISK` free (`statfs`), then object metadata
  with no chunks. Eviction is per chunk, so a partly evicted download refetches
  only the missing slices.
- A sweep only walks the index when something can be due: while the oldest
  access time seen on the last walk is within `CACHE_MAX_AGE` and the cache is
  under its limits, it does nothing (a full walk still runs hourly). When space
  must be freed it keeps only the least recently used chunks that cover the
  shortfall, not a record for every cached chunk.
- A slow scrub (5 minutes after start, then daily) removes unindexed chunk
  files older than 1 hour, forgets indexed chunks whose file is gone, and
  removes chunks whose file is not the size the index says. It lists
  directories and stats files but never reads chunk contents. `cacheparty verify`
  runs the same check on demand (see [Cache repair](cache-repair.md)).
