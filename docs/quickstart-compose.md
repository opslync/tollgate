# Quickstart: Docker Compose

The fastest way to see Tollgate work. No cluster, no API key, no config file to hand-edit — the demo ships with a mock Anthropic upstream, so nothing is spent and nothing leaves your machine.

You need Docker with the Compose plugin, and `git`.

## Two commands

```sh
git clone https://github.com/opslync/tollgate.git && cd tollgate/deploy/compose
./demo.sh up      # builds and starts Tollgate + a mock upstream + Prometheus + Grafana
./demo.sh trip     # sends traffic until the demo budget alerts, then hard-blocks
```

`up` builds the images, waits for `/healthz` to answer, then prints the URLs:

| Service | URL | Notes |
|---|---|---|
| Tollgate | `http://localhost:8080` | the proxy, `/usage`, `/admin`, `/metrics` |
| Prometheus | `http://localhost:9090` | scraping Tollgate's `/metrics` |
| Grafana | `http://localhost:3000` | `admin` / `admin`, dashboard pre-loaded |

`trip` sends up to ten requests as `reports-agent` and prints the HTTP status of each, stopping as soon as one comes back `403` — that's the budget hard-blocking. Open the Grafana dashboard while it runs and you'll see the budget-consumed gauge climb, the budget-state timeline step from OK to Alert to Blocked, and denied requests start counting.

## What's running

Four containers, defined in [`docker-compose.yml`](https://github.com/opslync/tollgate/blob/main/deploy/compose/docker-compose.yml):

- **`tollgate`** — built from the repo's `Dockerfile`, configured by [`demo-config.yaml`](https://github.com/opslync/tollgate/blob/main/deploy/compose/demo-config.yaml).
- **`mock-anthropic`** — a tiny stand-in for the Anthropic API ([`deploy/compose/mock`](https://github.com/opslync/tollgate/tree/main/deploy/compose/mock)) that returns plausible responses with token counts, so the demo needs no real key and costs nothing.
- **`prometheus`** — scraping Tollgate's always-on `/metrics`.
- **`grafana`** — with the Prometheus datasource and the Tollgate dashboard provisioned on startup.

The demo config defines two agents on purpose, so every panel has something to show on first run:

| Agent | Team | Budget (30m window) | Behaviour |
|---|---|---|---|
| `checkout-agent` | `payments` | `$0.50`, alert at 80%, block | plenty of headroom — steady "healthy" traffic |
| `reports-agent` | `analytics` | `$0.003`, alert at 50%, block | tight — trips into a hard block within a few requests |

Without that second agent, the budget-state and denied-requests panels would sit empty even though everything is working correctly.

## The rest of `demo.sh`

```
usage: ./demo.sh {up|down|urls|request [agent]|trip|status|kill <agent>|revive <agent>}
```

- **`up`** — `docker compose up -d --build`, wait for `/healthz`, then print the URLs.
- **`down`** — `docker compose down`.
- **`urls`** — reprint the three service URLs.
- **`request [agent]`** — send one `POST /v1/messages` through Tollgate as that agent (default `checkout-agent`; the other valid name is `reports-agent`) and print the HTTP status it came back with.
- **`trip`** — the loop described above: up to ten `reports-agent` requests, stopping on the first `403`.
- **`status`** — read `/metrics` and print the budget state per target (`0=ok 1=alert 2=throttled 3=blocked`), the budget-consumed ratio, and request counts by agent and status.
- **`kill <agent>`** — `POST /admin/agents/<agent>/kill` with the demo admin key. The agent's very next request gets a `403 agent_disabled`.
- **`revive <agent>`** — `DELETE` on the same path, lifting the kill.

A good sequence after `trip`: run `./demo.sh status` to see `reports-agent` sitting at state `3`, then `./demo.sh kill checkout-agent` and `./demo.sh request checkout-agent` to watch a healthy agent get cut off on demand, and `./demo.sh revive checkout-agent` to bring it back.

The agent keys and the admin key are hard-coded demo values in `demo.sh` and `demo-config.yaml`. They are not secrets and are not meant to be reused anywhere.

## Pointing it at the real API

`demo-config.yaml` documents the one change: set the provider's `base_url` to `https://api.anthropic.com` and its `api_key` to `${ANTHROPIC_API_KEY}`, then export that variable for the container. Everything else — agents, budgets, the dashboard — works unchanged.

## Tearing it down

```sh
./demo.sh down
```

The SQLite database lives on a `tmpfs`, so stopping the demo takes the usage history with it. That is deliberate for a demo; for anything real, see [Configuration](configuration.md#storage) and the [Kubernetes quickstart](quickstart-k8s.md).

## Next

- [Quickstart: Kubernetes](quickstart-k8s.md) — the same thing in a cluster, via the Helm chart.
- [Configuration](configuration.md) — every key in `config.yaml`.
- [Grafana](grafana.md) — wiring `/metrics` into an existing Prometheus Operator setup rather than the bundled one.
