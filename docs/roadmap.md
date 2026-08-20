# Roadmap

Milestone strategy and sequencing. Tracked issues live on the [milestones page](https://github.com/opslync/tollgate/milestones).

## Versioning

One milestone = one release, tags are immutable once announced. `v0.1.0` = M1–M6, `v0.2.0` = M7, `v0.3.0` = M8. Next milestone (M9) ships as `v0.4.0`.

## Shipped

- **M1** ✅ (2026-07-05, v0.1.0): transparent passthrough proxy to Anthropic; streaming included; per-request token usage logged to stdout.
- **M2** ✅ (2026-07-05, v0.1.0): agent identity via API keys + per-agent attribution; provider key injection.
- **M3** ✅ (2026-07-05, v0.1.0): SQLite metering + cost conversion (versioned pricing YAML) + `GET /usage`.
- **M4** ✅ (2026-07-05, v0.1.0): budgets with enforcement — alert / throttle / block — + kill switch.
- **M5** ✅ (2026-07-05, v0.1.0): OpenAI-compatible endpoint support (covers vLLM and most agent frameworks).
- **M6** ✅ (2026-07-05, v0.1.0): Helm chart + kind quickstart.
- **M7** ✅ (2026-07-06, v0.2.0): Kubernetes-native identity & attribution. ServiceAccount-bound identity via TokenReview alongside static API keys; pod → namespace/deployment/ServiceAccount enrichment via a hand-rolled minimal K8s REST client (no client-go); namespace/label → team mapping; `GET /usage` grouping by team/deployment.
- **M8** ✅ (2026-07-08, v0.3.0): Prometheus metrics + OTel export. `/metrics` (always-on, unauthenticated — per-agent/team token/cost counters, latency histogram, budget-state gauges); hand-rolled OTLP/HTTP+JSON trace export (no OTel SDK); Grafana dashboard JSON + `ServiceMonitor` in Helm; [`grafana.md`](grafana.md) walkthrough. Verified end-to-end on a fresh kind cluster 2026-08-09; docker-compose quickstart + demo GIF + two Helm chart bugs ([#10](https://github.com/opslync/tollgate/issues/10), [#11](https://github.com/opslync/tollgate/issues/11)) shipped alongside it.

## Post-v0.1.0 sequencing

Adoption-led: meet platform teams where they are before building the hardest consumer — MCP enforcement — on top of a general policy engine.

### Phase 2 — Kubernetes awareness

- **M9**: MCP passthrough + audit-only logging. Transparent proxy for MCP servers (Streamable HTTP/SSE); every tool call logged (agent, server, tool, args summary, status, latency) to the existing audit store; `GET /audit/tools`. No enforcement yet — plants the "we see every tool call" category flag ~2 quarters before M11.

### Phase 3 — Policy engine

- **M10**: General policy engine. One evaluation chassis (subject selectors, rule type, effect, precedence, default posture) that budgets (M4) get refactored onto and model-access rules plug into; `environment` dimension (dev/staging/prod); dry-run (`effect: audit`) mode; decision logging.

### Phase 4 — MCP tool governance

- **M11**: MCP enforcement. Tool/server allow-lists and deny-by-default as a policy rule type (rides on M9 + M10); approval gates (`require_approval` + webhook/Slack notify + TTL timeout-deny); shipped presets for GitHub/AWS/DB/kubectl-style tools.
- **M12**: Audit export & compliance pack. JSONL/CSV export with filters; hash-chained tamper-evident audit records + verification CLI; retention policies; EU AI Act / SOC 2 mapping docs. First natural OSS/paid seam (compliance evidence packs, long retention, signed attestations → paid tier).

### Phase 5 — Dashboard

After M12, once the paid surface (policy management, approval inbox, fleet view, compliance export) is well-defined. Prometheus/Grafana from M8 covers visibility until then.

## Deliberately not building

Model routing/fallback/caching (that's LiteLLM's fight, dilutes governance positioning); Postgres (SQLite is the zero-dependency install story — add it only when a real user hits the wall); dashboard before M12.
