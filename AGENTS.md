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
then `go vet ./...`, `go mod tidy -diff`, `golangci-lint run` (rules in
`.golangci.yml`), `go test -race -count=1 ./...` and govulncheck. Some proxy
tests branch on `raceEnabled`. Fix lint findings rather than adding
`//nolint`; a deliberately ignored error is written `_ = f()`.

## Invariants

- **One Go binary, one distroless image.** Every feature ships as Go code
  inside the binary and runs on every platform the release builds. Operators
  run nothing beside it: no helper scripts, sidecars, second containers or
  host tools like `tail` or `cron`. CI checks Go only, so new code comes with
  Go tests.
- **The server owns the numbers.** Totals, grouping and anything persisted are
  computed in Go and served by `/api/*`. The dashboard formats and sorts what
  the API returns, so every open tab shows the same figures.
- **Outside sources fail safe.** Data fetched from the internet (domain lists,
  name maps, metadata) keeps its last good copy when a fetch errors, times out
  or returns a 404. The failure is logged and retried later; only a good
  response replaces known-good data.
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
- The author and committer are the human accountable for the change, never a
  tool, bot or AI agent identity. Use the git identity already configured for
  the session; don't copy a name from this file.
- No AI attribution in commits: no `Co-Authored-By` trailers, "Generated
  with" lines or similar credits for an agent, assistant or tool. This holds
  even when an agent's own defaults say to add them.

## Pull requests

- One user-visible change per pull request. For a new feature, open an issue
  first so the design is agreed before the code is written.
- Before opening, run every command in Checks across `./...` and list them in
  the description. A subset of packages or commands is not a pass.
- Fill in every section of `.github/pull_request_template.md`; with
  `gh pr create`, pass it as the body. The description is for the reviewer.
  Keep agent self-reviews, scores and review transcripts out; a
  one-line credit to the tool at the end is fine.
- The human opening the pull request has read every line and answers review
  questions in their own words.
