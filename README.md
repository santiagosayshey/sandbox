# sandbox

An ephemeral scratch space for agent-driven homelab work. A small Go
service holds files in a RAM disk, shows them live in a browser as they
arrive, and proxies requests to services like TMDb and Plex with the
credentials injected server-side. A reset or restart empties it.

It exists so an agent can build Kometa assets and metadata, a person can
eyeball them at `http://localhost:8080`, and the approved files get copied
into the [orion](http://forgejo.orion/santiagosayshey/orion) repository.

The usage contract for agents is [`llms.txt`](llms.txt), embedded in the
image and served at `/llms.txt`. Everything is plain HTTP; curl is the
client.

## Server

| Variable | Default | Purpose |
| --- | --- | --- |
| `SANDBOX_ADDR` | `:8080` | listen address |
| `SANDBOX_DATA` | `/data` | directory to serve; a tmpfs in production |
| `SANDBOX_TOKEN` | required | bearer for writes and upstream routes |
| `SANDBOX_MAX_UPLOAD` | `52428800` | per-file byte limit |
| `SANDBOX_PUBLIC_URL` | unset | URL shown to people and in `llms.txt` |
| `SANDBOX_UPSTREAM_<NAME>` | unset | registers `/<name>/*` forwarding to this URL |
| `SANDBOX_HEADER_<NAME>` | unset | `Header: value` injected on every request to that upstream |
| `SANDBOX_QUERY_<NAME>` | unset | `key=value` injected into the query string |
| `SANDBOX_ALLOW_<NAME>` | unset | `METHOD /path` lines; only matching requests are forwarded, the rest get 403. No rules means nothing is allowed |

A path ending in `/*` matches itself and everything beneath it; any other
path must match exactly. `*` as the method allows any method.

[`compose.example.yml`](compose.example.yml) and
[`.env.example`](.env.example) are a complete configuration with TMDb and
Plex upstreams. Copy them to `compose.yml` and `.env`, fill in the values,
and `docker compose up -d`.

Run it locally:

    SANDBOX_TOKEN=dev SANDBOX_DATA=/tmp/sandbox go run ./cmd/sandboxd
    curl -X PUT -H 'Authorization: Bearer dev' --data-binary @README.md localhost:8080/files/docs/README.md

The image is `gcr.io/distroless/static:nonroot`; mount `/data` as a tmpfs
owned by uid 65532 and run with a read-only root filesystem.

## Development

    go generate ./...        # fetch the pinned htmx assets (once)
    go test -race ./...
    docker build -t sandbox:dev .

Images: `ghcr.io/santiagosayshey/sandbox:vX.Y.Z` from release tags,
`:develop` from every push to `develop`. See
[CONTRIBUTING.md](CONTRIBUTING.md) for the branch, commit and release flow.
