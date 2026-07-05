---
name: verify
description: Build and drive Tollgate end-to-end against a mock Anthropic upstream to verify proxy behavior at the HTTP surface.
---

# Verifying Tollgate

Tollgate's surface is a socket: agents point their Anthropic base URL at it.
Verify by running the binary and curling through it — not by re-running tests.

## Build & run

```sh
make build                       # -> bin/tollgate
./bin/tollgate --config <cfg>    # config: server.listen + providers[].base_url
```

Point `providers[0].base_url` at a mock Anthropic upstream (real API works too
if `ANTHROPIC_API_KEY` is set — then use `https://api.anthropic.com` and pass
the key as `x-api-key`).

## Mock upstream

A faithful mock lives in the session scratchpad pattern: a small Go server on
127.0.0.1:9911 that answers `POST /v1/messages` with either a real-shaped JSON
Messages response (usage block included) or, when the request body has
`"stream":true`, the full SSE sequence (`message_start` with input/cache
tokens → deltas, 150ms apart, flushed → `message_delta` with final
`output_tokens` → `message_stop`). Honor `Accept-Encoding: gzip` on the JSON
path to exercise Tollgate's gzip transparency.

The mock should only accept the exact provider key (e.g.
`sk-ant-real-provider-key`) — then a 200 through Tollgate proves the
agent→provider credential swap, not just passthrough.

## Flows worth driving

1. Non-streaming: response byte-identical (headers like `request-id` too);
   stdout log line has `model= input_tokens= output_tokens=` matching the
   response's own usage block.
2. Streaming (`curl -sN`, timestamp lines): events must arrive incrementally
   at the upstream's cadence, not in one burst; final log has `stream=true`
   and output tokens from `message_delta`, not `message_start`.
3. Auth (M2+): agent key accepted via both `x-api-key` and
   `Authorization: Bearer`; log line carries `agent= team= namespace=`;
   unknown/missing key → 401 with Anthropic-shaped error body and **zero
   upstream requests** (count mock log lines before/after); `/healthz` stays
   unauthenticated; config without `agents:` starts in open mode with a WARN.
4. Metering (M3+): point `storage.path` at a scratch file; drive known token
   counts and check `GET /usage` returns exact dollar math (mock's JSON
   response = 25 in / 50 out on claude-sonnet-5 → $0.000825; SSE = 472/91
   + 100 cache-write + 200 cache-read → $0.003216). Probe: unauthenticated
   /usage → 401; `group_by=password` and `since=whenever` → 400 JSON errors;
   restart the binary and confirm rows persist.
5. Budgets (M4+): set a tiny `limit_usd` (each mock JSON request costs
   $0.000825) and fire a rapid loop — expect ~limit/0.000825 successes then
   403 `budget_exceeded` (check the message shows the exact limit); throttle
   action → 429 `rate_limit_error` + `Retry-After`, one 200 per interval;
   kill via `POST /admin/agents/{name}/kill` (x-admin-key) → 403
   `agent_disabled` on the next request, still killed after restart, revived
   with DELETE. Admin probes: no/wrong key → 401, unknown agent → 404.
6. Probes: client sends `Accept-Encoding: gzip` (body must stay readable,
   usage still parsed); upstream 4xx error body passes through verbatim with
   `usage=unknown` logged; unknown path passes through; killed upstream → 502
   with `error=` in the log.

## Gotchas

- The request log line is written after the body finishes streaming — sleep
  ~300ms before tailing the log.
- Content-Length becomes Transfer-Encoding: chunked downstream (expected:
  Accept-Encoding is stripped so the transport decompresses upstream gzip).
