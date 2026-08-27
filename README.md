# sandbox

Sandbox is an ephemeral web service that an agent runs on the person's
machine for the length of a working session. It exists because agents
produce things that a person needs to look at before they are committed,
and chat is a poor place to look at a poster, listen to a theme, or
compare two backgrounds. Sandbox gives the agent a way to put those
things in front of the person, in a browser, with a question attached,
and gives the person a way to answer with a click.

It does three things.

**Decisions.** When the agent has something to show, it uploads the files
together with a question about them, which the sandbox calls a decision.
The person opens the sandbox in a browser, sees the files and the
question, and answers with one of the buttons the decision offers. The
agent, meanwhile, is waiting on an HTTP request that returns as soon as
the answer is given, so it can act on the answer straight away.

For example, the agent has found three posters on TMDb for *The Good, the
Bad and the Ugly*, encoded each one to the size Plex needs, and wants to
know which should replace the poster the Kometa configuration currently
uses. It uploads the three candidates as options, uploads the poster that
is in use as an attachment so the person can see what they are replacing,
and asks "Which of these should be the poster?". The person clicks one and
presses **Use selected**; the agent then copies that file into the Kometa
configuration in the repository checkout, where it sits as an uncommitted
change until the person reviews and commits it. Nothing the agent does
through the sandbox changes the repository's history on its own.

**Upstreams.** Agents often need data from APIs that require a key, such
as TMDb for film metadata or Plex for what a library already contains.
Rather than giving the agent the key, the configuration tells the sandbox
where each API lives, which credential to attach to requests for it, and
which request methods and paths are allowed. The agent sends its requests
to the sandbox, which adds the credential and forwards the request if it
is on the list, or refuses it with a 403 if it is not. The key never
appears in the agent's context or its transcript.

**Nothing is kept.** Decisions and their files live in memory and on a
tmpfs inside the container. Stopping the container with `docker compose
down` deletes all of it, so a session leaves nothing behind that needs
cleaning up. Anything worth keeping, the agent copies into the repository
checkout once the person has approved it.

The contract for agents is [`llms.txt`](llms.txt), embedded in the image
and served at `/llms.txt`. Everything is plain HTTP; curl is the client.

## Decisions

Every decision has a type, and the type determines both what the page
shows and which answers are possible. There are five:

| Type | What the page shows | How the person answers |
| --- | --- | --- |
| `choose` | two or more files | clicks one (or several, if the decision allows it) and presses **Use selected**, or presses **None of these** |
| `approve` | one file | **Use it**, **Changes** or **No** |
| `compare` | the file already in the repository beside the proposed replacement | **Use new**, **Keep current** or **Changes** |
| `confirm` | a question | **Yes** or **No** |
| `ask` | a question | types an answer and presses **Send** |

Every decision also has a note box, so the person can say why, or what to
do differently. **Changes** and **None of these** are the answers that ask
the agent to try again; when it does, it posts a new decision with
`supersedes` set to the id of the one being revised, and the page shows
the new card as a revision of the old one.

Besides the files the person can pick, a decision can carry attachments,
which are shown for context but cannot be selected. The poster already in
the repository, shown beside three candidates, is an attachment.

Images, audio, video and text files (YAML, JSON, Markdown, plain text)
are displayed inline; images also show their width, height, aspect ratio
and file size, since those are usually part of what is being judged. Any
other file type appears as a download link.

The page is a queue. Unanswered decisions appear at the top and answered
ones below, each with the answer that was given. Clicking an image opens
it full screen; pressing `1` switches between fitting the screen and
showing actual pixels, the left and right arrow keys move between the
options of the same decision, and `Esc` closes it.

```
POST /decisions                multipart: spec JSON plus files; or bare JSON
GET  /decisions[/<id>]         decisions with their answers
POST /decisions/<id>/answer    from the page (or JSON)
GET  /decisions/<id>/files/<f> attached file
GET  /answers?since=&wait=     long-poll for new answers
GET  /events                   SSE; the page re-renders its queue on it
POST /reset
```

The page is a queue: pending decisions at the top, answered ones below.
Images open full-screen; `1` toggles actual pixels, arrow keys move
between an option's siblings, `Esc` closes.

## Server

| Variable | Default | Purpose |
| --- | --- | --- |
| `SANDBOX_ADDR` | `:8080` | listen address |
| `SANDBOX_DATA` | `/data` | where attached files go; a tmpfs in practice |
| `SANDBOX_MAX_UPLOAD` | `52428800` | per-file byte limit |
| `SANDBOX_PUBLIC_URL` | `http://localhost:8080` | URL shown to people and in `llms.txt` |
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

    SANDBOX_DATA=/tmp/sandbox go run ./cmd/sandboxd
    curl -s -X POST -H 'Content-Type: application/json' localhost:8080/decisions \
      -d '{"title":"Does this work?","type":"confirm"}'

The image is `gcr.io/distroless/static:nonroot`; mount `/data` as a tmpfs
owned by uid 65532 and run with a read-only root filesystem.

## Use cases

**Kometa assets and metadata.** The agent looks a title up through
`/tmdb`, encodes candidate posters, and posts a `choose` with the current
poster attached; a replacement background is a `compare`; a theme is an
`approve`; the YAML entry is a `confirm` with the file attached. The
person picks in the browser, the agent commits what was approved. This is
what `compose.example.yml` is configured for.

**Any question with a closed answer.** `confirm` and `ask` need no files:
"delete these three?", "which id is it?". Cheaper than a chat round-trip
when several are pending at once.

**Any API with a key.** Register the upstream, inject the credential,
allow the read paths, and the agent can query it without the key
appearing in its context or the transcript.

## Development

    go generate ./...        # fetch the pinned htmx assets (once)
    go test -race ./...
    docker build -t sandbox:dev .

No dependencies beyond the standard library. Images:
`ghcr.io/santiagosayshey/sandbox:vX.Y.Z` from release tags, `:develop`
from every push to `develop`. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the branch, commit and release flow.
