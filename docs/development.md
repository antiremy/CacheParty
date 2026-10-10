# Development

See the [README](../README.md#quick-start) for running CacheParty locally without
root.

## Tests and checks

```sh
gofmt -l .
go vet ./...
go test ./... -race
go test -run xxx -bench . ./internal/domains ./internal/accesslog ./internal/chunk ./internal/proxy ./internal/store
```

For end-to-end comparisons against monolithic, see [Benchmarks](benchmarks.md).

## Layout

```
cmd/cacheparty/     main
internal/config/    env parsing, size/duration parsing, dashboard overrides
internal/domains/   cache-domains fetch + matcher
internal/resolver/  upstream DNS dialer + TTL cache
internal/upstream/  HTTP client, redirects, retry
internal/chunk/     range math
internal/store/     ChunkStore + file/bbolt store + scrub
internal/evict/     background eviction
internal/proxy/     :80 handler, coalescing, bypass rules
internal/sni/       :443 passthrough
internal/ratelimit/ shared download rate limit for upstream connections
internal/accesslog/ cachelog / cachelog-json / stream log writers
internal/metrics/   Prometheus text renderer
internal/dashboard/ web dashboard, its API and the throughput sampler
bench/              load generator, fake origin and DNS, compose file
docs/               user and design documentation
```

[How it works](how-it-works.md) describes the design these packages implement.

## Releases

`.github/workflows/publish.yml` publishes the image on every push to `main`.
To release, either push a `v1.2.3` tag or run the workflow from the Actions
tab with a version such as `1.2.3`, which tags the chosen commit `v1.2.3`.
A release pushes the `1.2.3`, `1.2` and `1` image tags and creates a GitHub
release with `linux/amd64` and `linux/arm64` binaries and their checksums.
Versions with a suffix (`1.3.0-rc.1`) are marked as prereleases and get only
the full version tag.
