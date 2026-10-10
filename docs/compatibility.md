# Compatibility

CacheParty aims to be a drop-in replacement for lancachenet/monolithic: same
environment variables, same `access.log` format. This page lists where it
deliberately behaves differently, and what it does not do yet.

## Differences from nginx lancache

- **Domain matching is anchored to whole DNS labels.** nginx's generated regexes
  are unanchored (`.*?host`) and match more loosely; for example
  `evilsteamcontent.com` is not treated as steam here. Hosts the original
  matched by accident fall back to identifier = host.
- `*.example.com` does not match `example.com` itself; list both if needed.
  Entries with a `*` anywhere but a leading `*.` are skipped and reported.
- Cache hits carry only `Content-Type`, `Last-Modified`, `Content-Length`,
  `Content-Range` and `Accept-Ranges`; other upstream headers are not kept.
- Responses carry an extra `X-CacheParty-Version` header; all of monolithic's
  headers are sent unchanged alongside it.
- Truncated upstream bodies abort the client connection rather than ending the
  response cleanly, so a short download never looks complete.
- CacheParty cannot read nginx's on-disk cache, so switching starts from an empty
  cache.
- When ambiguous, behavior is chosen for cache correctness (never serve wrong
  bytes).

## Known limitations

- Responses that are not cacheable chunks (404, 5xx, `Content-Encoding`,
  upstream ignoring `Range`) are not shared between coalesced clients; each
  makes its own upstream request.
- cache-domains is loaded once at startup (no hot reload).
- `HEAD` requests are passed through uncached.
- DNS answers use a fixed 60 s TTL because `net.Resolver` hides record TTLs.
- Go's resolver still reads `/etc/hosts` before DNS; in a container that holds
  only localhost.
- The append-only segment store (v2) for fewer files and fewer HDD seeks is not
  started.
- `client_golang` is not used (see [Metrics](monitoring.md#metrics)).
- The `cachelog-json` and `stream-access.log` formats have not been verified
  against a real monolithic container (see [Logs](monitoring.md#logs)).
