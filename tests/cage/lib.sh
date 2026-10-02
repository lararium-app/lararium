# Cage helper library — sourced by run.sh. Not executable alone.
# shellcheck shell=bash

ok() { printf 'ok: %s\n' "$*"; }
# CAGE_FAIL counts failures (set by run.sh). Increment, never set to
# a constant: run_check compares before/after, and a boolean would
# make every group after the first failure report "passed".
# shellcheck disable=SC2034
bad() { printf 'FAIL: %s\n' "$*"; CAGE_FAIL=$((CAGE_FAIL + 1)); }

# cellrun -- cmd... runs cmd inside $ID as root via `cell run`.
cellrun() {
  local extra=()
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--" ]; then shift; break; fi
    extra+=("$1"); shift
  done
  "$BIN" --config "$CFG" run "$ID" "${extra[@]}" -- "$@"
}

# expect_fail "name" cmd... — passes if cmd exits non-zero.
expect_fail() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then
    bad "$name: expected failure, got success"
  else
    ok "$name blocked"
  fi
}

# fresh_cell — create+start a new cell (id in $ID), destroying the old.
fresh_cell() {
  local no_proxy="${1:-}"
  "$BIN" --config "$CFG" destroy "$ID" --yes >/dev/null 2>&1 || true
  local args=(create)
  [ "$no_proxy" = "--no-proxy" ] && args+=(--no-proxy)
  args+=("$ID")
  "$BIN" --config "$CFG" "${args[@]}" >/dev/null || { bad "fresh_cell: create"; return 1; }
  "$BIN" --config "$CFG" start "$ID" >/dev/null || { bad "fresh_cell: start"; return 1; }
  derive_gw
}

# derive_gw — the gateway is 10.91.<n>.1 with n = the cell's ALLOCATED
# subnet index (cell.json), not a constant: recreation can move a cell
# to a different /28. Hardcoding .0.1 only works on the first cell.
derive_gw() {
  local idx
  idx=$(sed -n 's/.*"subnet_index":[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
    "$ROOT/cells/$ID/cell.json" 2>/dev/null)
  [ -n "$idx" ] || { bad "derive_gw: no subnet_index in cell.json"; return 1; }
  GW="10.91.$idx.1"
}

# settle — bounded wait (<=15s) for guest addressing: a bare TCP
# handshake to the proxy door (GW:PROXY_PORT) must succeed. Probe is
# connect-only — the proxy answers junk with 400/close, which still
# proves the door — and no ICMP: the cage rejects ping by spec.
settle() {
  for _i in $(seq 1 15); do
    cellrun -- timeout 2 bash -c "</dev/tcp/$GW/$PROXY_PORT" 2>/dev/null && return 0
    sleep 1
  done
  return 1
}
