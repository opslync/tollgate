#!/usr/bin/env bash
# Driver for the docker-compose quickstart demo. Talks to the Tollgate
# instance this compose file starts on :8080, using the two demo agent keys
# baked into config.yaml. Also drives the recorded GIF (deploy/compose/demo.tape).
set -uo pipefail
cd "$(dirname "$0")"

TG=http://127.0.0.1:8080
ADMIN_KEY="adm_demo_0123456789abcdef"
CHECKOUT_KEY="tg_demo_checkout_0123456789ab"
REPORTS_KEY="tg_demo_reports_0123456789abc"

key_for() {
  case "$1" in
    checkout-agent) echo "$CHECKOUT_KEY" ;;
    reports-agent)  echo "$REPORTS_KEY" ;;
    *) echo "unknown agent: $1" >&2; exit 1 ;;
  esac
}

cmd_up() {
  docker compose up -d --build
  echo "waiting for tollgate..."
  until curl -s -o /dev/null http://127.0.0.1:8080/healthz; do sleep 1; done
  cmd_urls
}

cmd_down() {
  docker compose down
}

cmd_urls() {
  cat <<EOF
Tollgate    http://localhost:8080
Prometheus  http://localhost:9090
Grafana     http://localhost:3000  (admin / admin, dashboard pre-loaded)
EOF
}

cmd_request() {
  local agent="${1:-checkout-agent}"
  local key; key=$(key_for "$agent")
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$TG/v1/messages" \
    -H "x-api-key: $key" -H 'content-type: application/json' \
    -d '{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}')
  echo "$agent -> HTTP $code"
}

cmd_trip() {
  echo "sending requests as reports-agent until its budget blocks..."
  for i in $(seq 1 10); do
    code=$(cmd_request reports-agent)
    echo "  $code"
    [[ "$code" == *"403"* ]] && { echo "budget tripped after $i requests"; return; }
  done
  echo "budget did not trip in 10 requests"
}

cmd_status() {
  local m; m=$(curl -s "$TG/metrics")
  echo "budget state (0=ok 1=alert 2=throttled 3=blocked):"
  echo "$m" | grep '^tollgate_budget_state{' | sed 's/^/  /'
  echo "budget consumed ratio:"
  echo "$m" | grep '^tollgate_budget_consumed_ratio{' | sed 's/^/  /'
  echo "requests by agent/status:"
  echo "$m" | grep '^tollgate_requests_total{' | sed 's/^/  /'
}

cmd_kill() {
  local agent="${1:?usage: demo.sh kill <agent>}"
  curl -s -X POST "$TG/admin/agents/$agent/kill" -H "x-admin-key: $ADMIN_KEY" -w '\nHTTP %{http_code}\n'
}

cmd_revive() {
  local agent="${1:?usage: demo.sh revive <agent>}"
  curl -s -X DELETE "$TG/admin/agents/$agent/kill" -H "x-admin-key: $ADMIN_KEY" -w '\nHTTP %{http_code}\n'
}

case "${1:-}" in
  up)      cmd_up ;;
  down)    cmd_down ;;
  urls)    cmd_urls ;;
  request) cmd_request "${2:-checkout-agent}" ;;
  trip)    cmd_trip ;;
  status)  cmd_status ;;
  kill)    cmd_kill "${2:-}" ;;
  revive)  cmd_revive "${2:-}" ;;
  *)
    echo "usage: $0 {up|down|urls|request [agent]|trip|status|kill <agent>|revive <agent>}"
    exit 1
    ;;
esac
