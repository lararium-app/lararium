# C9 — crash containment (spec §8). SIGKILL the boot unit mid-run; the
# host must be unaffected, `start` must work again, workspace intact.
# Contract: run.sh sourced us inside a fresh settled proxy cell ($ID,
# $BIN, $CFG, $ROOT, ok/bad/cellrun/fresh_cell/derive_gw/settle).
# shellcheck shell=bash

check_c09() {
  local unit marker
  # Unit name per internal/cell.UnitName + boot.go systemd-run argv.
  unit="lararium-cell-${ID}.service"

  # Plant a workspace marker as keeper-visible root inside the cell.
  if ! cellrun -- bash -c 'echo c9-marker > /workspace/c9.txt' >/dev/null 2>&1; then
    bad "C9: could not plant workspace marker"
    return
  fi

  # Kill the boot unit mid-life: SIGKILL, no graceful shutdown path.
  systemctl kill -s KILL "$unit" >/dev/null 2>&1
  # systemctl kill is async: CONFIRM the unit actually died, else a
  # later `start` is a no-op against the unkilled original (false green).
  local dead=0 _i
  for _i in $(seq 1 15); do
    systemctl is-active --quiet "$unit" || { dead=1; break; }
    sleep 1
  done
  if [ "$dead" -ne 1 ]; then
    bad "C9: unit still active 15s after SIGKILL"
    return
  fi
  ok "C9: unit confirmed dead after SIGKILL"

  # Host unaffected: systemctl still answers for the unit's manager.
  if ! systemctl show "$unit" --property=LoadState >/dev/null 2>&1; then
    bad "C9: systemctl broken after kill (host affected)"
    return
  fi
  ok "C9: host systemd healthy after SIGKILL"

  # Start again on the same cell id (spec: `start` works again).
  if ! "$BIN" --config "$CFG" start "$ID" >/dev/null 2>&1; then
    bad "C9: restart after crash failed"
    return
  fi
  ok "C9: start works again after crash"

  # Workspace intact: marker readable through the operator path.
  marker=$(cat "$ROOT/cells/$ID/workspace/c9.txt" 2>/dev/null)
  if [ "$marker" = "c9-marker" ]; then
    ok "C9: workspace intact across crash"
  else
    bad "C9: workspace marker lost after restart"
  fi

  # And the cell is functional again (door reachable).
  derive_gw
  if settle; then
    ok "C9: cell reachable after recovery"
  else
    bad "C9: cell not reachable after recovery"
  fi
}
