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
ARCHITECTURE §2.5, the Go component living in hearthd per §2) — that gives the hearth
three standing capabilities:

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
  5s `: ping` via `custosHeartbeatInterval` :143, per-client 64-slot buffer whose
  overflow DISCONNECTS the client, no
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
`Hub.RunHeadlessTurn(ctx, sessionID, prompt, channel, 0, onDelta)` (turns.go:491),
where onDelta is a per-firing text accumulator. No
fasti code assembles a `loop.Session` directly (that path is the REPL's, repl.go:98).
**Turn-output law**: the accumulated delta text is the ONLY announcement source —
but shipping is routed, never blanket (announce-routing law, D7): a firing
announces only if it EXPECTS to (crons: `announce` set; goals: the closing turn
only; heartbeat: accumulator not `HEARTBEAT_OK`). An un-announced firing's
accumulator still feeds audit summaries, it just never reaches `Bridge.Announce`;
the heartbeat's `HEARTBEAT_OK` verdict (D6) is evaluated on the accumulator; the
session log's `msg` events
(persisted unconditionally) are the durable fallback when the accumulator comes up
empty (mid-turn cancel). A nil-onDelta firing that expected to announce is a
defect, not a mode.
Consequences by construction: approvals, audit fallback, compaction, per-turn model
routing, TurnTimeout, and secret-needle scrubbing apply unchanged. New src-channel
values fasti may stamp on `msg` events: `cron`, `goal`, `heartbeat` (extending the
precedent set `{"api","web","telegram","heartbeat"}`); the channel is an audit
attribute, never a capability.

**D3 — busy session law: skip, never queue; two busy reasons, one auto-pause.**
fasti runs TWO goroutines: a command loop (timer arming, store mutations from
CLI/surface/verdict hooks, state reads) and a single firing worker (the global
one-turn gate lives here). `RunHeadlessTurn` runs ONLY on the worker; the command
loop never executes a turn; results return via a buffered completion channel.
A firing into a session that is already in flight (`ErrTurnInFlight`, turns.go:24)
does NOT block and does NOT queue: the worker records a `fasti_fired` event with
`{"status":"skipped","reason":"session_busy"}` and moves on. Gate contention is
detected AT DISPATCH by the command loop (the worker is a mailbox with depth 1):
if the worker is occupied when a due-time arrives, the loop records
`reason:"engine_busy"` and arms the retry on the LOOP's timers (+60s, loop-owned
per R3 — the worker never schedules anything, it owns no mailbox-drain beyond its
current turn), and only a retry that still lands on session_busy counts as a
skip. Two distinct
reasons, because only session_busy feeds the auto-pause: a job that skips 5
consecutive due times pauses itself and raises one notice (via D7) — a dead
session must not be answered by a growing queue nor by silence, but a busy
neighbor is no crime. User-originated turns always win contention: fasti backs
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
announce (channel target, D7), quiet (object override: `{window,
mode}` — window `"HH:MM-HH:MM"` local, inclusive start, exclusive end, may span
midnight; mode `defers` (no firing during window; firings collapse per D7) or
`announce_only` (firings run, announcements suppressed); default
`announce_only`), enabled, created_by,
last_fire, next_due, consecutive_skips}`. `created_by ∈ {"user","model:<session>"}`.
User-created jobs arrive via CLI/surface (D8). A MODEL-created job goes through a
`fasti_job` tool — registered unconditionally alongside `schedule_wake` (the
precedent for daemon-registered loop tools, schedule.go:426); the tool only ever
PROPOSES, there is no direct-write variant — that REGISTERS a proposal as a
custody-style card
(`registerCustosNamed` pattern, approvals.go:256) titled with schedule + prompt +
announcer; the proposal does NOT travel the turn-card REST path — `HandleApproval`
rejects any scope but `"connector"` (turns.go:784-787), and fasti cards carry no
scope: like custody cards they settle through the daemon-registered verdict hook on
the `ResolveFromVia` chain (approvals.go:556), which posts the verdict into the
engine command channel (D8). The job row is CREATED `enabled:false` at proposal
time (the card references its stable id); an allow verdict flips it to enabled, a
deny REMOVES it, and a card timeout removes it with its own
`fasti_job_timeout` audit event — one terminal state per outcome, never an
ambiguous residue — mirroring the "standing orders are the human's control
surface" law of HEARTBEAT.md
(§1.5) and the device auto-registration precedent in reverse (adding standing
agency needs a card; reading a rail does not). Audit: penatus events
`fasti_job_created/enabled/paused/resumed/removed/timeout` (fields: id, name,
schedule, created_by, and for cards the settling via — never
secrets, D9).

**D6 — heartbeat is the §1.5 executor, and only that.** On startup and each wake,
fasti reads `<hearth>/HEARTBEAT.md` fresh (no caching — user edits take effect at the
next wake): `cadence hourly|daily|off` (default `daily`; hourly fires on the hour,
daily at `fasti.heartbeat.daily_at` local, default 07:30); empty checklist body ⇒
off, per the frozen contract. Each wake is ONE headless turn (D2, channel
`heartbeat`, sessionID constant `"heartbeat"` — one durable log of all wakes,
matching the existing wake path's channel name) whose
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
unchanged; if nuntius is disabled, announces fall back to surface-events-only (D8)
and the penatus event stands as the record. Quiet hours (schema per D5; heartbeat's
§1.5 `quiet` always runs in `defers` mode): the baseline window is
`fasti.quiet_hours` (D11; "" = no baseline), inherited by every job and goal
unless the job's `quiet` object overrides it; a window suppresses ANNOUNCEMENTS for
every fasti source, and suppresses FIRINGS in `defers` mode — a deferred cron due
inside the window is rescheduled to the quiet-window end, where each job collapses
to EXACTLY ONE firing (the most-recent due prompt, `reason:"quiet_collapse"`);
due-times older than `fasti.catchup_window` (default 24h) are not replayed at all
— discarded with one `fasti_fired{status:"skipped",reason:"stale_discard"}` per
job. A 9-hour weeknight therefore collapses, never floods. Noise budget: per UTC day,
fasti may Announce at most `fasti.max_announce_per_day` (default 8) messages; the
budget counts ANNOUNCEMENTS, not turns (thinking is free, spam is not). Overflow:
the announcement is dropped with `fasti_fired{status:"silenced",reason:"noise_budget"}`;
5 overflow-drop days in any 7 fires one digest notice and resets the streak.
Operational notices (the 5-skip auto-pause notice, this digest) are DAEMON-authored
fixed templates, not announcements: they bypass the noise budget and quiet hours
(a hearth that silenced its own alarm clock cannot be trusted with alarm clocks),
never count against the budget, and are auditable per type; no model text rides in
them (names + counts only). A
`[URGENT]`-style model override does NOT exist: the only bypass is an approval card,
which is interactive by nature and therefore not subject to quiet hours (a sleeping
user's phone buzzing for a verdict is the user's choice, already NUNTIUS law).
Dedup law: two announcements with identical normalized text (case/whitespace-folded,
first 200 chars) inside 24h → second is dropped `reason:"dedup"`. All noise state —
per-UTC-day announce counts, the 7-day overflow-day streak, the dedup window, and
per-type operational-notice history — persists in `<hearth>/fasti/state.json`
(`writeSyncTempNamed` 0600, written by the command loop on every announce event
and UTC-day rollover): a daemon restart must not launder the budget away.

**D8 — surface and CLI: state bootstrap + one SSE rail, cloned not invented.**
Bearer-token routes under the existing dispatch (server.go:60-104 pattern):
`GET /v1/fasti/state` (snapshot: jobs + goals + heartbeat status + noise counters —
the bootstrap, replacing Last-Event-ID law), `POST /v1/fasti/jobs` body
`{name, schedule:{kind:"cron",expr:"..."}|{kind:"interval",every:"30m"}, prompt
(≤1000 runes, single-paragraph; longer prompts refused), session?, announce?,
quiet?}` → the job row (validation errors: 400 + reason, matching wire discipline),
`PATCH /v1/fasti/jobs/{id}` (enable/disable/schedule/`prompt?` — a prompt update
while `enabled:true` is rejected 400 per D9's edit workflow),
`DELETE /v1/fasti/jobs/{id}`,
`POST /v1/fasti/jobs/{id}/run` (fire-now, still D2/D3 law),
`POST /v1/fasti/goals` (body = D10 `goal new` fields),
`PATCH /v1/fasti/goals/{id}` (status ∈ {paused, active}, budget),
`POST /v1/fasti/goals/{id}/close {mode:"user_force"}` (the only REST close path;
the agent close path lives in-loop under R7),
`GET /v1/fasti/events`
(SSE: `fasti_event` full-row upserts for jobs/goals/heartbeat, cloned from
connectors_events.go — 5s ping, 64-slot buffer whose overflow DISCONNECTS the
client, no resume). CLI over the
existing `hearthd.sock` line protocol (socket.go:103-137): mutations as
`FASTI {"verb":"add",...payload}` — ONE JSON line inside the 4096-byte cap (the
1000-rune prompt limit keeps every legal job inside it; over-cap payloads refuse
with `ERR payload_too_large`); `FASTI list` streams one JSON row per line until
EOF (rows fit the cap by the same limit); ack-then-progress law unchanged: `hearthd
job add|list|pause|resume|remove|run`, `hearthd goal ...`. Every store mutation
flows through the command loop's channel — the CLI never writes jobs.json
(single-writer, D5), and the loop services commands while the worker runs turns
(D3), so a 5-minute turn never blocks `hearthd job list`.

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
[{text, done, evidence: [seq...]}], turn_count, turn_budget, step_after (duration,
default 15m), session_id, next_hint, created_at, created_seq}`.
Lifecycle: `hearthd goal new "<title>" [--session S] --requirement "<r>" [--budget
N turns]` — session defaults to `goal-<id>`, `created_at`/`created_seq` (the
session log high-water at creation) are stamped by fasti; one goal, one durable
log. `hearthd goal list` prints id/status/turn_count/budget/next-step;
`hearthd goal pause|resume|cancel <id>` set status (`failed` on cancel). Each goal
step is one headless turn (D2, channel `goal`) whose prompt = journal rendering +
the legal citation window `(created_seq, now]` + "advance the goal by one concrete
step". The model reports through two tools,
registered alongside `schedule_wake` (the precedent for daemon-registered loop
tools, schedule.go:426):
`fasti_goal_step {next_hint, progress_prose, satisfied?: [req_index]}` and
`fasti_goal_close {}`.
**Step-window law** (the evidence machinery, machine-bound by construction):
fasti stamps each step turn with `window_start` (session log high-water at turn
start) and `window_end` (high-water at turn end); evidence seqs are NEVER
model-supplied — `fasti_goal_step` attaches `window_start+1..call_seq` to EACH
`satisfied` index (one requirement per step call: closing k requirements takes k
step calls; zero steps close nothing), records `progress_prose` into the journal
as escaped checkpoint data, writes `next_hint`, and chains the next step at
`completion + step_after`. `fasti_goal_close` closes ONLY when every requirement
is `done` with non-empty recorded evidence (R7: those seqs exist in the goal
session's log and post-date `created_seq` — fasti checks existence, it does NOT
grade their quality); a close attempt that fails this is refused with
`goal_close_refused{reason:"evidence"}` and the goal stays `active`. On pass:
status `done`, and the closing turn's announcement always says
`closed by agent, evidence seqs …` so a human audits it.
`hearthd goal close <id>` (user tier) closes regardless of evidence, audit
`goal_closed{mode:"user_force"}` — user-tier authority is absolute over its own
goals (SURFACE D5 doctrine); the mode field is how the difference stays visible.
Stepping chain: a step completing with status still `active`
schedules the NEXT step `step_after` later (D3 law applies — a busy session defers
the chain, it does not burn a step); `turn_count` increments ONLY when a step
actually starts a turn (a skip reschedules, it does not charge budget); the chain ends only at
done/failed/stale/paused — no daily cap while budget remains.
Budget exhaustion ⇒ status `stale` + one notice; `stale` goals
auto-fail (status `failed`) after 30 days untouched (checked at boot and on each
step) — no zombie objectives.
Goals run never concurrently with each other (worker gate, D3).
Goal events (penatus kind, `kind:"goal"` on the fasti rail): `goal_created`,
`goal_stepped`, `goal_closed`, `goal_close_refused`, `goal_staled`, `goal_failed`.

**D11 — config block, all-or-nothing like everything else.** `lararium.yaml` gains:

```yaml
fasti:
  enabled: false         # default off; flipping to true is the opt-in act (R9)
  local_timezone: ""     # IANA name; "" = system zone
  quiet_hours: ""        # daemon-wide baseline window "HH:MM-HH:MM" (local);
                         # "" = no baseline; jobs/goals inherit unless overridden
  heartbeat:
    daily_at: "07:30"    # used when HEARTBEAT.md cadence=daily
  max_announce_per_day: 8
  catchup_window: 24h    # D7: due-times older than this are discarded, not replayed
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
new lock crosses the backup→custos→keys order (D1 stores are command-loop-local,
no cross-process flock needed; fasti takes NO lock while firing — the firing worker
calls RunHeadlessTurn holding no fasti mutex, and the command loop never calls it
at all (D3), keeping AUDIT-LOCKS law true by construction).

## Requirements

- **R1**: All time reads and timer creation in fasti go through the injected
  `Clock` (schedule.go:50); production wires `realClock`, tests `FakeClock`. No
  `time.Now` in package fasti (CI grep gate).
- **R2**: Job/goal/noise-state stores survive daemon restart with next-due and
  budget counters preserved; writes are `writeSyncTempNamed` 0600 (vault.go:36-68).
- **R3**: A firing never blocks anything but another firing: `RunHeadlessTurn`
  executes only on the firing worker (D3); ErrTurnInFlight → skip event; provider
  or network latency inside one firing can never stall cron advancement, command
  processing, or the SSE rails (timer re-arm and command servicing live on the
  loop, independent of the worker).
- **R4**: Enabling a model-proposed job without an allow verdict is impossible;
  the proposal card carries schedule + prompt text; audit shows `created_by`.
- **R5**: Quiet windows (grammar per D5) always suppress announcements, and
  suppress+defer turn-firings in `defers` mode (heartbeat always); a deferred
  firing DISPATCHES within 60s of the window end and executes as soon as the
  worker gate is free (D3 law: deferral is dispatch-bounded, execution is
  gate-ordered); collapse and stale-discard behave per D7.
- **R6**: Noise budget overflow drops announcements only; the underlying turn still
  executes and its events persist; budget state is visible on `/v1/fasti/state`.
- **R7**: The daemon refuses an agent closing a goal `done` unless every
  requirement carries recorded step-window evidence (seqs from the goal session's
  log, post-dating `created_seq`); evidence is fasti-stamped, never model-supplied
  (D10). User-tier force-close is always available and audited distinctly.
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
- **V-Pe (quiet + noise law)**: fake-clock quiet window — deferral, per-job
  single-firing collapse, `stale_discard` past catchup_window, budget drop with
  event, digest notice on day 5 (and exempt from the budget it reports), dedup
  drop; budget survives restart (state.json);
  every drop is visible on `/v1/fasti/state` and the rail.
- **V-Pf (proposal-card law)**: model `fasti_job` call → job row created
  `enabled:false` + card carrying schedule+prompt referencing its id; allow →
  enabled; deny → row REMOVED with `fasti_job_removed{via:"deny"}`; timeout →
  removed with `fasti_job_timeout`; the card settles through the verdict hook (no
  scope field; a resolve attempt through the turn-card REST path is rejected —
  scope law, turns.go:784); audit rows carry `created_by=model:<session>`; card
  text never appears in logs.
- **V-Pg (goal law)**: budget-12 goal closed only after every requirement has
  step-window evidence (R7); a close attempt with any requirement lacking
  evidence → `goal_close_refused`, status stays `active`; evidence seqs are
  fasti-stamped (no tool path accepts a model-supplied seq); `hearthd goal close`
  force-closes with `mode:"user_force"` in audit; step chain: busy-session step
  defers without burning budget (D3), next step fires `step_after` after
  completion; budget exhaustion → `stale` + exactly one notice; 30-day stale →
  `failed`.
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
- v1 Rev-1 (2026-11-02): G1 round-1 amendments (10 of 11 blocking folded; F13
  disproven — `ResolveScopeVia` exists, approvals.go:574): turn-output capture via
  onDelta (D2); command-loop/firing-worker split + `engine_busy` reason (D3/R3);
  card settlement off the scope-rejecting REST path + one terminal state per
  verdict (D5/V-Pf); quiet-window schema `mode∈{defers,announce_only}` resolving
  the D7/R5 conflict (D5/R5); per-window collapse with 24h stale-discard (D7);
  operational notices budget-exempt (D7); noise state persisted (D7/R2/V-Pe);
  socket JSON payload law + 1000-rune prompt cap (D8); SSE overflow wording
  corrected to disconnect (D8/inventory); goal stepping tools + chain + session
  default (D10/V-Pg); `fasti.heartbeat.daily_at` path (D6/D11); POST body schema
  (D8); minors F9/F12/F14/F15 folded.
- v1 Rev-2 (2026-11-02): G1 round-2 amendments (5 blocking, 3 minor, all folded):
  announce-routing law — accumulator ships only when the firing expects to
  announce (D2); `engine_busy` contention detected at dispatch by the command
  loop, retries armed on loop timers (D3); goal evidence rebuilt as the
  **step-window law** — fasti-stamped windows, one requirement per step call,
  `fasti_goal_close {}` verifies, user force-close audited distinctly,
  `created_at`/`created_seq` added (D10/R7/V-Pg); `fasti.quiet_hours` baseline
  added to D11 and named as the inherited window (D7); PATCH `prompt?` gated on
  disabled (D8); goal REST routes (D8);
  `fasti_job_enabled/resumed` + close-mode added to audit sets (D5/D10).
