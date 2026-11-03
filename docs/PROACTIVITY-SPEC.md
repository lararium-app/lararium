# PROACTIVITY-SPEC — fasti: crons, goals, and the heartbeat (P5)

Status: **FREEZE-PROPOSED (v1)** — merging this document freezes the design (issue #225;
PO directive 2026-11-02). Amendments follow the CUSTOS-SPEC append-only protocol.
Implementation is tracked separately and MUST land the V-P suite below as its exit criterion.

Companions: PENATUS-SPEC (§1.5 HEARTBEAT.md — already frozen; this spec implements it,
it does not amend it), SURFACE-SPEC (auth, session model, audit object), NUNTIUS-SPEC
(owner chats, flood buckets, card taps), CUSTOS-SPEC (cards, audit chain, scope rules),
DEVICE-RAILS-SPEC + CONNECTOR-LIFE-SPEC (SSE rail pattern, mint-on-win precedent),
ARCHITECTURE §2.5 (the paragraph this realizes).

## Purpose

The v1 cut line is "text + browser + connectors + memory + proactivity" (ARCHITECTURE
§6). Proactivity is the last pillar with no code: today the agent only ever speaks when
spoken to. fasti is the scheduler inside hearthd — named in the component map (
ARCHITECTURE §2, "Go (in hearthd)") — that gives the hearth three standing capabilities:

1. **Crons** — user-defined recurring jobs (5-field cron + intervals) that fire a
   headless model turn and may announce the result to the owner's channel.
2. **Goals** — multi-turn, checkpointed objectives with a requirement-checked close.
3. **Heartbeat** — the PENATUS-SPEC §1.5 contract, executed: periodic wake against the
   user's HEARTBEAT.md checklist.

Everything fires through the one turn door the daemon already has — the same code
paths a Telegram message travels — so approvals, redaction, budgets, and persistence
apply to proactive turns exactly as they do to reactive ones. Proactivity changes
*who knocks*; it does not change *who answers*.

## Inventory (verified on main, 2026-11-02)

- ALREADY: headless turn entry `Hub.RunHeadlessTurn(ctx, sessionID, text, channel,
  updateID, onDelta)` — internal/surface/turns.go:491. Applies TurnTimeout, shares the
  one-turn-per-session gate (`ErrTurnInFlight`), keeps approvals + persistence + secret
  needle scrubbing (`Hub.SetSecrets`, turns.go:88) unchanged.
- ALREADY: one-shot wake scheduler `internal/loop/schedule.go` — `Scheduler` (:225),
  persisted wakes `<home>/wakes.json` (`WakesFile()`, :267), `Clock`/`Timer` injection
  with `FakeClock` (:50-155), offline replay with `reason:"slipped_while_offline"`
  audit events (:290+, fireWake :527-566), caps 1s..1yr, `MaxPendingWakes=50` (:22-26),
  model tool `ScheduleWakeTool` (:426), `TurnRunnerFunc` (:194).
- ALREADY: wake→turn wiring precedent: `attachWakes` routes due wakes through
  `RunHeadlessTurn(..., "heartbeat", ...)` — cmd/hearthd/main.go:832-837. The
  `"heartbeat"` src-channel string exists; fasti owns it from here.
- ALREADY: HEARTBEAT.md format law — PENATUS-SPEC §1.5: user-authored ("agent may
  propose diffs, never self-edit"), keys `penatus`, `updated`, `cadence`
  (`hourly|daily|off`, default `daily`), `quiet` (e.g. `22:00-07:00`, user TZ), body =
  checklist, **empty body = heartbeat off**. NOT injected into chat turns
  (internal/loop/prompt.go:60-63 — "the heartbeat loop reads it separately").
- ALREADY: SSE rail pattern — custody card stream (internal/surface/custos_cards.go:390,
  5s `: ping` via `custosHeartbeatInterval` :143, per-client 64-slot overflow-drop, no
  Last-Event-ID) cloned by connectors_events.go:11-134 and devices_events.go:110.
- ALREADY: nuntius outbound — gated `SendMessage` (internal/nuntius/bridge.go:321)
  behind per-chat token bucket + global 429 pause (bucket.go:141; pauseOn429 :361),
  owner-chat set via `rememberOwnerChat` (:945), generic `replyText` primitive (:961).
  There is NO agent-initiated proactive send path yet; every call site sits inside
  update handling.
- ALREADY: config law — `lararium.yaml` decodes whole with `KnownFields(true)`
  (cmd/hearthd/main.go:92-100), comment at :30 names fasti as the reason ("silent
  config drift is how daemons start doing surprise things"). No `fasti:` block exists.
- ALREADY: no cron library anywhere (go.mod verified) — all timing is stdlib
  `time.AfterFunc`/`Ticker` behind the injected `Clock`.
- ALREADY: mint-on-win standing-rule seam — `ResolveScopeVia` scope=="connector"
  (internal/surface/approvals.go:622-655) through `AddAlwaysRule`
  (cmd/hearthd/policysrc.go:194-206); `HandleApproval` rejects every other scope
  value (turns.go:785-789).
- ALREADY: lock law — no network I/O under a held mutex (docs/AUDIT-LOCKS.md:4);
  cross-process flock order backup→custos→keys (internal/backup/lock.go:14-45);
  `Hub.TrySingleWriterLock` / RLock-held turns (turns.go:61-77, :600).
- ALREADY: atomic store law — temp+rename+fsync `writeSyncTempNamed`, 0600
  (internal/custos/vault.go:36-68); per-hearth dir registrations (`sessions/`,
  `wakes.json`, `nuntius/`, `custos/`, `devices/<id>/`) all follow it.
- GAPS: no cron grammar parser; no job store; no goals anywhere; no heartbeat loop;
  no quiet-hours or noise-budget code; no proactive nuntius send; no `/v1/fasti/*`
  surface; no `fasti:` config block.

## Decisions (design freeze)

**D1 — fasti is a package, and it absorbs the wakes scheduler.** New
`internal/fasti` holds the engine (clock, stores, jobs, goals, heartbeat, policy).
`internal/loop/schedule.go` MOVES into it as `fasti.Wakes` with the exported API
preserved (`Scheduler`, `Schedule`, `ScheduleWakeTool`, `Clock`, `FakeClock`, caps);
`internal/loop` keeps deprecated aliases for one minor version (deprecation-window
policy, ARCHITECTURE §3), then they are removed at the next MAJOR. Rationale: one
clock, one wakes.json, one offline-replay law — ARCHITECTURE already draws the line.
The wake path keeps firing through `TurnRunnerFunc` (schedule.go:194) exactly as
`attachWakes` wires it today (main.go:832-837).

**D2 — every fasti firing is one headless turn through the hub door.** Jobs, goals,
and the heartbeat start work ONLY via
`Hub.RunHeadlessTurn(ctx, sessionID, prompt, channel, 0, nil)` (turns.go:491). No
fasti code assembles a `loop.Session` directly (that path is the REPL's, repl.go:98).
Consequences by construction: approvals, audit fallback, compaction, per-turn model
routing, TurnTimeout, and secret-needle scrubbing apply unchanged. New src-channel
values fasti may stamp on `msg` events: `cron`, `goal`, `heartbeat` (extending the
precedent set `{"api","web","telegram","heartbeat"}`); the channel is an audit
attribute, never a capability.

**D3 — busy session law: skip, never queue.** A firing into a session that is already
in flight (`ErrTurnInFlight`, turns.go:24) does NOT block and does NOT queue: fasti
records a `fasti_fired` event with `{"status":"skipped","reason":"session_busy"}` and
moves on. A job that skips 5 consecutive due times pauses itself and raises one
notice (via D7) — a dead session must not be answered by a growing queue nor by
silence. fasti fires at most ONE turn globally at a time (own gate, mirroring the
single-writer discipline); user-originated turns always win contention: fasti backs
off and retries the firing at the next due time if the hub refuses.

**D4 — cron grammar: our own 5-field parser, no new dependency.** `M H DOM MON DOW`
supporting `*`, lists (`1,15`), ranges (`9-17`), and steps (`*/5`, `1-30/2`) —
standard Vixie-cron semantics, DOM/DOW OR-joined when both restricted. Interval jobs
are the simple form (`every: 30m`), stored canonically as `{kind:"interval"|"cron"}`.
Next-due is computed in the hearth's LOCAL zone (`fasti.local_timezone`, a
Go-parseable IANA name, default from system) so "07:30 every morning" survives DST:
a due time that falls inside a spring-forward gap is skipped with a
`fasti_fired{status:"skipped",reason:"dst_gap"}` event; repeated times in a
fall-back overlap fire once (monotonic next-due, never re-derive from wall clock
alone). Borrowing a cron lib is rejected under the build-vs-borrow ledger (ARCHITECTURE
§5 — own the decision paths): the grammar is ~200 lines plus a fuzz corpus.

**D5 — job store is the source of truth; the model creates jobs only through a card.**
`<hearth>/fasti/jobs.json` (0600, `writeSyncTempNamed`, single writer = the fasti
engine goroutine). Job row: `{id (fasti NewID, crockford ULID-style, penatus/session.go:19),
name, schedule, prompt, session (new-per-run "cron-<id>" or bound session id),
announce (channel target, D7), quiet (per-job override), enabled, created_by,
last_fire, next_due, consecutive_skips}`. `created_by ∈ {"user","model:<session>"}`.
User-created jobs arrive via CLI/surface (D8). A MODEL-created job goes through a
`fasti_job` tool that only REGISTERS a proposal as a custody-style card
(`registerCustosNamed` pattern, approvals.go:256) titled with schedule + prompt +
announcer; the job lands disabled and is enabled only by an allow verdict —
mirroring the "standing orders are the human's control surface" law of HEARTBEAT.md
(§1.5) and the device auto-registration precedent in reverse (adding standing
agency needs a card; reading a rail does not). Card verdicts settle over the same
`ResolveFromVia` chain (approvals.go:556). Audit: penatus events
`fasti_job_created/paused/removed` (fields: id, name, schedule, created_by — never
secrets, D9).

**D6 — heartbeat is the §1.5 executor, and only that.** On startup and each wake,
fasti reads `<hearth>/HEARTBEAT.md` fresh (no caching — user edits take effect at the
next wake): `cadence hourly|daily|off` (default `daily`; hourly fires on the hour,
daily at `fasti.daily_at` local, default 07:30); empty checklist body ⇒ off, per the
frozen contract. Each wake is ONE headless turn (D2, channel `heartbeat`) whose
operator-tier prompt is: the checklist lines (user-authored, trusted tier — the same
tier SOUL.md/MEMORY.md untagged lines hold, PENATUS-SPEC §1.4) + a fixed frame
instructing: evaluate each item, act where able, reply `HEARTBEAT_OK` and announce
nothing when there is nothing worth sending. The checklist NEVER rides in the system
prompt (prompt.go:60-63 keeps holding); it is turn input, and the turn is ordinary —
budgets, approvals, compaction, everything. Daemon NEVER writes HEARTBEAT.md; the
agent may propose diffs only as ordinary replies (control-surface law).

**D7 — announcements ride nuntius flood rails; noise budget caps the product.**
New exported send path: `Bridge.Announce(ctx, text)`, fanning out to the recorded
owner chats (`rememberOwnerChat`, bridge.go:945) through the gated `SendMessage`
(:321) — every existing flood bucket, 429 pause (:361), and token-scrubbing applies
unchanged; if nuntius is disabled, announces fall back to surface-events-only (D10)
and the penatus event stands as the record. Quiet hours: job `quiet` or heartbeat's
§1.5 `quiet` window suppresses FIRES (not bookkeeping): a cron due inside quiet is
deferred to the quiet-window end; if >12h of due-times accumulated at quiet's end
(likely a weeknight's worth), they collapse to a single catch-up firing per job —
one turn, most-recent prompt, `reason:"quiet_collapse"`. Noise budget: per UTC day,
fasti may Announce at most `fasti.max_announce_per_day` (default 8) messages; the
budget counts ANNOUNCEMENTS, not turns (thinking is free, spam is not). Overflow:
the announcement is dropped with `fasti_fired{status:"silenced",reason:"noise_budget"}`;
5 overflow-drop days in any 7 fires one digest notice and resets the streak. A
`[URGENT]`-style model override does NOT exist: the only bypass is an approval card,
which is interactive by nature and therefore not subject to quiet hours (a sleeping
user's phone buzzing for a verdict is the user's choice, already NUNTIUS law).
Dedup law: two announcements with identical normalized text (case/whitespace-folded,
first 200 chars) inside 24h → second is dropped `reason:"dedup"`.

**D8 — surface and CLI: state bootstrap + one SSE rail, cloned not invented.**
Bearer-token routes under the existing dispatch (server.go:60-104 pattern):
`GET /v1/fasti/state` (snapshot: jobs + goals + heartbeat status + noise counters —
the bootstrap, replacing Last-Event-ID law), `POST /v1/fasti/jobs`, `PATCH
/v1/fasti/jobs/{id}` (enable/disable/schedule), `DELETE /v1/fasti/jobs/{id}`, `POST
/v1/fasti/jobs/{id}/run` (fire-now, still D2/D3 law), `GET /v1/fasti/events` (SSE:
`fasti_event` full-row upserts for jobs/goals/heartbeat, cloned from
connectors_events.go — 5s ping, 64-slot overflow-drop, no resume). CLI over the
existing `hearthd.sock` line protocol (socket.go:103-137, new verb family `FASTI
<sub> <args>` with the 4096-byte line cap and ack-then-progress law): `hearthd job
add|list|pause|resume|remove|run`, `hearthd goal ...`. Every store mutation flows
through the engine's command channel — the CLI never writes jobs.json (single-writer, D5).

**D9 — secrets and injection posture.** Job prompts are validated at create time
against the same needle set the scrubber uses (`Hub.SetSecrets`, turns.go:88):
literal vault values / key material in a prompt are REFUSED with a pointer to the
`${vault:NAME}` doctrine (CONNECTOR-LIFE D1). Prompts are stored verbatim otherwise
— they are user-tier text, and they are USER-authored or card-approved (D5), which
is what makes them trusted input. Goal checkpoint prose (D10) is model-authored and
therefore re-injected ONLY inside a data-tier frame, mirroring the MEMORY-DERIVED
split (PENATUS-SPEC §1.4): the frame is fixed daemon text, checkpoint prose is
escaped (tag-openings collapsed) before it enters the prompt. A cron prompt is
immutable while enabled (edit = disable → edit → re-approve if created_by=model) so
"what runs at 3am" is always auditable to what a human approved.

**D10 — goals: checkpointed, budgeted, requirement-checked, honest.**
`<hearth>/fasti/goals.json` + one journal per goal `<hearth>/fasti/goals/<id>.md`
(markdown, human-readable per principle 2 — the file layer is the product's memory):
frontmatter `{id, title, status: active|paused|done|failed|stale, requirements:
[{text, done, evidence_seq?}], turn_count, turn_budget, session_id, next_hint}`.
Lifecycle: `hearthd goal new "<title>" --requirement "<r>" [--budget N turns]` (default
12). Each goal step is one headless turn (D2, channel `goal`) whose prompt = journal
rendering + "advance the goal by one concrete step". The model may close a goal only
by citing event `Seq` numbers from the session log as evidence per requirement
(appending to the log is cheap; the citation makes laziness checkable); fasti verifies
the citations exist and post-date goal creation — it does NOT grade their quality,
and the closing turn's announcement always says `closed by agent, evidence seqs …`
so a human audits it. Budget exhaustion ⇒ status `stale` + one notice; `stale` goals
auto-fail (status `failed`) after 30 days untouched — no zombie objectives. Goals run
only when their session is idle (D3) and never concurrently with each other.
Goal events (penatus kind, `kind:"goal"` on the fasti rail): `goal_created`,
`goal_stepped`, `goal_closed`, `goal_staled`, `goal_failed`.

**D11 — config block, all-or-nothing like everything else.** `lararium.yaml` gains:

```yaml
fasti:
  enabled: true          # default false until a user opts in? NO — see below
  local_timezone: ""     # IANA name; "" = system zone
  heartbeat:
    daily_at: "07:30"    # used when HEARTBEAT.md cadence=daily
  max_announce_per_day: 8
  catchup_window: 12h    # D7 quiet-collapse threshold
```

`enabled` DEFAULTS **true**. The subsystem is inert until the user gives it standing
orders — heartbeat is already off-by-default upstream (missing or empty HEARTBEAT.md
= off, §1.5), and with no jobs and no goals fasti fires nothing. The block decodes
under `KnownFields(true)` (main.go:99): a typo'd key refuses boot, matching the
comment at main.go:30 that names fasti as the cautionary lesson.

**D12 — audit trail: penatus now, custody only if standing grants ever appear.**
All fasti lifecycle and firing events are penatus session/daemon events (precedent:
`schedule_created`/`schedule_fired`, schedule.go:356, :557), carried verbatim on the
`GET /v1/fasti/events` rail (D8). fasti v1 mints NO custody scope rules: the
mint-on-win seam (approvals.go:622-655) and `AddAlwaysRule` (policysrc.go:194) stay
connector-scoped; if a future slice wants "this cron may always send mail", that is
a new `scope=="job"` handler and a CUSTOS-SPEC closed-set amendment — named here so
it can't arrive unannounced. Daemon logs (stderr) scrub like every other line; no
new lock crosses the backup→custos→keys order (D1 stores are engine-goroutine-local,
no cross-process flock needed; fasti takes NO lock while firing — all firing happens
outside every held mutex per AUDIT-LOCKS law by construction, since RunHeadlessTurn
is called from the engine's own goroutine with no fasti mutex held).

## Requirements

- **R1**: All time reads and timer creation in fasti go through the injected
  `Clock` (schedule.go:50); production wires `realClock`, tests `FakeClock`. No
  `time.Now` in package fasti (CI grep gate).
- **R2**: Job/goal/heartbeat stores survive daemon restart with next-due preserved;
  writes are `writeSyncTempNamed` 0600 (vault.go:36-68).
- **R3**: A firing never blocks: `RunHeadlessTurn` is called from the engine
  goroutine; ErrTurnInFlight → skip event (D3); provider/network latency can never
  stall cron advancement for other jobs (timer re-arm happens before turn start).
- **R4**: Enabling a model-proposed job without an allow verdict is impossible;
  the proposal card carries schedule + prompt text; audit shows `created_by`.
- **R5**: Quiet windows suppress announcements AND turn-firings for jobs that opt
  in (`quiet: defers`) — heartbeat always defers; a deferred firing executes within
  60s of the window end.
- **R6**: Noise budget overflow drops announcements only; the underlying turn still
  executes and its events persist; budget state is visible on `/v1/fasti/state`.
- **R7**: A goal cannot reach `done` without per-requirement evidence seqs that
  exist in the goal session's log and post-date `goal_created`.
- **R8**: HEARTBEAT.md is never written by the daemon; cadence/quiet changes take
  effect at the next wake with no restart.
- **R9**: The `fasti:` block with an unknown key refuses boot; absent block = D11
  defaults.
- **R10**: No new process, port, or third-party dependency. SSE rail clones the
  frozen pattern (no Last-Event-ID; bootstrap = state GET).

## Exit criterion (V-P suite, all in one CI job `proactivity`)

- **V-Pa (grammar + clock law)**: FakeClock table — cron grammar corpus (lists,
  ranges, steps, DOM/DOW OR, DST gap/overlap cases per D4) asserts exact next-due
  sequences across a spring-forward and fall-back boundary; CI grep proves no
  `time.Now` in `internal/fasti`.
- **V-Pb (store + replay law)**: jobs+goals+heartbeat across daemon restart: due
  while offline fires EXACTLY once with `slipped_while_offline` (wake precedent,
  schedule.go:290), future dues preserved; kill -9 mid-write leaves a parseable
  store (temp+rename).
- **V-Pc (door law)**: cron firing executes a real turn through the hub: approval
  card raised mid-turn settles from the surface; tool calls see the same audit
  object as an api-channel turn; a session-busy fire records skip + 5th skip
  self-pauses and notices (D3).
- **V-Pd (heartbeat law)**: fixture HEARTBEAT.md — empty body = no fires; cadence
  flip takes effect next wake with no restart; wake prompt contains checklist and
  the fixed frame; `HEARTBEAT_OK` produces zero Announce calls; daemon never
  writes the file (inotify/mtime assert).
- **V-Pe (quiet + noise law)**: fake-clock quiet window — deferral, collapse at
  >catchup_window, budget drop with event, digest notice on day 5, dedup drop;
  every drop is visible on `/v1/fasti/state` and the rail.
- **V-Pf (proposal-card law)**: model `fasti_job` call → card with schedule+prompt;
  allow → job enabled; deny/timeout → job absent or disabled; audit rows carry
  `created_by=model:<session>` and verdict via; card text never appears in logs.
- **V-Pg (goal law)**: budget-12 goal closed by evidence seqs (R7 pass) and by fake
  seqs (R7 refuse, stays active); budget exhaustion → `stale` + exactly one notice;
  30-day stale → `failed`.
- **V-Ph (secret + injection law)**: job prompt containing a live vault value →
  create refused (D9); goal checkpoint prose containing a forged tag-open is
  escaped in the rendered next-step prompt (needle + alignment check, V-T1
  suite technique); `${vault:NAME}` passes through as literal reference only.
- **Vi (wire compatibility)**: existing wake path (`schedule_wake` tool,
  wakes.json, `attachWakes`) passes its current tests UNMODIFIED against
  `fasti.Wakes` aliases; `/v1/fasti/*` is additive; `lararium.yaml` without the
  block boots with D11 defaults (backup/upgrade job re-runs green).

## Out of scope

- Goals-as-parallel-agents, multi-goal scheduling heuristics, and requirement
  grading (permanent for v1: laziness is checked by citation, judgment by humans);
  nuntius-side job admin (Telegram inline keyboards) — CLI + surface first; the web
  UI screen (surface rail lands first, panel later); cross-daemon or distributed
  cron (single node, principle 3); wake-up alarms that bypass quiet hours by
  category; natural-language schedule parsing (the model proposes cron, D5, humans
  approve); per-job model selection (router profiles, not fasti's business);
  standing grants / `scope=="job"` (needs a CUSTOS-SPEC amendment first, D12);
  HEARTBEAT.md format change (frozen upstream, §1.5); anything touching the Android
  client.

## Changelog
- v1 (2026-11-02): initial freeze text per issue #225; PO directive 2026-11-02 (P5
  proactivity selected as the next daemon track after T3/v1.0.0-beta6).
