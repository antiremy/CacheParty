# Configuration

CacheParty is configured with environment variables. They use the same names and
defaults as lancachenet/monolithic, plus a few additions (marked "added").

| Var                    | Default                                       | Meaning                                                                                                                 |
| ---------------------- | --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `CACHE_DISK_SIZE`      | `1000g`                                       | Max total cache size                                                                                                    |
| `MIN_FREE_DISK`        | `10g`                                         | Evict to keep at least this much free on the volume                                                                     |
| `CACHE_MAX_AGE`        | `3560d`                                       | Evict chunks not accessed within this time                                                                              |
| `CACHE_SLICE_SIZE`     | `1m`                                          | Chunk size                                                                                                              |
| `CACHE_MAX_PREFETCHES` | `64`                                          | Max chunk fetches running ahead of clients, across all clients; `0` disables [read-ahead](how-it-works.md#read-ahead) (added) |
| `UPSTREAM_RATE_LIMIT`  | `0`                                           | Combined download cap for misses, bypass and `:443` passthrough, in bits/s (`500m`, `1g`); `0` is unlimited (added)     |
| `BYPASS_DOMAINS`       | (empty)                                       | Comma-separated domains that skip the cache, e.g. `example.com, *.cdn.example.net`; see [Bypass rules](how-it-works.md#bypass-rules) (added) |
| `UPSTREAM_DNS`         | `8.8.8.8 8.8.4.4`                             | Resolvers for upstream lookups (the system resolver is never used, because LAN DNS points CDN names back at this cache) |
| `CACHE_DOMAINS_REPO`   | `https://github.com/uklans/cache-domains.git` | Domain list source                                                                                                      |
| `CACHE_DOMAINS_BRANCH` | `master`                                      | Branch of the domain list                                                                                               |
| `NOFETCH`              | `false`                                       | Skip updating cache-domains at startup                                                                                  |
| `NGINX_LOG_FORMAT`     | `cachelog`                                    | `cachelog` or `cachelog-json`                                                                                           |
| `CACHE_ROOT`           | `/data/cache`                                 | Chunk files, `index.db`, `tmp/`                                                                                         |
| `LOG_DIR`              | `/data/logs`                                  | Log directory                                                                                                           |
| `DOMAINS_DIR`          | `/data/cachedomains`                          | Where cache-domains is kept (added)                                                                                     |
| `HTTP_ADDR`            | `:80`                                         | Caching proxy listener (added)                                                                                          |
| `SNI_ADDR`             | `:443`                                        | SNI passthrough listener; `off` disables (added)                                                                        |
| `METRICS_ADDR`         | `:8080`                                       | Metrics, health and dashboard listener; `off` disables (added)                                                          |
| `SETTINGS_FILE`        | `/data/settings.json`                         | Where settings changed from the dashboard are saved (added)                                                             |
| `STATS_FILE`           | `/data/stats.json`                            | Where the dashboard's traffic totals are kept across restarts; `off` keeps them in memory only (added)                  |
| `DNS_SYNC_FILE`        | `/data/dns-sync.json`                         | DNS servers to sync cache-domains to, with their passwords; `off` disables [DNS sync](monitoring.md#dns-sync) (added)   |
| `DASHBOARD_PASSWORD`   | (empty)                                       | Password for settings changes, `verify` and `purge`; empty makes them read-only (added)                                 |

## Live settings

`CACHE_DISK_SIZE`, `MIN_FREE_DISK`, `CACHE_MAX_AGE`, `CACHE_MAX_PREFETCHES`,
`UPSTREAM_RATE_LIMIT` and `BYPASS_DOMAINS` can be edited in the
[dashboard](monitoring.md#dashboard) while CacheParty runs (this needs
`DASHBOARD_PASSWORD`).

- The environment variable is the starting value.
- A change takes effect immediately (eviction at its next sweep, within a
  minute), is saved to `SETTINGS_FILE`, and overrides the environment variable
  from then on, including after a restart.
- **Reset** removes the override and falls back to the environment.
- Saving an empty `BYPASS_DOMAINS` from the dashboard bypasses nothing, even if
  the variable is set.

Everything else is shown read-only and still needs the environment and a
restart.

## Memory

Each chunk fetch in progress holds one `CACHE_SLICE_SIZE` buffer, taken from a
pool and returned when the fetch and every client reading it are done, so
`CACHE_MAX_PREFETCHES` and the number of concurrent misses bound memory use.

If the container has a memory limit, set `GOMEMLIMIT` a little below it (for
example `GOMEMLIMIT=900MiB` for a 1 GiB limit) so the Go runtime collects
garbage before the limit is reached rather than being killed.

The chunk index is also held in memory, at 75–110 bytes per chunk (see
[Storage and eviction](how-it-works.md#storage-and-eviction)).

## Log rotation

`SIGHUP` flushes and reopens `access.log`, `error.log` and `stream-access.log`.
