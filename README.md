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

All six roadmap milestones shipped: transparent passthrough proxy (streaming included), per-agent identity, SQLite metering with dollar costs + `GET /usage`, budgets with real-time enforcement + kill switch, OpenAI-compatible endpoints, and Helm/kind packaging. Next: MCP tool-call policy and the dashboard.

## Quickstart

```sh
make build
cp config.example.yaml config.yaml   # add your provider key + agent keys
./bin/tollgate --config config.yaml
```

Point your agent at Tollgate instead of the provider, using its Tollgate agent key in place of the provider key:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=tg_your_agent_key   # terminated at Tollgate, never sent upstream
```

Tollgate authenticates the agent, swaps in the real provider key upstream, and every request produces a structured log line with agent, team, namespace, model, status, latency, and parsed token counts:

```
msg=request provider=anthropic path=/v1/messages status=200 agent=support-bot team=support namespace=prod model=claude-sonnet-5 stream=false input_tokens=25 output_tokens=50
```

Every request is also persisted to SQLite with its dollar cost (from the versioned [pricing table](pricing/pricing.yaml), fixed at request time). Ask who spent what:

```sh
curl "http://localhost:8080/usage?group_by=agent&since=24h" -H "x-api-key: $TOLLGATE_KEY"
```

```json
{"group_by":"agent","rows":[
  {"key":"support-bot","requests":3,"input_tokens":522,"output_tokens":191,"cost_usd":0.004866}
]}
```

`group_by` accepts `agent`, `team`, `namespace`, `model`, or `provider`; `since`/`until` take durations (`24h`) or RFC3339 timestamps; `agent=`/`model=` filter.

### Budgets and the kill switch

Give agents or teams rolling-window budgets; Tollgate enforces them in real time — a runaway loop is counted request by request, not at the next billing sync:

```yaml
budgets:
  - agent: support-bot
    window: 24h
    limit_usd: 10.00
    action: block        # or throttle: 429 + Retry-After, one request per interval
```

At 80% of the limit (configurable) Tollgate logs a warning; at the limit it blocks with a `budget_exceeded` error or throttles with `rate_limit_error` — both in the Anthropic error shape, so SDKs back off natively. And when something is truly on fire:

```sh
curl -X POST http://localhost:8080/admin/agents/support-bot/kill -H "x-admin-key: $ADMIN_KEY"
```

The kill takes effect on the very next request (milliseconds, not minutes), survives restarts, and lifts with `DELETE` on the same path.

### OpenAI-compatible providers (vLLM and friends)

Add an `openai`-type provider and OpenAI-style paths route to it — one Tollgate instance fronts both APIs, and a single agent identity and budget follow the agent across providers:

```yaml
providers:
  - name: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${ANTHROPIC_API_KEY}"
  - name: vllm
    type: openai
    base_url: "http://vllm.internal:8000"
```

OpenAI SDK users set `OPENAI_BASE_URL=http://tollgate:8080/v1` and their Tollgate agent key as the API key. For streaming token counts, request `stream_options: {"include_usage": true}` (vLLM emits the usage chunk the same way).

## Kubernetes (kind quickstart)

Try the full in-cluster experience in ~2 minutes with [kind](https://kind.sigs.k8s.io/):

```sh
kind create cluster --name tollgate
docker build -t tollgate:dev .
kind load docker-image tollgate:dev --name tollgate

kubectl create secret generic tollgate-keys \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=TOLLGATE_ADMIN_KEY=$(openssl rand -hex 16)

cat > my-values.yaml <<'EOF'
image: {repository: tollgate, tag: dev}
existingSecret: tollgate-keys
config:
  server: {listen: ":8080", admin_key: "${TOLLGATE_ADMIN_KEY}"}
  storage: {path: "/data/tollgate.db"}
  providers:
    - name: anthropic
      base_url: "https://api.anthropic.com"
      api_key: "${ANTHROPIC_API_KEY}"
  agents:
    - {name: my-agent, key: "tg_change_me_0123456789abcdef", team: demo}
  budgets:
    - {agent: my-agent, window: 24h, limit_usd: 5.00, action: block}
EOF

helm install tollgate deploy/helm/tollgate -f my-values.yaml
kubectl port-forward svc/tollgate 8080:8080 &

export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=tg_change_me_0123456789abcdef
# ... run your agent, then ask who spent what:
curl "http://localhost:8080/usage" -H "x-api-key: $ANTHROPIC_API_KEY"
```

In production, agents in the cluster point at `http://tollgate.<namespace>.svc:8080` and the chart's `persistence.enabled=true` keeps usage history and kill-switch state across restarts.

## Roadmap

| Milestone | Scope |
|---|---|
| 1 ✅ | Transparent passthrough proxy (Anthropic, streaming included), token usage logged |
| 2 ✅ | Agent identity via API keys, per-agent attribution |
| 3 ✅ | SQLite metering, cost conversion via versioned pricing table, `GET /usage` |
| 4 ✅ | Budgets with enforcement — alert / throttle / block — and kill switch |
| 5 ✅ | OpenAI-compatible endpoint support (vLLM and most agent frameworks) |
| 6 ✅ | Helm chart + kind quickstart |
| Later | MCP tool-call policy, web dashboard |

## License

[Apache-2.0](LICENSE)
