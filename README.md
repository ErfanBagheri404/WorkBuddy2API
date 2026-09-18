<div align="center">

# WorkBuddy2API

**OpenAI-compatible proxy for WorkBuddy AI — run WorkBuddy's models through any OpenAI client, without the desktop app open.**

[![CI](https://github.com/ErfanBagheri404/WorkBuddy2API/actions/workflows/ci.yml/badge.svg)](https://github.com/ErfanBagheri404/WorkBuddy2API/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ErfanBagheri404/WorkBuddy2API?color=blue)](https://github.com/ErfanBagheri404/WorkBuddy2API/releases/latest)
[![Go](https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

</div>

---

WorkBuddy2API turns [WorkBuddy AI](https://www.workbuddy.ai) (Tencent's CodeBuddy
desktop product) into a plain OpenAI endpoint. Log in once through your browser,
and from then on any tool that speaks the OpenAI API — SDKs, chat UIs, coding
agents, routers — can talk to WorkBuddy's models at `http://localhost:61021/v1`.

The desktop app does **not** need to be running.

## Features

- **One-time browser login** — OAuth device flow; no password ever touches this tool.
- **Runs headless** — tokens are stored on disk and refreshed automatically, so the proxy survives without WorkBuddy open.
- **Live model list** — `/v1/models` is fetched from WorkBuddy, never hardcoded.
- **Streaming and non-streaming** — SSE passthrough when you ask for it; the proxy assembles a normal JSON response when you don't.
- **Zero dependencies** — pure Go standard library, single static binary.
- **Interactive CLI** — a menu for status, models, test chat and the server, plus a `--headless` flag for scripts.

## Quick start

1. Download `WorkBuddy2API.exe` from the [latest release](https://github.com/ErfanBagheri404/WorkBuddy2API/releases/latest).
2. Run it. On first launch it opens your browser to sign in to WorkBuddy.
3. Pick **Start server** from the menu.

Your endpoint is now live:

```
http://localhost:61021/v1
```

## Usage

```bash
# interactive menu
WorkBuddy2API.exe

# server only (requires a prior login)
WorkBuddy2API.exe --headless

# protect /v1 endpoints with a bearer token
WorkBuddy2API.exe --headless --api-key=YOUR_SECRET

# enable per-IP rate limiting (2-second minimum interval with ±20% jitter)
WorkBuddy2API.exe --headless --rate-limit=2s

# rewrite system-prompt keywords that trigger content moderation
WorkBuddy2API.exe --headless --desensitize

# combine all three
WorkBuddy2API.exe --headless --api-key=YOUR_SECRET --rate-limit=2s --desensitize
```

| Flag                | Env variable            | Default | Description                                           |
| ------------------- | ----------------------- | ------- | ----------------------------------------------------- |
| `--api-key=...`     | `WORKBUDDY2API_KEY`     | *none*  | Require `Authorization: Bearer <key>` on `/v1` routes |
| `--rate-limit=...`  | `WORKBUDDY2API_RATE_LIMIT` | *off* | Min interval per IP (`2s`, `500`, `500ms`)          |
| `--desensitize`     | `WORKBUDDY2API_DESENSITIZE` | *off* | Rewrite moderation-triggering system-prompt keywords  |

Menu options:

| # | Option          | What it does                                |
| - | --------------- | ------------------------------------------- |
| 1 | Status          | Auth state + live model list                |
| 2 | Start server    | OpenAI proxy on `:61021`                    |
| 3 | List models     | Fetch models from WorkBuddy                 |
| 4 | Test chat       | Quick chat without starting the server      |
| 5 | Re-login        | Get fresh credentials                       |
| 6 | Quit            | Exit                                        |

## API

| Method | Path                   | Description                            |
| ------ | ---------------------- | -------------------------------------- |
| `GET`  | `/healthz`             | Liveness check                         |
| `GET`  | `/v1/models`           | Live model list from WorkBuddy         |
| `POST` | `/v1/chat/completions` | OpenAI-compatible chat, streaming or not |

### Chat

```bash
curl http://localhost:61021/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "default-model",
    "stream": false,
    "messages": [{"role": "user", "content": "hi"}]
  }'
```

### Streaming

```bash
curl -N http://localhost:61021/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "fast-model",
    "stream": true,
    "messages": [{"role": "user", "content": "count to five"}]
  }'
```

### Authentication

When `--api-key` (or `WORKBUDDY2API_KEY`) is set, `/v1/models` and
`/v1/chat/completions` require a credential. `/healthz` stays open so liveness
probes keep working.

```bash
curl http://localhost:61021/v1/models \
  -H "Authorization: Bearer YOUR_SECRET"
```

`x-api-key: YOUR_SECRET` is accepted as an alternative. The scheme name is
case-insensitive (`bearer`, `Bearer`, `BEARER` all work), and comparison is
constant-time.

With no key configured, authentication is disabled entirely and every request
is served — the original localhost-only behaviour.

### OpenAI SDK

```python
from openai import OpenAI

# no --api-key configured: any placeholder works
client = OpenAI(base_url="http://localhost:61021/v1", api_key="unused")

# --api-key=YOUR_SECRET configured: the SDK's api_key is sent as the bearer token
client = OpenAI(base_url="http://localhost:61021/v1", api_key="YOUR_SECRET")

resp = client.chat.completions.create(
    model="default-model",
    messages=[{"role": "user", "content": "hello"}],
)
print(resp.choices[0].message.content)
```

> WorkBuddy's own authentication comes from the tokens saved during login, not
> from the SDK `api_key`. When you pass `--api-key`, that value doubles as the
> proxy's bearer token.

## How it works

```
OpenAI client ──► localhost:61021/v1 ──► WorkBuddy /v2/chat/completions ──► models
                        │
                        └── tokens from ~/.workbuddy2api-auth.json (auto-refreshed)
```

1. **Login** — the CLI asks WorkBuddy for a login URL, opens it in your browser, and polls until you have signed in.
2. **Storage** — the resulting access and refresh tokens are written to `~/.workbuddy2api-auth.json` with `0600` permissions.
3. **Refresh** — the access token is refreshed automatically before it expires, so the proxy keeps working across restarts and with the desktop app closed.
4. **Translation** — OpenAI-shaped requests are forwarded to WorkBuddy's chat endpoint; SSE chunks stream straight back, and non-streaming requests are assembled from the stream.

## Notes on models

`/v1/models` returns whatever WorkBuddy currently advertises, plus a small set of
direct model names that the chat endpoint accepts but the listing omits. The
list is fetched at runtime — nothing is baked into the binary.

## Releases

Every push to `main` automatically:

1. bumps the patch version in `VERSION`,
2. builds `WorkBuddy2API.exe` with the version stamped in,
3. commits the bump, tags `vX.Y.Z`,
4. publishes a GitHub Release with the binary attached and the commit log as notes.

No manual tagging required. `ci.yml` runs `go vet` and a build on every push and pull request.

## Security

- Tokens live only in `~/.workbuddy2api-auth.json` on your machine, mode `0600`.
- Credential files, binaries and build output are **gitignored** — the repository holds source only.
- The proxy listens on `localhost` only; it is not reachable from your network.
- Never paste your auth file contents into an issue.

## Requirements

- Windows, macOS or Linux
- Go 1.23+ (only if building from source)

## Building from source

```bash
git clone https://github.com/ErfanBagheri404/WorkBuddy2API.git
cd WorkBuddy2API
go build -ldflags "-X main.Version=v0.1.0" -o WorkBuddy2API.exe .
```

## Disclaimer

Unofficial, community-built tool. Not affiliated with, endorsed by, or
supported by Tencent or the WorkBuddy team. Use it with your own account and at
your own risk; you are responsible for complying with the service's terms.

## License

MIT — see [LICENSE](LICENSE).
