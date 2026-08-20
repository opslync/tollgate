# Quickstart: Kubernetes

Try the full in-cluster experience in ~2 minutes with [kind](https://kind.sigs.k8s.io/).

You need `kind`, `kubectl`, `helm`, `docker`, and a clone of the repo — every command below runs from the repo root, since it builds the image from the local `Dockerfile` and installs the chart from `deploy/helm/tollgate`:

```sh
git clone https://github.com/opslync/tollgate.git && cd tollgate
```

## The whole thing

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

!!! note "This one uses your real Anthropic key"

    Unlike the [Compose quickstart](quickstart-compose.md), which ships a mock upstream, this points at `https://api.anthropic.com` and spends real money — bounded by the `$5.00 / 24h` budget in the values above. Swap `base_url` for any OpenAI-compatible endpoint (vLLM included) if you'd rather not, and see [Configuration](configuration.md#providers) for how paths route between provider types.

## What each step is doing

- **`kind create cluster`** — a throwaway single-node cluster named `tollgate`.
- **`docker build` + `kind load docker-image`** — builds the image locally and side-loads it into the kind node, so nothing has to be pushed to a registry. `image: {repository: tollgate, tag: dev}` in the values points the chart at it. For a real cluster, drop both and use the published `ghcr.io/opslync/tollgate` default instead.
- **`kubectl create secret`** — the secret whose keys become environment variables in the pod. `existingSecret: tollgate-keys` wires it in, and the `${ANTHROPIC_API_KEY}` / `${TOLLGATE_ADMIN_KEY}` references in the config resolve from it at startup. A reference to a variable that isn't set is a startup error, not a silent empty value.
- **`my-values.yaml`** — the chart's `config:` block is rendered verbatim into a ConfigMap as `config.yaml`, so it's the same schema documented in [Configuration](configuration.md). Here it defines one agent with one key, and a `$5.00` rolling 24-hour budget that hard-blocks at the limit.
- **`helm install`** — the Deployment, Service, ConfigMap, and (when enabled) PVC, ServiceAccount, RBAC, and ServiceMonitor.
- **`port-forward` + the two env vars** — this is the whole integration story on the agent side. Your agent keeps using its normal SDK; it points `ANTHROPIC_BASE_URL` at Tollgate and sends its Tollgate agent key instead of the provider key. That key is terminated at the proxy and never goes upstream.
- **`curl .../usage`** — the attribution readout. `group_by` accepts `agent`, `team`, `namespace`, `model`, or `provider`; `since`/`until` take durations (`24h`) or RFC3339 timestamps.

## Going to production

In production, agents in the cluster point at `http://tollgate.<namespace>.svc:8080` and the chart's `persistence.enabled=true` keeps usage history and kill-switch state across restarts.

Two more things worth turning on for a real install:

- **Kubernetes-native identity** (`config.kubernetes.enabled`, plus `serviceAccount.create` and `rbac.create`) — agent pods get attributed by their ServiceAccount token via the TokenReview API, with no per-agent key to copy or leak. See [Configuration](configuration.md#kubernetes).
- **`serviceMonitor.enabled`** — scraping for a Prometheus Operator. Set `serviceMonitor.prometheusRelease` alongside it or Prometheus will silently never select it; the [Grafana walkthrough](grafana.md) covers that gotcha in detail.

## Want the Grafana dashboard populated too?

The chart's production defaults (`agents: []`, `budgets: []`) leave most dashboard panels with nothing to group by, so a first install can look broken even when scraping works fine. [`values-demo.yaml`](https://github.com/opslync/tollgate/blob/main/deploy/helm/tollgate/values-demo.yaml) configures two demo agents and budgets specifically so every panel has something to show. Full walkthrough: [Grafana](grafana.md).

## Cleaning up

```sh
kind delete cluster --name tollgate
```

## Next

- [Configuration](configuration.md) — every key in `config.yaml`.
- [Grafana](grafana.md) — `/metrics`, the ServiceMonitor, and the shipped dashboard.
- [Correctness](correctness.md) — what the spend numbers guarantee under restarts and concurrency.
