# C10 — uid map + binds (spec §8). (a) in-cell keeper uid == 1000;
# (b) a `cell run` payload seen on the HOST with uid == cell.json
# subuid_base + 1000; (c) keeper workspace write readable from the
# operator path with on-disk owner == the mapped uid.
#
# Mechanics (live-probed 2026-10-01): `cell run` = systemd-run
# --wait --pipe --machine=<id>; the payload lands in the HOST boot
# unit's cgroup with uid == subuid_base + guest uid. CGroupPath reads
# EMPTY for these transient units — derive the path from the unit
# name. `cell run` is synchronous, so the payload is backgrounded
# around the host-side walk; argv[0] is renamed for a deterministic
# match (no other sleep should exist, but name-matching is cheap).
# shellcheck shell=bash

check_c10() {
  local unit cgp base uid_h pid f runner _i found

  # (a) in-cell identity
  if [ "$(cellrun -- id -u keeper 2>/dev/null)" = "1000" ]; then
    ok "C10a: in-cell keeper uid == 1000"
  else
    bad "C10a: in-cell keeper uid != 1000"
    return
  fi

  base=$(sed -n 's/.*"subuid_base":[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
    "$ROOT/cells/$ID/cell.json")
  if [ -z "$base" ]; then
    bad "C10b: no subuid_base recorded in cell.json"
    return
  fi

  # (b) host-visible uid of a payload started via cell run.
  unit="lararium-cell-${ID}.service"
  cgp="/sys/fs/cgroup/system.slice/${unit}"
  if [ ! -d "$cgp" ]; then
    bad "C10b: unit cgroup dir not found: $cgp"
    return
  fi

  runner=""
  cellrun -- bash -c 'exec -a c10-sleep sleep 300' >/dev/null 2>&1 &
  runner=$!
  found=""
  for _i in $(seq 1 10); do
    while read -r p; do
      cl=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null)
      if [ "$cl" = "c10-sleep 300 " ]; then found=$p; break; fi
    done < <(find "$cgp" -name cgroup.procs -exec cat {} + 2>/dev/null)
    [ -n "$found" ] && break
    sleep 1
  done
  # Read the payload's uid BEFORE tearing down the runner: killing it
  # reaps the process and /proc/<pid> can vanish under us (flaky fail).
  pid=$found
  if [ -z "$pid" ]; then
    kill "$runner" 2>/dev/null
    wait "$runner" 2>/dev/null
    bad "C10b: payload not found under the unit cgroup"
    return
  fi
  uid_h=$(stat -c %u /proc/"$pid" 2>/dev/null)
  kill "$runner" 2>/dev/null
  wait "$runner" 2>/dev/null
  if [ "$uid_h" = "$((base + 1000))" ]; then
    ok "C10b: host-visible payload uid == subuid_base+1000 ($uid_h)"
  else
    bad "C10b: payload uid $uid_h != subuid_base+1000 ($((base + 1000)))"
  fi

  # (c) keeper write lands mapped on disk, readable via operator path.
  cellrun -- bash -c 'echo c10 > /workspace/c10.txt' >/dev/null 2>&1
  f="$ROOT/cells/$ID/workspace/c10.txt"
  if [ ! -r "$f" ]; then
    bad "C10c: keeper-written file not readable via operator path"
    return
  fi
  uid_h=$(stat -c %u "$f")
  # MUST equal subuid_base+1000 exactly: "not 0 and not operator" also
  # passes when userns mapping is broken and raw 1000 or nobody lands.
  if [ "$uid_h" = "$((base + 1000))" ]; then
    ok "C10c: workspace write owner is mapped uid $uid_h (subuid_base+1000)"
  else
    bad "C10c: workspace write owner uid $uid_h != mapped $((base + 1000))"
  fi
}
