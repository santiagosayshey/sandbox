# Contributing

## Branches and pull requests

- Never commit to `develop`; branch from an up-to-date `develop` as
  `<type>/<short-description>`, for example `fix/sse-reconnect`.
- Open a pull request. `develop` is squash-merge only and the PR title becomes
  the commit message, so the title must be a Conventional Commit; the
  `pr-title` check enforces this.
- CI must pass: gofmt, `go vet`, staticcheck, `go test -race`, govulncheck
  and a Docker build.

## Commits

Conventional Commits: `<type>(<scope>): <description>`.

Types: `feat`, `fix`, `chore`, `docs`, `ci`, `refactor`, `test`.
Scopes: `server`, `web`, `proxy`, `store`, `deps`, or none.

    feat(server): add video previews
    fix(proxy): drop client Authorization headers before forwarding
    chore(deps): update all non-major dependencies

## Releases

Versions come from the commits. From an up-to-date `develop`:

    git cliff --bumped-version          # see what the next version will be
    git cliff --bump -o CHANGELOG.md    # regenerate the changelog with it
    git commit -am "chore(release): vX.Y.Z"
    git tag vX.Y.Z && git push && git push --tags

The `release` workflow builds and pushes `ghcr.io/santiagosayshey/sandbox:vX.Y.Z`
(and `:latest`), scans it with Trivy, and publishes a GitHub release with
that version's changelog section. Release commits go straight to `develop`;
they are the one exception to the branch rule.

## Vendored assets

`internal/web/htmx.min.js` and `internal/web/sse.js` are not tracked.
`go generate ./...` downloads the versions pinned in
`tools/fetchassets/main.go`; Renovate bumps those pins like any other
dependency.
