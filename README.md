# Tollgate

**AI runtime governance for Kubernetes.** See, budget, and control every token and tool call your AI agents make — inside your own cluster.

Tollgate is a proxy + control plane that platform and FinOps teams install in their own Kubernetes cluster. AI agents route their outbound LLM API traffic (Anthropic today; OpenAI-compatible endpoints including vLLM on the roadmap) through it by changing one setting: the API base URL.

## What it does

- **Attribution** — every request is tagged with an agent identity. Token usage is parsed from provider responses and attributed to agent, team, and namespace.
- **Budgets with real-time enforcement** — not retrospective reporting. Per-agent or per-team token/dollar budgets: alert at threshold, throttle or hard-block at limit, and a kill switch that stops a runaway agent loop in seconds.
- **Audit** — every LLM call (and later, MCP tool call) logged: agent, model, tokens, cost, latency, status, timestamp.

Cost governance is the wedge; MCP tool-call policy (allow-lists, deny-by-default) rides on the same chassis later.

## Design principles

- **Provider-transparent.** Agents just change their base URL. Requests are forwarded unmodified; responses (including streaming) are parsed for usage on the way through.
- **Zero-dependency install.** Single static Go binary, SQLite storage, one YAML config file. Runs locally with nothing else; Helm chart for Kubernetes coming as a milestone.
- **Open source.** Apache-2.0.

## Status

Early development. Current milestone: **M1 — transparent passthrough proxy** to the Anthropic API with per-request token usage logged to stdout.

## Quickstart

```sh
make build
cp config.example.yaml config.yaml
./bin/tollgate --config config.yaml
```

Point your agent at Tollgate instead of the provider:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
```

Every request through the proxy produces a structured log line with model, status, latency, and parsed token counts.

## Roadmap

| Milestone | Scope |
|---|---|
| 1 | Transparent passthrough proxy (Anthropic, streaming included), token usage logged |
| 2 | Agent identity via API keys, per-agent attribution |
| 3 | SQLite metering, cost conversion via versioned pricing table, `GET /usage` |
| 4 | Budgets with enforcement — alert / throttle / block — and kill switch |
| 5 | OpenAI-compatible endpoint support (vLLM and most agent frameworks) |
| 6 | Helm chart + kind quickstart |
| Later | MCP tool-call policy, web dashboard |

## License

[Apache-2.0](LICENSE)
