# AGENTS.md

CacheParty: a single-binary Go drop-in replacement for lancachenet/monolithic
(a LAN game-download cache). Start with the [README](README.md); its
Documentation section indexes `docs/`.

- Package layout and test commands: [docs/development.md](docs/development.md).
- Before changing request handling, cache keys, coalescing, read-ahead or
  storage: read [docs/how-it-works.md](docs/how-it-works.md).
- Before changing anything a lancache user would notice: read
  [docs/compatibility.md](docs/compatibility.md).

## Checks

CI (`.github/workflows/ci.yml`) is the bar: `gofmt -l .` must print nothing,
then `go vet ./...` and `go test -count=1 ./...`. Run `go test -race` on
`internal/proxy` too when touching concurrency; some proxy tests branch on
`raceEnabled`.

## Invariants

- **Compatibility surface is frozen.** Environment variable names, the
  `X-LanCache-Processed-By` and `X-Upstream-*` response headers, the
  `/lancache-heartbeat` endpoint and the `access.log` formats match monolithic
  and stay unchanged, even where names look dated. Loop detection with nginx
  caches and outside tools depend on them. Add new behavior alongside them,
  never in their place. Any deliberate divergence from nginx lancache gets a
  line in `docs/compatibility.md`.
- **Cache correctness beats speed.** Never serve wrong or mixed bytes; a
  truncated upstream body aborts the client. Anything feeding the chunk key
  (identifier, path, slice size, chunk index) changes which chunks existing
  caches can read, so treat it as a migration.
- **Persisted files load across versions.** New fields in `STATS_FILE` are
  additive; older files still load.
- **Published image names are public.** `publish.yml` pushes
  `ghcr.io/<owner>/cacheparty` and `docker.io/<DOCKERHUB_USERNAME>/cacheparty`;
  users pull those names and tags (`latest`, semver), so don't rename them.

## Dashboard

`internal/dashboard/static/` is plain HTML/CSS/JS with no build step, embedded
with `go:embed`, so restart `go run ./cmd/cacheparty` to see edits. Its `/api/*`
routes are registered in `dashboard.go`; changes to them get a test in the
package's `*_test.go` files.

## Docs and commits

- Behavior changes update the matching page in `docs/` in the same change.
- Commit subjects are plain imperative sentences describing the user-visible
  change ("Show HTTPS passthrough traffic by domain on the dashboard"), no type
  prefixes. Bodies are prose: what changed for the operator and why, plus any
  compatibility notes.
