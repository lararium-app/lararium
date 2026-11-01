# DEVICE-RAILS-SPEC — device rails: phone as paired push client (T3)

Status: **FREEZE-PROPOSED (v2)** — merging this document freezes the design (issue #217;
PO directive 2026-11-01). Amendments follow the CUSTOS-SPEC append-only protocol.
Implementation is tracked separately and MUST land the V-T3 suite below as its exit criterion.

Companions: MCP-SPEC (transport, discovery, flags), CONNECTOR-LIFE-SPEC (lifecycle verbs, store,
state rail), CUSTOS-SPEC (cards, doors, audit), SURFACE-SPEC (phone surface, bearer gate, pairing).

## Purpose

T3 (ROADMAP, external): first-party device rails exposing phone capabilities (`calendar`, `contacts`,
`notifications`, `calllog`, `health`, `dialer`, `sms`) to the hearth. The phone pairs as an ordinary HTTPS
client over its existing authenticated surface session, pushing rail data and keeping a single live SSE stream
open for action dispatch, while the hearth materializes the device as an in-process connector (`kind=device`).
This specification freezes design for device registration, bearer identity, rail tool grammar, card-gated
action approvals, status and offline semantics, live streaming, push sync, rail data at rest, rail toggles,
and audit logging without VPN/Tailscale dependencies, inbound phone dialing, or new wire protocols.

## Inventory (verified on main, 2026-11-01)

- ALREADY: phone surface bearer auth gate (`internal/surface/server.go:36`; `internal/surface/gates.go:126-145`),
  token store at `<hearth>/tokens.json` (`internal/surface/tokens.go:20-53`), pairing endpoint and CLI
  (`internal/surface/pair.go:131-206`; `cmd/hearthd/pair.go:15-180`; `docs/SURFACE-SPEC.md §3`), and bearer gate
  doctrine on the phone surface (`internal/surface/credentials.go:15-22`).
- ALREADY: connector status SSE rail pattern with 5 s `: ping` heartbeats (`internal/surface/connectors_events.go:11-33`;
  `internal/surface/custos_cards.go:142-144`), long-lived streaming support with zero `WriteTimeout` on the surface
  HTTP server (`internal/surface/server.go:150-171`), and connector app state derivation (`internal/surface/connectors_row.go:106-120`).
- ALREADY: connector lifecycle verbs, approval gate via `registerCustosNamed` (`internal/surface/approvals.go:255`),
  carded PATCH/DELETE flow (`internal/surface/connectors_crud.go:190-200, 202-317, 422-435`), and atomic store
  persistence with mode 0600 (`writeSyncTempNamed`, `internal/custos/vault.go:36-68`).
- ALREADY: model-facing tool naming `mcp__<connector>__<tool>` under 64-char cap with FNV-32a truncation
  (`cmd/hearthd/mcpwiring.go:67-88`), policy gating (`cmd/hearthd/mcpwiring.go:127-189`), card-minted always-rules
  (`cmd/hearthd/custoswiring.go:178-191`; `cmd/hearthd/policysrc.go:188-206`), and dynamic tool injection into Hub
  (`internal/surface/turns.go:138-160`; `cmd/hearthd/custoswiring.go:135-148`).
- ALREADY: loop untrusted-tool approval path (`internal/loop/tools.go:69-85`; `internal/loop/session.go:291-328`)
  and turn-approval card registry (`internal/surface/approvals.go:171-187`; `internal/surface/turns.go:539-559`).
- ALREADY: closed audit kind sets (`internal/custos/mcp_catalog.go:25-42`; `internal/custos/types.go:76-115`),
  never-echo doctrine (`docs/CREDHANDLE-SPEC.md:31`), and secret needle scrubbing (`internal/custos/vault.go:194-197`;
  `internal/custos/vt2_e2e_test.go:586-612`).
- GAPS: no `/v1/devices` REST/SSE routes, no push sync or action stream endpoints, no `<hearth>/custos/devices.json`
  store; loop approver truncates args at 200 bytes (`internal/loop/tools.go:112-117`) rather than rendering full JSON
  on cards; no in-process device connector source in `hearthd`; no rail data-at-rest stores or closed rail handshake.

## Decisions (design freeze)

**D1 — Device = hearth-hosted connector fed by phone push.** Lararium pairs one phone to one hearth. The phone
never serves, hosts no listener, and is never dialed into. Instead, the hearth materializes each registered phone
as an in-process connector (`kind=device`). Built-in static tools (`buildTools`, `cmd/hearthd/tools.go:19-50`;
`cmd/hearthd/main.go:559`) and external MCP connectors (`cmd/hearthd/mcpwiring.go:38-63`) register into `Hub` at
turn start (`internal/surface/turns.go:138-160`; `cmd/hearthd/custoswiring.go:135-148`). The hearth-hosted device
connector plugs into this dynamic tool provider as an in-process tool source: read rails execute directly against
locally cached snapshots, while action rails dispatch out over the phone's open stream (D6). Model-facing tool
naming `mcp__<device_id>__<rail>_<verb>` survives v1 verbatim under the 64-char cap with FNV-32a truncation
(`cmd/hearthd/mcpwiring.go:67-88`).
**D2 — Registration over the paired session.** `POST /v1/devices/register` on the existing bearer-authed surface
(`internal/surface/server.go:36`; `internal/surface/gates.go:126-145`) carries `{device_id, display_name, rails_declared}`
— NO `endpoint_url` anywhere. Validates `device_id` slug `^[a-z0-9][a-z0-9_-]{0,31}$` and `rails_declared` ⊆ first-party
vocabulary (D4). The rails handshake replaces v1's network probe: hearth validates declared rails against closed
flag rules (read rails => `read_only: true`; action rails => not `read_only`). Undeclared or misflagged rails return
400 `rail_mismatch` with zero state persisted. On handshake success, surface raises a custody card via
`ApprovalHub.registerCustosNamed` (`internal/surface/approvals.go:255`) reviewing device name, ID, declared rails with
flags, and fingerprint. Approve → persist in registry + broadcast `device_state` upsert; deny → zero residue, audit
`device_registration_denied`. 409 `duplicate_name` or `pending_add` semantics match T2 (`internal/surface/connectors_crud.go:259-271`).
**D3 — Device registry and identity.** Persists at `<hearth>/custos/devices.json` (mode 0600) via atomic temp+rename+fsync
(`writeSyncTempNamed`, `internal/custos/vault.go:36-68`), storing registered devices (`device_id`, `display_name`,
`rails_declared`, `registered_at`, `last_seen`, `sync_epoch`) and tombstones. Boot merge mirrors connectors store law
(`docs/CONNECTOR-LIFE-SPEC.md D2`). Identity binds to the PAIRING token already held by that phone from `<hearth>/tokens.json`
(`internal/surface/tokens.go:20-53`; `internal/surface/pair.go:193-206`; `internal/surface/credentials.go:15-22`);
per-device rail tokens are ABOLISHED. Registration inherits the pairing session's bearer credential. Re-registration of
a tombstoned `device_id` requires a fresh card.
**D4 — Rail vocabulary and tool grammar.** Rails are a closed first-party vocabulary: read rails (`calendar`, `contacts`,
`notifications`, `calllog`, `health`), whose tools MUST carry `read_only: true`; and action rails (`dialer`, `sms`), whose
tools MUST NOT carry `read_only`. Handshake rejects undeclared tools or misflagged read rails with 400 `rail_mismatch`. Tools
surface model-facing as `mcp__<device>__<rail>_<verb>` under the 64-char cap with FNV-32a hashing on overflow
(`cmd/hearthd/mcpwiring.go:67-88`). Runtime schema drift rejects execution; re-registration re-attests.
**D5 — Approval path for action rails.** Action-rail tools ride the loop untrusted-tool approval path (`internal/loop/tools.go:69-85`;
`internal/loop/session.go:291-328`). Turn-approval cards (`internal/surface/approvals.go:171-187`; `internal/surface/turns.go:539-559`)
must show device name, rail, tool, and FULL arguments JSON (dialer phone number, SMS recipient and message body), strictly
bypassing the 200-byte summarizer truncation (`internal/loop/tools.go:112-117` `summarize()`). Verdicts follow once/deny/always
semantics: "always" mints an exact worker rule `<device>/<tool> -> auto` (`cmd/hearthd/custoswiring.go:178-191`;
`cmd/hearthd/policysrc.go:188-206`) silencing future cards for that tool. On approve, the action is enqueued for delivery on
the phone's open stream (D6) with an execution receipt required before the tool call resolves; if no stream is open, the call
fails `device_offline` immediately (no queueing to wake later).
**D6 — Live stream and push sync.** The phone maintains one long-lived authenticated stream: `GET /v1/devices/<id>/stream`.
Streaming runs over SSE (`internal/surface/connectors_events.go:11-33`); `newHTTPServer` sets `WriteTimeout: 0`
(`internal/surface/server.go:150-171`), permitting long-lived connections, while periodic `: ping` heartbeats
(`custosHeartbeatInterval` = 5 s, `internal/surface/custos_cards.go:142-144`) prevent `IdleTimeout` (120 s) drops.
The stream carries server->phone action dispatch frames (`event: action_dispatch`, data: `{"action_id","tool","args"}`);
the phone responds via `POST /v1/devices/<id>/actions/<action_id>/receipt`. Rail data is pushed via `POST /v1/devices/<id>/sync`:
full snapshot per rail plus incremental deltas with monotonically increasing `seq` per rail. Rail reads are served IN-PROCESS
by the hearth connector from local store; reads never round-trip to the phone, with result freshness reported as sync-lag.
**D7 — Liveness and offline.** Stream connected => device row state `read+act` (or `read-only` if only read rails enabled,
per `ConnectorAppState`, `internal/surface/connectors_row.go:106-120`). If the stream drops, a 30-second grace window absorbs
transient mobile handoffs before transitioning to `not-connected` with reason `offline`. Stream keepalives and sync POSTs
maintain `last_seen`. When offline: reads return the local copy with a staleness marker (`[stale: phone offline since <last_seen>, lag <duration>]`);
action calls fail fast with `device_offline`; reconnecting restores `online` without re-registration. Sleeping phone = offline
is accepted, though Android foreground services maintain streaming in practice; push notifications succeed because the phone
initiates push on event arrival.
**D8 — Audit kinds.** Closed audit kind set (`internal/custos/mcp_catalog.go:25-42`; `internal/custos/types.go:76-115`) appends:
`device_registered`, `device_forgotten`, `device_rail_toggled`, `device_registration_denied`. Payloads record `device_id`,
display name, fingerprint, rail names, and credential NAME only — never tokens, addresses, or phone numbers. Data-plane calls
for connectors are NOT audited in the tree (`cmd/hearthd/mcpwiring.go:183`); push sync and stream dispatch mirror this precedent
and omit data-plane audit lines. Policy gating records standard `worker_call_allowed` / `worker_call_denied`.

## Rail Data At Rest

Hearth stores push-synced copies of phone data locally at `<hearth>/devices/<device_id>/<rail>.json` (mode 0600),
persisted via atomic temp+rename+fsync (`writeSyncTempNamed`, `internal/custos/vault.go:36-68`). Retention defaults:
- `notifications`: TTL 7 days, max-items cap 500 events (rolling FIFO window pruned on sync).
- `calllog`: TTL 30 days, max-items cap 200 entries.
- `calendar`: events bounded to window `[-30 days, +90 days]` relative to current time.
- `contacts`: full address book snapshot, updated via incremental deltas.
- `health`: last-snapshot-only (current day metrics; no multi-year historical telemetry stored).

Wipe-on-forget: `DELETE /v1/devices/<id>` purges `<hearth>/devices/<device_id>/` from disk completely, revokes device
state, emits `device_gone`, and records a tombstone in `devices.json` (`internal/surface/connectors_crud.go:422-435`).
Red lines: rail data NEVER enters daemon logs, audit lines, or SSE broadcast frames. Secret needle scrubbing
(`internal/custos/vault.go:194-197`; `internal/custos/vt2_e2e_test.go:586-612`) applies to all rail payloads.
Future audits must verify that sensitive health metrics and call records remain strictly isolated to the rail store.

## Surface & Routes Summary

All endpoints ride behind the existing surface bearer gate (`internal/surface/server.go:36`; `internal/surface/gates.go:126-145`;
`internal/surface/tokens.go:20-53`).
- Phone-scoped routes:
  - `POST /v1/devices/register`: `{device_id, display_name, rails_declared}` → 202 `{device_id, card_id}` (or 400 `rail_mismatch`, 409 `duplicate_name`/`pending_add`).
  - `GET /v1/devices/<id>/stream`: long-lived SSE stream delivering server->phone `action_dispatch` frames.
  - `POST /v1/devices/<id>/actions/<action_id>/receipt`: phone submits action execution receipt `{ok, result, error}`.
  - `POST /v1/devices/<id>/sync`: phone pushes `{rail, snapshot, deltas, seq}` to update local rail store.
- Admin-scoped routes:
  - `GET /v1/devices`: lists device rows (`device_id`, `display_name`, `state`, `reason`, `rails`, `last_seen`, `registered_at`).
  - `PATCH /v1/devices/<id>/rails`: `{rail, enabled}` carded rail toggle (reuses T2 flow, `internal/surface/connectors_crud.go:190-200, 202-317`).
  - `DELETE /v1/devices/<id>`: forgets device, purges rail store, emits `device_gone`, persists tombstone (`internal/surface/connectors_crud.go:422-435`).
  - `GET /v1/devices/events` & `GET /v1/devices/<id>/events`: SSE stream emitting `device_state` upserts and `device_gone` events (`internal/surface/connectors_events.go:11-33`).

## Requirements

R1. `custosd`/`hearthd`: in-process device connector source materializing `kind=device` connectors feeding the dynamic
    tool catalog (`mcpToolSource`, `internal/surface/turns.go:138-160`) from push-synced rail stores, with fast-fail
    `device_offline` action dispatch.
R2. `surface`: phone-scoped routes (register, sync, stream, receipt) and admin-scoped routes (GET rows, PATCH rails toggle,
    DELETE forget, SSE events) behind existing bearer auth; device registry `<hearth>/custos/devices.json` and rail stores
    `<hearth>/devices/<device_id>/<rail>.json` with atomic fsync; full-arguments JSON card review.
R3. Gate: registration handshake-then-card pipeline via `ApprovalHub.registerCustosNamed` (`internal/surface/approvals.go:255`)
    with cleanup on deny or timeout (zero residue, audit `device_registration_denied`).
R4. Rail enforcement: strict handshake validation rejecting undeclared rails or misflagged tools (400 `rail_mismatch`).
R5. Audit kinds: append `device_registered`, `device_forgotten`, `device_rail_toggled`, `device_registration_denied`
    (`internal/custos/mcp_catalog.go:25-42`; `internal/custos/types.go:76-115`).
R6. Exit criterion: V-T3 suite in CI job `device-rails`, green on main, with fake-phone HTTPS client fixture (plain HTTPS
    client; no round-tripper dial hacks or `LARARIUM_TEST_ALLOW_DEVICE_HOST`).

## Exit criterion (V-T3 suite, all in one CI job)

- V-T3a: paired phone registers → card on phone rail → approve → device row online, tools carry `mcp__<device>__` prefix, and `device_state` event arrives over SSE without test polling.
- V-T3b: rail sync (snapshot then delta with seq) → read through the session loop serves synced data in-process, `last_used` stamps, no card for read rails.
- V-T3c: action call parks card with FULL args JSON; approve → action dispatch frame on open stream → receipt resolves call.
- V-T3d: stream drop → offline after 30s grace; reads carry staleness marker; action fast-fails `device_offline`; reconnect → online, no re-registration.
- V-T3e: PATCH disables a rail → carded toggle → tool drops from catalog and sync for that rail is rejected; re-enable round-trips; both audited `device_rail_toggled`.
- V-T3f: DELETE forgets: gone from GET, `device_gone` event emitted, rail data wiped from disk, restart keeps tombstone; re-register needs fresh card.
- V-T3g: token never leaks: pairing token appears in NO response body, SSE frame, audit line, or log — raw or encoded (asserted via `assertNoNeedle`, `internal/custos/vt2_e2e_test.go:586-612`).
- V-T3h: misflag handshake (read rail declaring non-read_only tool) → 400 `rail_mismatch`, nothing persisted, no card.
- V-T3i: legacy compatibility: pre-T3 clients on `GET /v1/mcp/connectors` see consistent catalog; device rows are additive, never re-type legacy entries.

## Out of scope

Android client rail implementation (`android-client` repo: permissions, foreground services, Health Connect, Play release
flavor); multi-phone fleets; cross-hearth roaming; notification action-on-phone features (dismiss-as-read); OS push wake
(FCM) for action dispatch (stream-only v1); per-rail encryption-at-rest beyond the existing 0600 store law (deferred with note:
disk-level or user-passphrase encryption evaluated for v3); reverse gateway processes or Tailscale dialing (rejected by PO
directive 2026-11-01); non-first-party device registration; TLS interception (permanent V10 doctrine); altering pairing flow.

## Changelog

- v2 (2026-11-01): transport reversed to phone-push per PO directive; v1 never merged.
- v1 (2026-11-01): initial freeze text per issue #217 (rejected: tailnet pull transport).
