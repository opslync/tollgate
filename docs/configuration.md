# Configuration

Tollgate is configured by one YAML file. Point the binary at it with `--config`:

```sh
./bin/tollgate --config config.yaml
```

[`config.example.yaml`](https://github.com/opslync/tollgate/blob/main/config.example.yaml) in the repo is a working, fully-commented starting point — copy it to `config.yaml` and adjust. In Kubernetes the same schema goes in the Helm chart's `config:` block, which is rendered verbatim into a ConfigMap as `config.yaml`; see [`values.yaml`](https://github.com/opslync/tollgate/blob/main/deploy/helm/tollgate/values.yaml).

Two things worth knowing before you start:

- **Unknown fields are rejected.** A typo'd key fails at startup instead of being silently ignored.
- **Secrets come from the environment.** `providers[].api_key` and `server.admin_key` expand `${ENV_VAR}` references, so no secret has to live in the YAML. A reference to an unset or empty variable is a startup error — proxying with an empty upstream key would fail somewhere far more confusing.

## `server`

```yaml
server:
  listen: ":8080"
  # Enables the /admin kill-switch endpoints when set.
  admin_key: "${TOLLGATE_ADMIN_KEY}"
```

| Field | Type | Notes |
|---|---|---|
| `listen` | string | Required. Address the proxy listens on. |
| `admin_key` | string | Optional. When set, enables the `/admin` endpoints (the kill switch). Supports `${ENV_VAR}`. |

The admin key is checked in constant time. With it set:

```sh
curl -X POST http://localhost:8080/admin/agents/support-bot/kill -H "x-admin-key: $ADMIN_KEY"
```

takes effect on that agent's very next request and survives a restart; `DELETE` on the same path lifts it. An unknown agent name returns `404`, so a typo can't silently kill nothing.

## `storage`

```yaml
# SQLite database for usage/audit records (created if missing).
storage:
  path: "tollgate.db"
```

| Field | Type | Default | Notes |
|---|---|---|---|
| `path` | string | `tollgate.db` | SQLite database file for usage and audit records. Created if missing. |

In Kubernetes this should point inside a mounted volume — the chart uses `/data/tollgate.db` and `persistence.enabled=true` keeps usage history and kill-switch state across pod restarts. With persistence off it's an `emptyDir`, and the data goes when the pod moves.

## `providers`

Upstream LLM providers.

```yaml
providers:
  - name: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${ANTHROPIC_API_KEY}"
  # OpenAI-compatible providers (OpenAI, vLLM, most self-hosted servers).
  # /v1/chat/completions, /v1/completions, /v1/embeddings route here; omit
  # api_key for servers without auth (e.g. vLLM) to pass credentials through.
  - name: vllm
    type: openai
    base_url: "http://vllm.internal:8000"
```

| Field | Type | Default | Notes |
|---|---|---|---|
| `name` | string | — | Required, unique. Appears in logs, `/usage`, and metrics. |
| `base_url` | string | — | Required. Must be `http(s)://host[:port]`. |
| `type` | string | `anthropic` | `anthropic` or `openai`. Selects the wire protocol: usage parsing, credential header, and which paths route here. |
| `api_key` | string | — | Optional. The real provider key, injected upstream in place of the agent's Tollgate key. Supports `${ENV_VAR}`. When omitted, the caller's own credentials pass through unchanged. |

At least one provider is required. Names must be unique, and for now there can be **one provider per type** — one `anthropic` and one `openai` at most.

**Routing is path-based.** `/v1/chat/completions`, `/v1/completions`, and `/v1/embeddings` go to the `openai` provider; `/v1/messages` goes to the `anthropic` one; anything else goes to the first entry in the list. One Tollgate instance can therefore front both APIs, with a single agent identity and budget following the agent across them.

**Credential handling is type-native.** Agents authenticate to Tollgate with their Tollgate key in whichever header their SDK already uses (`x-api-key` or `Authorization: Bearer`). When the matched provider has an `api_key`, that Tollgate key is terminated at the proxy and the provider key injected upstream in the header that provider expects. Client headers otherwise pass through untouched.

OpenAI SDK users set `OPENAI_BASE_URL=http://tollgate:8080/v1` and their Tollgate agent key as the API key. For streaming token counts, request `stream_options: {"include_usage": true}` — vLLM emits the usage chunk the same way.

## `agents`

Who is calling, and what their spend gets attributed to.

```yaml
agents:
  - name: support-bot
    key: "tg_replace_with_openssl_rand_hex_16"
    team: support
    namespace: prod
```

| Field | Type | Notes |
|---|---|---|
| `name` | string | Required, unique. The attribution key in logs, `/usage`, metrics, and budgets. |
| `key` | string | Required. Minimum 16 characters, unique across agents. |
| `team` | string | Optional. Groups agents for team budgets and `group_by=team`. |
| `namespace` | string | Optional. Attribution dimension; `group_by=namespace`. |

Generate keys with something like `openssl rand -hex 16`. The minimum length is enforced at startup: once a provider `api_key` is configured, agent keys are what stand between the internet and your LLM bill.

!!! warning "An empty `agents:` list disables authentication"

    Tollgate then runs in open passthrough mode — every request is forwarded, unattributed — and logs a warning at startup. Useful for a first look at the proxy; not something to leave on.

Agents don't need static keys in Kubernetes. See [`kubernetes`](#kubernetes) below.

## `budgets`

Rolling-window spend limits, per agent or per team, enforced before the request goes upstream.

```yaml
budgets:
  - agent: support-bot
    window: 24h
    limit_usd: 10.00
    action: block
  - team: support
    window: 7d
    limit_usd: 100.00
    alert_at: 0.75
    action: throttle
    throttle_interval: 60s
```

| Field | Type | Default | Notes |
|---|---|---|---|
| `agent` | string | — | Exactly one of `agent` or `team` must be set. Must name a configured agent. |
| `team` | string | — | Must name a team that exists — either in [`teams`](#teams) or inline on an agent. |
| `window` | duration | — | Required. Rolling window. Go duration syntax plus an integer `d` suffix (`30m`, `24h`, `7d`). |
| `limit_usd` | float | — | Dollar limit. At least one of `limit_usd` or `limit_tokens` must be positive. |
| `limit_tokens` | int | — | Token limit, counting input + output. |
| `alert_at` | float | `0.8` | Fraction of the limit that logs a warning. Must be within `(0, 1]`. |
| `action` | string | `block` | `block` (`403`) or `throttle` (`429` + `Retry-After`, one request per `throttle_interval`). |
| `throttle_interval` | duration | `30s` | Only meaningful with `action: throttle`. |

Enforcement errors use the Anthropic error shape, so SDKs back off natively without special handling: throttle is `429 rate_limit_error` with a `Retry-After`, block is `403 budget_exceeded`, and a killed agent is `403 agent_disabled`.

Spend counters live in memory — re-synced from the store every few seconds, which is also what ages spend out of the rolling window, plus incremented live as each request completes. That's what makes a runaway loop catchable request-by-request rather than at the next reporting interval. The bias is fail-closed: a brief overcount is possible, an undercount is not, and a storage error enforces on stale counters rather than failing the request. The bound on that overshoot is measured and published in [Correctness](correctness.md#budget-overshoot).

## `kubernetes`

Kubernetes-native identity. Optional, disabled by default.

```yaml
kubernetes:
  enabled: false
  # poll_interval: 15s        # pod-cache refresh; must be >= 1s
  # audiences: []             # TokenReview audience allowlist; empty accepts any
```

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool | `false` | Turns on ServiceAccount-based identity. |
| `poll_interval` | duration | `15s` | Pod-cache refresh interval. Must be at least `1s`. |
| `audiences` | list of string | `[]` | TokenReview audience allowlist. Empty accepts any audience. |
| `namespaces` | list of string | `[]` | Reserved for future scoping. Empty means the pod cache reads across all namespaces — the ClusterRole is cluster-wide either way. |

When enabled and running in-cluster, an agent pod with no Tollgate key is attributed by its ServiceAccount token, validated through the Kubernetes TokenReview API. Its agent name becomes `<namespace>/<workload>` — e.g. `payments/checkout-worker`. This is the difference between an identity a platform team binds to a workload and a key an agent can copy, share, or paste into a notebook.

Static agent keys keep working unchanged alongside it, for agents outside the cluster.

Requires the ServiceAccount and RBAC from the Helm chart — set `serviceAccount.create=true` and `rbac.create=true`.

## `teams`

Map namespaces to teams so budgets and `/usage` can aggregate by team.

```yaml
teams:
  - name: payments
    namespaces: [payments, payments-staging]
  - name: search
    namespaces: [search]
```

| Field | Type | Notes |
|---|---|---|
| `name` | string | Required, unique. |
| `namespaces` | list of string | Namespaces belonging to this team. Each namespace may belong to at most one team. |

A namespace's `tollgate.io/team` label is resolved at runtime and **takes precedence** over this static list:

```sh
kubectl label namespace payments tollgate.io/team=payments
```

`teams` is optional. A team budget also validates against any team named inline on an agent, so configs that only use `agents[].team` work without a `teams` block at all.

## `tracing`

OTLP/HTTP trace export. Optional, off by default so non-observability installs stay zero-dependency.

```yaml
tracing:
  enabled: true
  otlp_endpoint: "http://otel-collector.monitoring.svc:4318/v1/traces"
```

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool | `false` | When true, `otlp_endpoint` is required. |
| `otlp_endpoint` | string | — | The full traces URL including path. Must be `http(s)://host[:port]/path`. The scheme decides TLS vs plaintext. |

One span per proxied request, with `gen_ai.*` and `tollgate.*` attributes (agent, team, namespace, cost, token counts), POSTed as JSON. Export is fire-and-forget: a slow or unreachable collector never blocks a proxied request. See [Grafana](grafana.md#optional-otlp-trace-export).

Prometheus metrics need no configuration at all — `/metrics` is always on, on the same port as the proxy, unauthenticated like `/healthz`.

## Related

- [Grafana](grafana.md) — `/metrics`, the ServiceMonitor, and the shipped dashboard.
- [Quickstart: Kubernetes](quickstart-k8s.md) — the same schema through the Helm chart.
- [Correctness](correctness.md) — what these budgets guarantee, and the two bounds that don't fully close.
