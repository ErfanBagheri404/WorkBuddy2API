# WorkBuddy2API

OpenAI-compatible proxy for **WorkBuddy AI** (Tencent CodeBuddy family, `www.workbuddy.ai`).

Point any OpenAI client at `http://localhost:61021/v1` and use WorkBuddy's
models without the desktop app open.

## How it works

1. **Login (OAuth device flow):** the CLI asks WorkBuddy for a login URL,
   opens it in your browser, and polls until you've signed in.
2. **Tokens are saved** to `~/.workbuddy2api-auth.json` and auto-refreshed —
   the proxy keeps working with WorkBuddy closed.
3. **OpenAI translation:** `/v1/chat/completions` requests are forwarded to
   WorkBuddy's `/v2/chat/completions` (SSE streaming supported).
4. **Models are fetched live** from `/v2/enterprises/personal/models` —
   nothing is hardcoded.

## Usage

Double-click `WorkBuddy2API.exe`, or from a terminal:

```
WorkBuddy2API.exe
```

First run: press Enter, log in in the browser, then pick from the menu:

```
  1. Status        — auth state + live model list
  2. Start server  — OpenAI proxy on :61021
  3. List models   — fetch models from WorkBuddy
  4. Test chat     — quick chat without starting the server
  5. Re-login      — get fresh credentials
  6. Quit
```

Headless (server only, needs prior login):

```
WorkBuddy2API.exe --headless
```

## Endpoints

| Method | Path                      | Notes                        |
| ------ | ------------------------- | ---------------------------- |
| GET    | `/healthz`                | liveness check               |
| GET    | `/v1/models`              | live model list from WorkBuddy |
| POST   | `/v1/chat/completions`    | OpenAI-compatible, SSE stream |

Example:

```bash
curl http://localhost:61021/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"default-model","stream":false,
       "messages":[{"role":"user","content":"hi"}]}'
```

## Releases

Push a tag to cut a release — the `release` workflow builds
`WorkBuddy2API.exe` with the version stamped in and attaches it to the
GitHub release:

```bash
git tag v0.2.0
git push origin v0.2.0
```

Every push to `main` runs `go vet` + build via the `ci` workflow.

## Security notes

- Auth tokens live in `~/.workbuddy2api-auth.json` (mode `0600`) and are
  **gitignored** — never commit them, never paste them into issues.
- The `.exe` and editor backups are gitignored too; releases ship only via
  GitHub Releases artifacts.
