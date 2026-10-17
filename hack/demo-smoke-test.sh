#!/usr/bin/env bash
# Brings up the demo stack (docker-compose.yml) and checks the whole chain:
# simulated Asterisk -> exporter -> Prometheus (rules loaded) -> Grafana (datasource
# healthy, dashboard provisioned). Requires docker (compose v2), curl and python3.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
PASS=0 FAIL=0

pass() { echo "[PASS] $1"; PASS=$((PASS+1)); }
fail() { echo "[FAIL] $1"; FAIL=$((FAIL+1)); }
# wait_for "<description>" <seconds> <command...>
wait_for() {
  local name="$1" secs="$2"; shift 2
  for _ in $(seq 1 "$secs"); do
    if "$@" >/dev/null 2>&1; then pass "$name"; return 0; fi
    sleep 1
  done
  fail "$name"; return 1
}

cleanup() {
  if [ "$FAIL" -ne 0 ]; then
    docker compose ps -a || true
    docker compose logs --no-color --tail=60 || true
  fi
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker compose down -v --remove-orphans >/dev/null 2>&1 || true
docker compose up -d --build

prom() { curl -sf -m 5 "http://127.0.0.1:9090$1"; }
graf() { curl -sf -m 5 "http://127.0.0.1:3000$1"; }

metric_is_up()   { curl -sf -m 5 http://127.0.0.1:9650/metrics | grep -q '^asterisk_up 1$'; }
prom_sees_up()   { prom '/api/v1/query?query=asterisk_up' | python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; sys.exit(0 if r and r[0]["value"][1]=="1" else 1)'; }
rules_loaded()   { prom '/api/v1/rules' | python3 -c 'import json,sys; g=[g for g in json.load(sys.stdin)["data"]["groups"] if g["name"]=="asterisk"]; sys.exit(0 if g and len(g[0]["rules"])>=9 and all(r["health"]=="ok" for r in g[0]["rules"]) else 1)'; }
grafana_ready()  { graf '/api/health' | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["database"]=="ok" else 1)'; }
datasource_ok()  { graf '/api/datasources/uid/prometheus/health' | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["status"]=="OK" else 1)'; }
dashboard_there() { graf '/api/dashboards/uid/asterisk-overview' | python3 -c 'import json,sys; d=json.load(sys.stdin)["dashboard"]; sys.exit(0 if d["title"]=="Asterisk" and len(d["panels"])>10 else 1)'; }

wait_for "exporter reports asterisk_up 1 (simulated AMI reachable)" 60 metric_is_up || true
wait_for "Prometheus scraped the exporter (asterisk_up == 1)" 90 prom_sees_up || true
wait_for "Prometheus loaded the alert rules, all healthy" 30 rules_loaded || true
wait_for "Grafana is up" 90 grafana_ready || true
wait_for "Grafana datasource Prometheus is healthy" 30 datasource_ok || true
wait_for "Grafana provisioned the Asterisk dashboard" 30 dashboard_there || true

# every dashboard query must run against the demo data
sleep 10 # a few scrapes, so rate-free queries have samples
if python3 hack/check-dashboard.py live deploy/grafana/dashboards/asterisk.json http://127.0.0.1:9090; then pass "all dashboard queries return data"; else fail "dashboard queries"; fi

echo
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
