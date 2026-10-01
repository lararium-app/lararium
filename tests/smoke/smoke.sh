#!/usr/bin/env bash
# Integration smoke for the T5 wiring: full CLI lifecycle with the
# network cage and proxy on a real host. Run as root on a Linux box
# with systemd and nftables. Mirrors tests/cellunit/SMOKE.md; exit 0
# means the release can be called alpha-candidate.
#
#   LARARIUM_CELL_ROOT=/some/abs/root ./tests/smoke/smoke.sh
set -u
if [ "$(id -u)" -ne 0 ]; then echo "error: run as root (drives nftables + systemd)" >&2; exit 1; fi
BIN="${CELL_BIN:-cell}"
ROOT="${LARARIUM_CELL_ROOT:?set LARARIUM_CELL_ROOT to an absolute scratch root}"
CFG="$ROOT/smoke.yaml"
ID=smoke-$-
PROXY_PORT="${PROXY_PORT:-3128}"
GW=10.91.0.1
fail=0
step() { printf '== %s\n' "$*"; }
ok()   { printf 'ok: %s\n' "$*"; }
bad()  { printf 'FAIL: %s\n' "$*"; fail=1; }

cleanup() {
  "$BIN" --config "$CFG" destroy "$ID" --yes >/dev/null 2>&1 || true
  "$BIN" --config "$CFG" net-remove >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$ROOT"
cat > "$CFG" <<EOF
schema_version: 1
hearth: $ROOT/hearth
cell:
  memory_mb: 512
  cpu_quota: "100"
  tasks_max: 256
EOF

step "net-install is idempotent"
"$BIN" --config "$CFG" net-install && "$BIN" --config "$CFG" net-install || bad "net-install failed"
nft list table inet lararium_filter >/dev/null || bad "filter table missing after net-install"
# Interop chain is conditional by spec: skipped on nft-only hosts
# where the iptables binary is absent (CELL-SPEC §4).
if command -v iptables >/dev/null 2>&1; then
  iptables -w -C INPUT -j LARARIUM-INPUT 2>/dev/null \
    || bad "interop chain not wired into INPUT"
fi

step "create/start/status/stop/destroy lifecycle"
"$BIN" --config "$CFG" create "$ID" || bad "create"
"$BIN" --config "$CFG" start "$ID" || bad "start"
state=$("$BIN" --config "$CFG" status "$ID" 2>/dev/null | grep -oE "running|starting|stopped" | head -1)
[ "$state" = "running" ] || bad "status shows '$state', want running"
ip -o link | grep -q "v-lar-" || bad "no v-lar-* host veth after start"

step "cooperative egress (proxied) works"
out=$("$BIN" --config "$CFG" run "$ID" -- curl -s -o /dev/null -w '%{http_code}' --max-time 20 http://example.com/ 2>/dev/null)
case "$out" in 200|301|308) ok "http coop ($out)";; *) bad "http coop got '$out'";; esac

step "chunked POST survives the proxy (framing regression)"
head -c 200000 /dev/urandom | base64 > "$ROOT/cells/$ID/workspace/chunk.bin"
out=$("$BIN" --config "$CFG" run "$ID" -- curl -s -o /dev/null -w '%{http_code}' --max-time 30 -H 'Transfer-Encoding: chunked' --data-binary @/workspace/chunk.bin http://example.com/ 2>/dev/null)
case "$out" in 405|200|413) ok "chunked POST answered, not RST ($out)";; *) bad "chunked POST got '$out' (any REAL response beats a reset)";; esac

step "cooperative HTTPS (CONNECT tunnel) works"
out=$("$BIN" --config "$CFG" run "$ID" -- curl -s -o /dev/null -w '%{http_code}' --max-time 20 https://example.com/ 2>/dev/null)
case "$out" in 200|301|308) ok "https coop ($out)";; *) bad "https coop got '$out'";; esac

step "fail-closed: guest bypass dies with no leak"
"$BIN" --config "$CFG" run "$ID" -- curl -s -o /dev/null --max-time 10 --noproxy '*' http://example.com/ 2>/dev/null
rc=$?
[ $rc -ne 0 ] || bad "direct fetch unexpectedly succeeded"
grep -E ":53 " "$ROOT/proxy/access.log" \
  && bad "DNS reached the proxy" || ok "no DNS in access.log"

step "floor: cell cannot reach the host's own addresses"
own=$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
"$BIN" --config "$CFG" run "$ID" -- curl -s --max-time 10 -x http://$GW:$PROXY_PORT "http://$own/" >/dev/null 2>&1
grep -q "refused-floor" "$ROOT/proxy/access.log" && ok "host-IP refused-floor logged" || bad "no refused-floor entry for $own"

step "doctor runs clean-ish"
"$BIN" --config "$CFG" doctor >/dev/null 2>&1; rc=$?
[ $rc -eq 0 ] && ok "doctor rc=0" || bad "doctor rc=$rc"

step "stop/destroy unwind"
"$BIN" --config "$CFG" stop "$ID" || bad "stop"
"$BIN" --config "$CFG" destroy "$ID" --yes || bad "destroy"

[ $fail -eq 0 ] && echo "SMOKE: ALL PASS" || echo "SMOKE: FAILURES"
exit $fail
