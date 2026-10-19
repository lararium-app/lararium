# Lock discipline audit (beta criterion D)

Date: 2026-10-19. Scope: all long-lived daemons (`hearthd`, `custosd`)
under `internal/`. Standard: no network I/O under a held mutex, except a
path with an explicit, bounded, documented waiver.

## Method

1. **Static sweep.** Line-level scan of every non-test file in
   `internal/` for network primitives (`http` round trips, socket
   `writeRaw`, MCP `Connect`/`tools/list`, Telegram `SendMessage`/
   `EditMessage`) inside a mutex-held window, tracking `Lock`/`RLock`
   against their `Unlock`/`RUnlock` (defer and inline).
2. **Manual review of every hit** (heuristic windows are deliberately
   conservative; all initial candidates resolved to false positives on
   reading — locks released before the I/O line).
3. **Cross-function lock propagation** reviewed by hand for the one
   known case (below); scanners do not follow call graphs.
4. **Race detector.** `go test -race ./...` on `main`
   (`532b132`-lineage, post #111/#112): full suite green, zero data
   races (`cmd/hearthd` included; 90 s crash-matrix suite exercised).

## Findings

### Waived-safe (documented, bounded) — MCP connector start

`MCPConnector.startLocked` (and `startHTTPLocked`) run spawn + first
`tools/list` under `c.mu` **deliberately** (original audit: #87;
rationale annotated at the function). Bound analysis:

- every network step inside takes `ctx`, capped by `list_timeout`
  (default `DefaultMCPListTimeout`);
- the only unbounded step is `exec.CommandContext` spawn, which errors
  immediately for a missing binary;
- `teardownChildLocked` kills with a fixed `sigkillWait` grace and
  offloads the potentially blocking `session.Close()` to a goroutine;
- fan-out to door subscribers is dispatched on a fresh goroutine
  (`mcp_rail.go`), so no `c.mu → doors s.mu` cycle is possible while
  `c.mu` is held.

Cost: `MCP-STATUS` on the same connector can block up to `list_timeout`
while a start is in flight — accepted (status during start is transient;
rail-level status uses atomic pointers). Lock order is uniform
`refreshMu → c.mu` at all three acquisition sites (retry timer,
`Refresh`, `ManualRefresh`); no reverse-order path exists. `Refresh`
drops `c.mu` before the live network re-list. **Do not narrow this lock
without re-auditing restart-budget accounting.**

### Clean paths (verified this pass)

- `nuntius` — `turnQueue.startLocked` only spawns a worker goroutine;
  model calls run off-lock. `custodyFeed.pending` reads the dedupe map
  under `f.mu`, releases it, then sends to Telegram with retries under a
  30 s context (retry sleeps hold no lock).
- `custos/mcp_doors.go` — `railRegistry` returns the pointer under
  `RLock`; `broadcastMCPFrame` snapshots clients under `RLock`,
  releases, then enqueues frames. Door-socket writes never hold
  `DoorServer` locks.
- `custos/doors.go` — per-client writes go through the client's own
  serialized `writeRaw`/`enqueueEvent`, not the server mutex.
- `keystore` — file I/O under `flock` is intentional (cross-process
  durability): small file, atomic temp+fsync+rename in the same
  directory; local filesystem only, no network.

## Verdict

**PASS** — one waived-safe site (bounded by `list_timeout`, annotated in
code), zero unwaived network-under-lock paths, zero races under the
full suite. Re-run the race suite on any change to
`internal/custos/mcp_*.go` or `internal/nuntius/`.
