# Tollgate Architecture

Tollgate is a single Go binary that sits between your AI agents and the LLM
APIs they call. Agents change one setting — their base URL — and every
request flows through Tollgate on its way to the real provider (Anthropic,
or any OpenAI-compatible server such as vLLM).

## Request flow

```
                       ┌─ auth ──────┐   ┌─ budget ─────┐   ┌─ proxy ─────────────────────┐
Agent                  │ resolve key │   │ Check(agent) │   │ rewrite → forward → stream   │        Upstream
 (Tollgate agent key) ─▶ → Agent     ├──▶│ allow/       ├──▶│ back; meter response body    ├───────▶ (Anthropic /
                       │  in ctx,    │   │ throttle/    │   │ as it streams; on completion  │        OpenAI-compat /
                       │  else 401   │   │ block/killed │   │ call Recorder + log the line  │        vLLM)
                       └─────────────┘   └──────────────┘   └──────────────┬───────────────┘
                                          429/403 short-                    │ RequestRecord
                                          circuits here                    ▼
                                                                   ┌─ Recorder (main.go) ─┐
                                                                   │ cost := pricing.Cost │
                                                                   │ store.Insert(record) │
                                                                   │ engine.Record(spend) │──▶ feeds back into
                                                                   └──────────────────────┘    budget's live counters
```

Every proxied request passes through three middleware layers in this order,
wired in `cmd/tollgate/main.go`:

1. **`internal/auth`** — extracts the caller's key from `x-api-key` or
   `Authorization: Bearer` (whichever the agent's SDK already uses), looks it
   up against configured agents, and either 401s or attaches the matched
   `auth.Agent` (name/team/namespace) to the request context. Skipped
   entirely (open mode) if no agents are configured.
2. **`internal/budget`** — reads the agent from context and asks the
   enforcement engine for a decision: allow, throttle (429 +
   `Retry-After`), block (403 `budget_exceeded`), or killed (403
   `agent_disabled`). A non-allow decision short-circuits here; the request
   never reaches the proxy or the upstream.
3. **`internal/proxy`** — the actual reverse proxy. Rewrites the request
   onto the upstream URL, strips `Accept-Encoding` so Go's transport
   handles gzip transparently, and injects the provider's real credential
   in its native header (`x-api-key` for Anthropic, `Authorization: Bearer`
   for OpenAI-compatible) if one is configured — the agent's Tollgate key
   never reaches the upstream. The response streams back to the agent
   immediately (`FlushInterval: -1`); a tee reader feeds the same bytes to
   a usage parser as they pass through, so metering adds no buffering and
   no latency.

When the response body finishes streaming, the proxy builds a
`RequestRecord` (agent, model, status, latency, parsed token usage) and
hands it to a `Recorder` callback installed by `main.go`. That callback is
where the side effects happen: convert tokens to dollars via the pricing
table, insert the record into SQLite, and feed the spend back into the
budget engine's in-memory counters — which is what lets a runaway loop get
blocked on its *next* request rather than at some later polling interval.

## Path-based provider routing

Each configured provider gets its own `proxy.Proxy` instance. Requests route
by path so agents stay drop-in on either protocol:

| Path | Routes to |
|---|---|
| `/v1/messages`, `/v1/messages/*` | the `anthropic`-type provider |
| `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` | the `openai`-type provider |
| anything else | the first configured provider |

One provider per type today — see `internal/config`'s validation.

## Packages

| Package | Responsibility |
|---|---|
| `cmd/tollgate/` | Entrypoint: flags, config load, wires every package together, HTTP server lifecycle (graceful shutdown on SIGINT/SIGTERM). |
| `internal/config/` | Loads and validates `config.yaml` — agents, providers (with type), storage path, budgets, admin key — including `${ENV_VAR}` expansion for secrets. |
| `internal/auth/` | Agent-key authentication middleware; `auth.Agent` travels in the request context for every downstream layer. |
| `internal/budget/` | The enforcement engine (`Engine.Check` / `Engine.Record`) and its HTTP middleware. Spend is tracked in memory — seeded from SQLite and re-synced every 5s (which also ages spend out of rolling windows) plus live increments per completed request. The kill switch is immediate in memory and persisted to SQLite so a restart doesn't revive a killed agent. |
| `internal/proxy/` | The reverse proxy itself: request rewriting, credential injection, streaming passthrough, and the metering tee. Provider-agnostic — the same `Proxy` type serves both Anthropic and OpenAI-compatible upstreams via `Options.Type`. |
| `internal/meter/` | Parses token usage out of provider response bodies without buffering the stream — separate JSON and SSE parsers for Anthropic and OpenAI wire formats (OpenAI's `prompt_tokens` includes cached tokens; the parser subtracts so `Usage` semantics stay uniform across providers). |
| `internal/store/` | SQLite persistence (`modernc.org/sqlite`, pure Go — keeps the binary CGO-free and statically linked). Two tables: `requests` (one row per proxied call, cost fixed at write time) and `kills` (kill-switch state). `Aggregate` powers `GET /usage`; `Spend` powers the budget engine's re-sync. |
| `internal/api/` | Tollgate's own HTTP endpoints: `GET /usage` (aggregated spend, filterable by agent/team/namespace/model/provider and time window) and the `/admin/agents/{name}/kill` kill-switch endpoints, gated by a constant-time-compared admin key. |
| `pricing/` | A versioned `pricing.yaml` (embedded into the binary via `go:embed`) mapping model IDs to per-million-token rates, plus the `Cost()` conversion. Model IDs resolve by exact match, then longest dash-boundary prefix, so dated snapshots (`claude-haiku-4-5-20251001`) price correctly. |

## Data model

Two SQLite tables, created and migrated by `internal/store` on startup:

- **`requests`** — one row per proxied call: timestamp, agent/team/namespace,
  provider/model, method/path, status, duration, token counts (including
  cache read/write), and the dollar cost computed at insert time. Indexed on
  `ts` and `(agent, ts)` for the aggregation and budget-refresh queries.
- **`kills`** — one row per currently-killed agent name. Existence in this
  table is the persisted kill state; `budget.Engine` loads it at startup and
  keeps its own in-memory copy for zero-latency checks.

## Deployment

- **Binary**: `CGO_ENABLED=0`, statically linked — runs anywhere with no
  runtime dependencies beyond the SQLite file it manages itself.
- **Container**: multi-stage `Dockerfile`, final stage is
  `distroless/static-debian12:nonroot` (CA certs included, no shell).
- **Kubernetes**: `deploy/helm/tollgate` — single-replica `Recreate`
  deployment (SQLite has exactly one writer), config rendered from Helm
  values into a `ConfigMap` with a checksum annotation so pods roll on
  config changes, an optional `PersistentVolumeClaim` for the SQLite file,
  and `existingSecret` for injecting the environment variables that
  `${ENV_VAR}` references in config resolve against.

See the [README](README.md) for how to run it locally, in Kubernetes, and
the full configuration reference.
