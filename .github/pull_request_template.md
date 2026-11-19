## What changes for the operator

<!-- One user-visible change. Link the issue for a new feature. -->

Closes #

## Why

## How it was tested

<!-- Dashboard changes: add before and after screenshots. -->

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...`
- [ ] `go mod tidy -diff`
- [ ] `golangci-lint run`
- [ ] `go test -race -count=1 ./...`
- [ ] `govulncheck ./...`

## Compatibility

<!-- Env vars, headers, access.log, STATS_FILE, cache keys. Write "None" if
nothing changes. Divergence from nginx lancache needs a line in
docs/compatibility.md. -->

## Checklist

- [ ] I have read every line of this change and can answer questions about it.
- [ ] It follows the rules in [AGENTS.md](https://github.com/antiremy/CacheParty/blob/main/AGENTS.md).
- [ ] Docs in `docs/` are updated to match.
