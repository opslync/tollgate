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
cmd/tollgate/      — entrypoint: flags, config load, server lifecycle
internal/config/   — YAML config load + validation
internal/proxy/    — reverse proxy, streaming passthrough, request logging
internal/meter/    — provider response parsing → token usage
```

Later milestones add `internal/auth` (M2), `internal/store` (M3), `internal/budget` (M4), `pricing/pricing.yaml` (M3), `deploy/helm` (M6). Don't create directories before their milestone.

Proxy implementation notes:
- `httputil.ReverseProxy` with `Rewrite`; client headers (incl. `x-api-key`) pass through untouched.
- `Accept-Encoding` is stripped outbound so Go's transport handles gzip transparently and the meter always sees plaintext.
- `FlushInterval: -1` for immediate SSE flush.
- Usage is parsed by a tee reader wrapped around the response body in `ModifyResponse`; one structured `slog` line per request when the body completes. Parse failures never break the proxy.
- Anthropic streaming usage: `message_start` carries model + `input_tokens` (+ cache tokens); final `message_delta` carries `output_tokens`.

## Roadmap

- **M1** (done first): transparent passthrough proxy to Anthropic. Agent points base URL at Tollgate; requests forward untouched (streaming included); each request logged to stdout with parsed token usage. No auth, budgets, or storage.
- **M2**: agent identity via API keys + per-agent attribution.
- **M3**: SQLite metering + cost conversion (versioned pricing YAML) + `GET /usage`.
- **M4**: budgets with enforcement — alert / throttle / block — + kill switch.
- **M5**: OpenAI-compatible endpoint support (covers vLLM and most agent frameworks).
- **M6**: Helm chart + kind quickstart.
- **After M6**: MCP tool-call policy, React dashboard.

## Working agreements

- One milestone per session. Small commits. Tests land with each feature.
- Progress is tracked in GitHub milestones + issues on `opslync/tollgate` (one tracking issue per milestone, M2–M6 = issues #1–#5). A milestone session ends by checking off its issue's scope list and closing the issue and milestone.
- `make build` / `make test` / `make lint` must stay green; CI runs build, `go test -race`, and golangci-lint.
- Maintainer background: DevOps engineer, 10 years EKS/Kubernetes, comfortable in Go.
