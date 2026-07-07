# Tollgate in your Grafana in 10 minutes

`/metrics` is always on — no config needed, same port as the proxy, unauthenticated like `/healthz`. This walks through wiring it into a Prometheus Operator setup (e.g. [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)) and importing the shipped dashboard.

## Prereqs

- Tollgate installed via the [kind quickstart](../README.md#kubernetes-kind-quickstart) (or any cluster with the Helm chart installed).
- A Prometheus Operator. If you don't have one yet, the fast path:

  ```sh
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
  helm repo update
  helm install prom prometheus-community/kube-prometheus-stack -n monitoring --create-namespace
  ```

## 1. Turn on the ServiceMonitor

The **#1 "no data" gotcha**: kube-prometheus-stack's Prometheus only scrapes ServiceMonitors matching its `serviceMonitorSelector` — by default, ones labeled `release: <the helm release name you installed prometheus as>`. Set that label when you enable the toggle:

```sh
helm upgrade tollgate deploy/helm/tollgate -f my-values.yaml \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.labels.release=prom
```

(`prom` matches the `helm install prom ...` above — use whatever release name you actually used.)

## 2. Verify the scrape

Port-forward Prometheus and check the Targets page:

```sh
kubectl port-forward -n monitoring svc/prom-kube-prometheus-stack-prometheus 9090:9090
```

Open `http://localhost:9090/targets` — you should see a `tollgate` target in state `UP`. If it's missing entirely, the ServiceMonitor's label didn't match Prometheus's selector (see step 1); if it's `DOWN`, check `kubectl logs deploy/tollgate` and confirm `/metrics` responds via `kubectl port-forward svc/tollgate 8080:8080` + `curl localhost:8080/metrics`.

## 3. Import the dashboard

In Grafana: **Dashboards → New → Import**, upload [`deploy/grafana/tollgate-dashboard.json`](../deploy/grafana/tollgate-dashboard.json), and pick your Prometheus datasource when prompted (the dashboard ships with a templated datasource input, so it isn't hardcoded to whichever Grafana it was exported from).

## 4. What "done" looks like

Send a request or two through Tollgate, then watch the **Spend by agent** panel — it should populate within one scrape interval (30s by default). The other panels (requests/sec, tokens in/out, budget consumed %, budget state, p95 latency, denied requests) follow the same pattern: `sum by (agent) (...)` queries over the metrics Tollgate exposes.

## Optional: OTLP trace export

If you run a trace collector (Tempo, Jaeger, an OTel Collector, etc.), point Tollgate at it:

```yaml
config:
  tracing:
    enabled: true
    otlp_endpoint: "http://otel-collector.monitoring.svc:4318/v1/traces"
```

One span per proxied request, with `gen_ai.*` and `tollgate.*` attributes (agent, team, namespace, cost, token counts). Export is fire-and-forget — a slow or unreachable collector never blocks proxied requests.

## Troubleshooting

- **ServiceMonitor exists but no data in Grafana**: almost always the `release:` label mismatch from step 1 — `kubectl get servicemonitor tollgate -o yaml` and compare its labels against your Prometheus's `serviceMonitorSelector`.
- **Some agents missing from panels**: expected, not a bug — unattributed/open-passthrough traffic (no `agents:` configured, no Kubernetes identity) has no `agent` label to group by.
- **`tollgate_cost_usd_total` is `0` for a known-working agent**: check for a `model missing from pricing table, cost recorded as 0` warning in the Tollgate logs — cost is `0` for unpriced models by design, not a metrics bug.
