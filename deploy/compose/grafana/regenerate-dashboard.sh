#!/usr/bin/env bash
# Regenerates dashboards/tollgate-dashboard.json from the canonical
# deploy/grafana/tollgate-dashboard.json whenever that file changes.
#
# The canonical file uses a ${DS_PROMETHEUS} template token, resolved only by
# Grafana's Import UI (the Helm/kind path in docs/grafana.md). File-provisioned
# dashboards (this compose stack) don't do that substitution, so this script
# bakes in the fixed datasource uid from grafana/provisioning/datasources/datasource.yml.
set -euo pipefail
cd "$(dirname "$0")"

jq '(.. | objects | select(has("uid") and .uid == "${DS_PROMETHEUS}") | .uid) |= "prometheus"' \
  ../../grafana/tollgate-dashboard.json > dashboards/tollgate-dashboard.json

echo "wrote dashboards/tollgate-dashboard.json"
