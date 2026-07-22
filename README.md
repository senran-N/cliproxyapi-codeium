# devin-codeium — Devin / Windsurf (Codeium) provider for CLIProxyAPI

Exposes the Codeium **`GetChatMessage`** backend (the engine behind Devin / the
Windsurf editor) as a standard provider. Log in with your own account and call
any Devin model — `swe-1-7`, `claude-opus-4.5`, `gpt-5.2`, `gemini-3-flash`, … —
through all three CLIProxyAPI protocols:

| Endpoint | Protocol | Status |
|---|---|---|
| `/v1/chat/completions` | OpenAI chat | ✅ verified (stream + non-stream) |
| `/v1/messages` | Anthropic Messages | ✅ verified (stream + non-stream) |
| `/v1/responses` | OpenAI Responses | ✅ verified |

**Tool calling** works end-to-end on both OpenAI and Anthropic protocols,
including multi-turn agent flows (assistant tool call → tool result → answer).
The executor speaks OpenAI chat internally; the SDK's built-in translators bridge
the Anthropic / Responses protocols in and out.

## Install as a CLIProxyAPI plugin (marketplace)

This repo ships **two forms** of the provider:

1. **Dynamic-library plugin** (`plugin/`, C-ABI) — installable through the
   CLIProxyAPI plugin store. Add this repo's registry to your config:

   ```yaml
   plugins:
     enabled: true
     store-sources:
       - "https://raw.githubusercontent.com/senran-N/cliproxyapi-codeium/main/registry.json"
   ```

   The `codeium` plugin then appears in the plugin store
   (`GET /v0/management/plugin-store` / the management panel's Plugin Store),
   alongside the official plugins. Install it, then add a `codeium` auth file
   (`{"type":"codeium","session_token":"devin-session-token$…"}`).

   Then install the `codeium` plugin from the management UI / plugin store. The
   shared libraries (`.so` / `.dylib` / `.dll`) are built by CI and attached to
   each GitHub Release.

2. **Standalone server** (repo root) — a self-contained CLIProxyAPI build that
   embeds the provider; run it directly (see *Run* below). Best for trying it out
   without the plugin store.

> Status: **verified end-to-end**. The C-ABI plugin (`plugin/`) was loaded into a
> real CLIProxyAPI server and served live completions — `/v1/chat/completions`,
> `/v1/messages` (Anthropic, host-translated), and premium models (Claude Opus
> 4.5) all returned correctly. Shared libraries are built by CI for
> linux/amd64, darwin/arm64, and windows/amd64.

## Models & thinking variants

`/v1/models` lists the ~10 **base model families** the Devin picker shows
(`claude-opus-4.8`, `claude-fable-5`, `claude-sonnet-5`, `gpt-5.6-sol`,
`gpt-5.6-luna`, `glm-5.2`, `kimi-k2.7`, `swe-1.7`, `adaptive`, …), fetched live
from your account. The backend also exposes ~150 thinking/context **variants** of
those families (Low / Medium / High / XHigh / Max, Fast, 1M-context); the plugin
selects the right variant from the request's **reasoning effort** instead of
cluttering the model list.

**In Claude Code:** pick a base model (e.g. `claude-opus-4.8`) and use its
thinking control (think / think hard / ultrathink, or a thinking budget). The
effort is mapped to the matching variant automatically:

| reasoning effort | example wire model |
|---|---|
| *(none / default)* | `claude-opus-4-8-medium` |
| `low` | `claude-opus-4-8-low` |
| `high` | `claude-opus-4-8-high` |
| `xhigh` | `claude-opus-4-8-xhigh` |
| `max` | `claude-opus-4-8-max` |

**In OpenAI-style clients:** add `"reasoning_effort": "high"` to the request.

Not every family has every tier (e.g. `glm-5.2` only has base + `max`); missing
tiers fall back to the family default.

## How it works

```
OpenAI /v1/chat/completions
        │  (identity translator)
        ▼
codeiumExecutor
        │  1. session_token ──► exa.auth_pb.AuthService/GetUserJwt ──► short-lived api JWT (cached, auto-refreshed by exp)
        │  2. build GetChatMessageRequest protobuf (metadata + system + messages + tools + model=f21)
        │  3. POST exa.api_server_pb.ApiServerService/GetChatMessage   (Connect-RPC, application/connect+proto, gzip)
        │  4. parse streamed frames (f9 = text delta) ──► OpenAI SSE / completion
        ▼
server.codeium.com
```

Everything is hand-rolled over the raw protobuf wire format (`proto.go`) — no
generated stubs — because the upstream `.proto` files are not published. The
field numbers were reverse-engineered from captured traffic.

| File | Responsibility |
|------|----------------|
| `proto.go` | protobuf wire writer/reader + Connect envelope framing + gzip |
| `metadata.go` | shared `ClientMetadata` message (auth + chat variants) |
| `auth.go` | `GetUserJwt` refresh + JWT cache (keyed by session token, refreshes before `exp`) |
| `chat.go` | OpenAI ⇄ `GetChatMessage` request/response translation |
| `executor.go` | SDK `Executor` (Execute / ExecuteStream) + OpenAI output |
| `models.go` | model catalogue for `/v1/models` |
| `main.go` | SDK wiring (builder, translator, model registry) |
| `metadata_test.go` | **byte-for-byte** check against a captured `GetUserJwt` request |

## Getting your login credentials

The **only** thing you must supply is your **session token**
(`devin-session-token$<jwt>`, whose JWT payload is just
`{"session_id":"windsurf-session-…"}`). Grab it from a running Devin/Windsurf
while proxying its `language_server_windows_x64.exe` traffic (e.g. Reqable), out
of any `exa.auth_pb.AuthService/GetUserJwt` or `GetChatMessage` request metadata.

Drop this into your auth dir as `codeium-devin.json` (the file token store reads
the provider from the `type` field and passes the rest through as metadata):

```json
{
  "type": "codeium",
  "session_token": "devin-session-token$<your jwt>"
}
```

Everything else is handled automatically:

- **`team_id` / `user_id`** — parsed from the JWT the server mints for you (no
  need to provide them; override in `attributes` only if you want to force a value).
- **Device fingerprints** (`device_id`, `hw_hash`, `hash27`, `os_json`,
  `cpu_json`) — generated from the local machine and persisted once to
  `<user-config-dir>/cliproxy-codeium/identity.json`, so every request presents
  stable, self-consistent values. **Nothing machine-specific is hardcoded.**
- **`hex31` / `f16` / `f30`** — structured blobs whose meaning is not yet known;
  omitted by default. If the backend ever rejects a request for a missing field,
  capture that one value and paste it into `attributes` (e.g. `"hex31": "…"`).

The session token is the actual credential; the fingerprints are only telemetry /
install identifiers.

## Run

```bash
cd examples/devin-codeium/example
go run ../ --config ./config.yaml
# proxy on :8317, auth files in ./auths
```

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer sk-local-devin-proxy" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-5",
    "stream": true,
    "messages": [{"role":"user","content":"write quicksort in go"}]
  }'
```

Switch models by changing `"model"` — the value is passed straight through as the
upstream `f21` model selector.

## Status

**Verified live against `server.codeium.com`** — `GetUserJwt` mints a JWT and
`GetChatMessage` streams a real completion end-to-end, with **fully
auto-generated fingerprints** (only `session_token` supplied). Example:

```
model=swe-1-7
reasoning: The user asked for a simple response: exactly "pong"...
content:   "pong"
```

- **Auth + chat + streaming** — working end-to-end. ✅
- **Premium models work** — Claude Opus/Sonnet/Haiku 4.5, GPT-5.2, Gemini 3
  Flash all verified returning content on a standard Devin Pro account. ✅
- **Fingerprints** — generated locally, no hardcoded machine values; accepted by
  the backend. ✅
- **Response mapping** — `f3` → `content` (the answer), `f9` →
  `reasoning_content` (the chain-of-thought). ✅
- **Model ids must be the internal enum** — the backend rejects display names.
  Send a friendly id from the table below (mapped automatically to the `MODEL_*`
  enum in `models.go`), or the raw enum. Using a display name like
  `claude-sonnet-5` yields `permission_denied`.
- **Static client config** — `f7/f8/f9/f13` (incl. the Cascade capability block)
  are replayed from `staticconfig.go`; omitting them yields
  `failed_precondition: please update your editor`. Refresh the blob if you bump
  `ext_version` to a build with a different capability set.

### Models (friendly id → upstream enum)

| Friendly id | Upstream `f21` |
|---|---|
| `swe-1-7`, `swe-1-6` | `swe-1-7` / `swe-1-6` |
| `claude-opus-4.5` | `MODEL_CLAUDE_4_5_OPUS` |
| `claude-opus-4.5-thinking` | `MODEL_CLAUDE_4_5_OPUS_THINKING` |
| `claude-sonnet-4.5` | `MODEL_PRIVATE_2` |
| `claude-sonnet-4.5-thinking` | `MODEL_PRIVATE_3` |
| `claude-haiku-4.5` | `MODEL_PRIVATE_11` |
| `gpt-5.2` / `-none/-low/-high/-xhigh` | `MODEL_GPT_5_2_MEDIUM` / … |
| `gemini-3-flash` / `-minimal/-low/-high` | `MODEL_GOOGLE_GEMINI_3_0_FLASH_*` |
| `gemini-2.5-flash` | `MODEL_GOOGLE_GEMINI_2_5_FLASH` |

Enum ids are read from the client's `GetCliModelConfigs` / `GetCommandModelConfigs`
catalogs and change over time — refresh `models.go` if the roster changes.

To reproduce the live check:

```bash
CODEIUM_SESSION_TOKEN='devin-session-token$...' CODEIUM_MODEL=swe-1-7 \
  go test -v -run TestSmokeLive ./examples/devin-codeium/
```

Possible future refinement: **tool-call id mapping** (Codeium uses
`functions.<name>:<idx>`; round-tripping OpenAI `tool_calls` may need tuning for
multi-tool agent flows).

## Concurrency & multi-credential isolation

Designed for CPA importing several Devin accounts and serving many concurrent
requests:

- **Per-account isolation** — all state (JWT cache, device fingerprint,
  team/user ids) is keyed by the account's session token. Two imported accounts
  never share a JWT or a device identity; each presents its **own** fingerprint,
  so a device-scoped rate limit on the backend treats them independently
  (importing N accounts actually multiplies capacity).
- **No thundering herd** — concurrent requests that find an expired JWT collapse
  into a single `GetUserJwt` refresh per account via `singleflight`; the refresh
  runs on a detached, bounded context so one caller cancelling cannot poison the
  shared result.
- **No goroutine/connection leaks** — streaming sends select on the request
  context and abort on client disconnect (closing the upstream body); HTTP
  clients are pooled per proxy URL instead of rebuilt per request.
- **No shared mutable request state** — `providerConfig` is a per-request value
  copy; the executor is stateless; conversation/turn ids are fresh per request.

Verified by `concurrency_test.go` (fingerprint isolation/stability, 200-goroutine
cache access, single-flight collapse to exactly one refresh). Run
`go test ./examples/devin-codeium/`; add `-race` on a host with a C toolchain.

## Route through Reqable to debug

Uncomment `proxy-url: "http://127.0.0.1:9000"` in `config.yaml` to send the
upstream Codeium calls through Reqable and inspect the exact bytes.
