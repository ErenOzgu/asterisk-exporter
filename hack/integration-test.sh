#!/usr/bin/env bash
# =========================================================
# Integration test against a REAL Asterisk (no simulator).
#
# What it does: configures Asterisk (AMI user, three PJSIP endpoints, a ConfBridge
# extension), registers one endpoint with hack/sipstub.py, places calls and
# conference legs with Local channels, then scrapes the exporter and checks the
# numbers. Finally it stops Asterisk and checks that asterisk_up drops to 0.
#
# Requirements: root, asterisk (apt install asterisk), go, python3, curl.
# It overwrites manager.conf, pjsip.conf and extensions.conf in $ASTERISK_CONF_DIR
# (backups are restored on exit) and uses 127.0.0.1:5038 (AMI), :5060 (SIP) and
# :9650 (exporter).
# =========================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONF="${ASTERISK_CONF_DIR:-/etc/asterisk}"
WORK="$(mktemp -d)"
export TZ=UTC # AMI prints local wall-clock time; pin both sides to UTC
EXPORTER_PID="" STUB_PID=""

for tool in asterisk go python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing tool: $tool" >&2; exit 2; }
done
[ "$(id -u)" = 0 ] || { echo "run as root (needs to write $CONF and start asterisk)" >&2; exit 2; }

cleanup() {
  [ -n "$EXPORTER_PID" ] && kill "$EXPORTER_PID" 2>/dev/null || true
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  asterisk -rx "core stop now" >/dev/null 2>&1 || true
  for f in manager pjsip extensions; do
    [ -f "$CONF/$f.conf.it-bak" ] && mv -f "$CONF/$f.conf.it-bak" "$CONF/$f.conf"
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0 FAIL=0
metrics() { curl -sf -m 10 http://127.0.0.1:9650/metrics; }
# expect "<series>" "<value>"  — exact match on the sample value
expect() {
  local series="$1" want="$2" got
  got="$(metrics | awk -v s="$series" '$1 == s { print $2 }')"
  if [ "$got" = "$want" ]; then echo "[PASS] $series = $got"; PASS=$((PASS+1))
  else echo "[FAIL] $series = '${got:-<missing>}', want $want"; FAIL=$((FAIL+1)); fi
}
# expect_cmp "<series>" "<awk condition on v>"  — e.g. 'v > 0 && v < 1'
expect_cmp() {
  local series="$1" cond="$2" got
  got="$(metrics | awk -v s="$series" '$1 == s { print $2 }')"
  if [ -n "$got" ] && awk -v v="$got" "BEGIN { exit !($cond) }"; then echo "[PASS] $series = $got ($cond)"; PASS=$((PASS+1))
  else echo "[FAIL] $series = '${got:-<missing>}', want $cond"; FAIL=$((FAIL+1)); fi
}

echo "== Asterisk: $(asterisk -V)"
asterisk -rx "core stop now" >/dev/null 2>&1 || true
sleep 1

for f in manager pjsip extensions; do [ -f "$CONF/$f.conf" ] && cp -a "$CONF/$f.conf" "$CONF/$f.conf.it-bak"; done
# the example config shipped for users, with the test secret
sed 's/^secret = .*/secret = it-secret/' "$ROOT/deploy/asterisk/manager.conf" > "$CONF/manager.conf"
cat > "$CONF/pjsip.conf" <<CFG
[transport-udp]
type = transport
protocol = udp
bind = 127.0.0.1:5060

; 100 registers dynamically (hack/sipstub.py) and is qualified
[100]
type = endpoint
context = it
disallow = all
allow = ulaw
auth = auth100
aors = 100
[auth100]
type = auth
auth_type = userpass
username = 100
password = pw100
[100]
type = aor
max_contacts = 2
qualify_frequency = 2
remove_existing = yes

; 101 has a static contact and no qualify
[101]
type = endpoint
context = it
disallow = all
allow = ulaw
aors = 101
[101]
type = aor
contact = sip:101@127.0.0.1:5099
qualify_frequency = 0

; 102 has a static contact that nobody answers
[102]
type = endpoint
context = it
disallow = all
allow = ulaw
aors = 102
[102]
type = aor
contact = sip:102@127.0.0.1:5098
qualify_frequency = 5
CFG
cat > "$CONF/extensions.conf" <<CFG
[it]
exten => s,1,Answer()
 same => n,Wait(300)
exten => 700,1,Answer()
 same => n,ConfBridge(700)
CFG
chown asterisk:asterisk "$CONF"/{manager,pjsip,extensions}.conf 2>/dev/null || true

echo "== starting Asterisk"
asterisk -U asterisk -G asterisk
for _ in $(seq 1 60); do asterisk -rx "core waitfullybooted" >/dev/null 2>&1 && break; sleep 1; done
asterisk -rx "core waitfullybooted" >/dev/null

python3 "$ROOT/hack/sipstub.py" --user 100 --password pw100 --port 5099 >"$WORK/sipstub.log" 2>&1 &
STUB_PID=$!
for _ in $(seq 1 20); do grep -q "registered 100" "$WORK/sipstub.log" 2>/dev/null && break; sleep 0.5; done
grep -q "registered 100" "$WORK/sipstub.log" || { echo "SIP stub could not register:"; cat "$WORK/sipstub.log"; exit 1; }
sleep 3 # let Asterisk qualify the contact at least once

asterisk -rx "channel originate Local/s@it application Wait 300" >/dev/null
asterisk -rx "channel originate Local/700@it application Wait 300" >/dev/null
asterisk -rx "channel originate Local/700@it application Wait 300" >/dev/null
sleep 2

echo "== building and starting the exporter"
(cd "$ROOT" && go build -o "$WORK/asterisk-exporter" ./cmd/asterisk-exporter)
ASTERISK_AMI_USERNAME=exporter ASTERISK_AMI_SECRET=it-secret "$WORK/asterisk-exporter" \
  -web.listen-address 127.0.0.1:9650 -asterisk.timezone UTC -collect.sip >"$WORK/exporter.log" 2>&1 &
EXPORTER_PID=$!
for _ in $(seq 1 20); do metrics >/dev/null 2>&1 && break; sleep 0.5; done

echo "== checks"
expect 'asterisk_up' 1
for c in core channels pjsip sip confbridge; do expect "asterisk_collector_success{collector=\"$c\"}" 1; done
now=$(date +%s)
expect_cmp 'asterisk_core_start_time_seconds' "v <= $now && v > $now - 900" # started within the last 15 minutes: proves the time zone handling
metrics | grep -q '^asterisk_info{' && { echo "[PASS] asterisk_info present"; PASS=$((PASS+1)); } || { echo "[FAIL] asterisk_info missing"; FAIL=$((FAIL+1)); }

expect 'asterisk_calls_active' 3
expect 'asterisk_channels{state="up"}' 6
expect_cmp 'asterisk_core_current_channels' "v >= 6"
expect 'asterisk_confbridge_rooms' 1
expect 'asterisk_confbridge_parties{conference="700"}' 2

expect 'asterisk_pjsip_endpoint_up{endpoint="100"}' 1
expect 'asterisk_pjsip_endpoint_up{endpoint="101"}' 1
expect 'asterisk_pjsip_endpoint_up{endpoint="102"}' 0
expect 'asterisk_pjsip_contacts{status="reachable"}' 1
expect_cmp 'asterisk_pjsip_contact_rtt_seconds{endpoint="100"}' "v > 0 && v < 1"

echo "== Asterisk goes away"
asterisk -rx "core stop now" >/dev/null 2>&1 || true
sleep 3
expect 'asterisk_up' 0

echo
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || { echo "--- exporter.log ---"; cat "$WORK/exporter.log"; exit 1; }
