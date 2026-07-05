# Tollgate — project context for Claude

## What Tollgate is

AI runtime governance for Kubernetes. One line: "See, budget, and control every token and tool call your AI agents make inside your own cluster."

A proxy + control plane that platform/FinOps teams install in their own K8s cluster. AI agents' outbound LLM API traffic (Anthropic, OpenAI-compatible endpoints incl. vLLM) routes through it. Three pillars:

1. **Attribution** — every request tagged with an agent identity (API key per agent); token usage parsed from provider responses and attributed to agent/team/namespace.
2. **Budgets with real-time enforcement** — not retrospective reporting. Per-agent or per-team token/dollar budgets; alert at threshold, throttle or hard-block at limit; kill switch for a runaway agent loop, effective in seconds.
3. **Audit** — every LLM call and (later) MCP tool call logged: agent, model, tokens, cost, latency, status, timestamp.

Cost governance is the wedge; MCP tool-call security policy (allow-lists, deny-by-default) rides on the same chassis later. Open source (Apache-2.0); a SaaS/dashboard layer monetizes later, so repo hygiene matters.

## Tech decisions (settled — do not relitigate)

- Go, single static binary, minimal dependencies (stdlib `net/http`; chi only if routing outgrows it)
- SQLite for usage/audit storage — zero-dependency demo installs beat scale right now
- Config via one YAML file: agents, keys, budgets, upstream providers
- Proxy is provider-transparent: agents just change their base URL to Tollgate; requests forwarded unmodified, responses parsed for usage fields. Streaming supported — usage arrives in the final SSE `message_delta` event.
- Pricing table for cost conversion lives in a versioned YAML we maintain
- Kubernetes/Helm packaging is a later milestone; must run locally first
- License: Apache-2.0

## Architecture

```
cmd/tollgate/      — entrypoint: flags, config load, server lifecycle, recorder glue
internal/config/   — YAML config load + validation (agents, providers, storage, env expansion)
internal/auth/     — agent-key authentication middleware, agent identity in context
internal/proxy/    — reverse proxy, streaming passthrough, key injection, logging, Recorder hook
internal/meter/    — provider response parsing → token usage
internal/store/    — SQLite persistence (modernc.org/sqlite, pure Go) + aggregation
internal/api/      — Tollgate's own endpoints (GET /usage, /admin kill switch)
internal/budget/   — budget engine + enforcement middleware (alert/throttle/block/kill)
pricing/           — versioned pricing.yaml (embedded via go:embed) + cost conversion
```

Later milestones add `deploy/helm` (M6). Don't create directories before their milestone.

Metering notes:
- Cost is computed and stored at request time — pricing table updates never rewrite history. Unknown models record cost 0 with a warning log.
- SQLite runs WAL + busy_timeout(5000); the pure-Go driver keeps `CGO_ENABLED=0` static builds (it also forced go.mod to go 1.25).
- `GET /usage` group_by is an allowlist (agent/team/namespace/model/provider) — never interpolate caller input into SQL.

Budget enforcement notes:
- Middleware order is auth → budget → proxy. Spend counters live in memory: seeded/re-synced from the store every 5s (which ages spend out of rolling windows) plus live increments per completed request — runaway loops are caught request-by-request. Bias is fail-closed (brief overcount possible, undercount not); storage errors enforce with stale counters rather than failing requests.
- Enforcement errors use the Anthropic shape: throttle = 429 `rate_limit_error` + Retry-After (SDKs back off natively); block = 403 `budget_exceeded`; kill = 403 `agent_disabled`.
- Kill switch: /admin endpoints (constant-time admin-key check), in-memory effect is immediate and persisted in the kills table so restarts don't revive. Unknown agent names 404 (typo protection).

Proxy implementation notes:
- `httputil.ReverseProxy` with `Rewrite`; client headers pass through untouched **except credentials**: agents authenticate with their Tollgate key in `x-api-key` or `Authorization: Bearer`; when the provider has an `api_key` configured, that key is terminated at the proxy and the provider key is injected upstream (`x-api-key` set, `Authorization` stripped). Empty `agents:` list = open passthrough mode with a startup warning.
- `Accept-Encoding` is stripped outbound so Go's transport handles gzip transparently and the meter always sees plaintext.
- `FlushInterval: -1` for immediate SSE flush.
- Usage is parsed by a tee reader wrapped around the response body in `ModifyResponse`; one structured `slog` line per request when the body completes. Parse failures never break the proxy.
- Anthropic streaming usage: `message_start` carries model + `input_tokens` (+ cache tokens); final `message_delta` carries `output_tokens`.
- Providers have a `type` (anthropic default | openai; one per type for now). Routing is path-based: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` → openai provider; `/v1/messages` → anthropic; everything else → providers[0]. Credential injection is type-native (`x-api-key` vs `Authorization: Bearer`).
- OpenAI usage semantics: `prompt_tokens` INCLUDES `cached_tokens` — the parser subtracts so `meter.Usage.InputTokens` is always the uncached remainder across providers. Streaming usage arrives in the final non-null `usage` chunk (`stream_options.include_usage`; vLLM matches) before `[DONE]`.

## Roadmap

- **M1** ✅ (shipped 2026-07-05): transparent passthrough proxy to Anthropic; streaming included; per-request token usage logged to stdout.
- **M2** ✅ (shipped 2026-07-05): agent identity via API keys + per-agent attribution; provider key injection.
- **M3** ✅ (shipped 2026-07-05): SQLite metering + cost conversion (versioned pricing YAML) + `GET /usage`.
- **M4** ✅ (shipped 2026-07-05): budgets with enforcement — alert / throttle / block — + kill switch.
- **M5** ✅ (shipped 2026-07-05): OpenAI-compatible endpoint support (covers vLLM and most agent frameworks).
- **M6**: Helm chart + kind quickstart.
- **After M6**: MCP tool-call policy, React dashboard.

## Working agreements

- One milestone per session. Small commits. Tests land with each feature.
- Progress is tracked in GitHub milestones + issues on `opslync/tollgate` (one tracking issue per milestone, M2–M6 = issues #1–#5). A milestone session ends by checking off its issue's scope list and closing the issue and milestone.
- `make build` / `make test` / `make lint` must stay green; CI runs build, `go test -race`, and golangci-lint.
- Maintainer background: DevOps engineer, 10 years EKS/Kubernetes, comfortable in Go.
