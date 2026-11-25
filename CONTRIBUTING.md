# Contributing to CacheParty

Thanks for helping. CacheParty is a drop-in replacement for
[lancachenet/monolithic](https://github.com/lancachenet/monolithic), so most
of these rules exist to keep it a safe swap for people who already run
lancache.

## Bugs and ideas

- **Bugs:** open an issue with your CacheParty version or image tag, how you
  run it, what you expected and what happened. Lines from `access.log` or the
  container log help a lot.
- **New features:** open an issue first so we can agree on the design before
  you write the code.
- **Security problems:** don't open a public issue. Use
  [private vulnerability reporting](https://github.com/antiremy/CacheParty/security/advisories/new)
  instead.

## Getting set up

You need the Go version named in [`go.mod`](go.mod). The
[README](README.md#quick-start) shows how to run CacheParty locally without
root, and [Development](docs/development.md) covers the package layout.

Read these before you change the matching area:

- [How it works](docs/how-it-works.md): before you touch request handling,
  cache keys, coalescing, read-ahead or storage.
- [Compatibility](docs/compatibility.md): before you change anything a
  lancache user would notice.

## Ground rules

- **One binary, one image.** Every feature ships as Go code inside the
  binary. No helper scripts, sidecars or host tools.
- **Don't break the drop-in.** Environment variable names, the
  `X-LanCache-Processed-By` and `X-Upstream-*` headers, `/lancache-heartbeat`
  and the `access.log` formats stay as they are. Add new behavior beside them.
  Any deliberate difference from nginx lancache gets a line in
  [Compatibility](docs/compatibility.md).
- **Correct bytes beat fast bytes.** Never serve wrong or mixed data. A change
  to anything that feeds the chunk key is a migration, so call it out.
- **Old files still load.** New fields in the stats file are additive.
- **Outside data fails safe.** Anything fetched from the internet keeps its
  last good copy when a fetch fails.
- **The server owns the numbers.** The dashboard shows what `/api/*` returns.
  It formats and sorts; it doesn't compute totals.

## Checks

CI runs these on every pull request. Run all of them across `./...` before
you open one:

```sh
gofmt -l .                # must print nothing
go vet ./...
go mod tidy -diff
golangci-lint run
go test -race -count=1 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

- New code comes with Go tests. Changes to `/api/*` routes get a test in
  `internal/dashboard`.
- Fix lint findings instead of adding `//nolint`. Write a deliberately
  ignored error as `_ = f()`.

## Dashboard changes

The dashboard in `internal/dashboard/static/` is plain HTML, CSS and JS with
no build step. It is embedded in the binary, so restart
`go run ./cmd/cacheparty` to see edits. New areas go in their own module under
`static/js/`, not in `app.js`.

## Docs

If you change behavior, update the matching page in [`docs/`](docs) in the
same pull request.

## Commits

- Write the subject as a plain imperative sentence about what the operator
  will see: "Show HTTPS passthrough traffic by domain on the dashboard". No
  `feat:` or `fix:` prefixes.
- Write the body in prose: what changed, why, and any compatibility notes.
- Commit as yourself. If a tool or AI helped, don't add `Co-Authored-By` or
  "Generated with" lines to the commit.

## Pull requests

- Keep each pull request to one user-visible change.
- In the description, say what changes for the operator, why, and how you
  tested it. List the checks you ran.
- For dashboard changes, add before and after screenshots.
- Read every line you submit, and be ready to answer review questions in your
  own words. A one-line credit to a tool at the end of the description is
  fine.

## License

By contributing, you agree that your work is released under the
[MIT License](LICENSE).
