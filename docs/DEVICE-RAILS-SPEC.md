# DEVICE-RAILS-SPEC — device rails: phone as tailnet MCP server (T3)

Status: **FREEZE-PROPOSED (v1)** — merging this document freezes the design (issue #217;
PO directive 2026-11-01). Amendments follow the CUSTOS-SPEC append-only protocol.
Implementation is tracked separately and MUST land the V-T3 suite below as its exit criterion.

Companions: MCP-SPEC (transport, discovery, flags), CONNECTOR-LIFE-SPEC (lifecycle verbs, store,
state rail), CUSTOS-SPEC (cards, doors, audit), SURFACE-SPEC (phone surface, bearer gate, pairing).

## Purpose

T3 (ROADMAP): first-party device rails exposing phone capabilities (calendar, contacts,
notifications, calllog, health, dialer, sms) to the hearth as an embedded MCP server running on
the Android device. The hearth dials the device over its private Tailscale HTTPS endpoint
(tailscale-serve) using standard HTTP MCP transport. This specification freezes design for
device registration, bearer identity, rail tool grammar, card-gated action approvals, status and
offline semantics, rail toggles, and audit logging without gateway processes or new wire protocols.

## Inventory (verified on main, 2026-11-01)

- ALREADY: `MCPServerConfig` HTTP transport natively supports `bearer` (internal/custos/mcp_config.go:64),
  lazily resolving `${vault:}`/`${env:}` at dial (internal/custos/mcp_http.go:68-103) and injecting
  `Authorization: Bearer <token>` (internal/custos/mcp_http.go:41-46); HTTPS URLs permitted (line 232).
- ALREADY: `custosd` connector lifecycle verbs `MCP-ADD`, `MCP-REMOVE`, `MCP-STATE`, `MCP-PROBE`
  (docs/CONNECTOR-LIFE-SPEC.md D1; internal/surface/connectors_crud.go:277, 351-390), connector store
  `<hearth>/custos/connectors.json` (internal/custos/vault.go:36-68), and vault promotion (internal/custos/mcp_manage.go:154-244).
- ALREADY: HTTP transport failure tracking transitions to `backoff` (`MCPReasonTransport`) after 3
  consecutive failures (internal/custos/mcp_http.go:119-131; internal/custos/mcp_rail.go:361-387);
  surface derives `AppStateNotConnected` server-side (internal/surface/connectors_row.go:106-120).
- ALREADY: model-facing MCP tool naming `mcp__<connector>__<tool>` with 64-char cap and FNV-32a
  truncation (cmd/hearthd/mcpwiring.go:67-88), policy gating (cmd/hearthd/mcpwiring.go:127-189), and
  card-minted always-rules (cmd/hearthd/custoswiring.go:178-190; cmd/hearthd/policysrc.go:188-206).
- ALREADY: phone surface bearer auth gate (internal/surface/server.go:80-84; internal/surface/credentials.go:15-22),
  token store at `<hearth>/tokens.json` (internal/surface/tokens.go:20-40), and pairing CLI/store
  (cmd/hearthd/pair.go:15-170; docs/SURFACE-SPEC.md §3).
- GAPS: no `/v1/devices` REST/SSE endpoints or `<hearth>/custos/devices.json` store; loop approver
  truncates args at 200 bytes (internal/loop/tools.go:112-117) rather than rendering full JSON on cards;
  no tailnet endpoint validation or closed rail vocabulary enforcement at probe.

## Decisions (design freeze)

**D1 — Device = reverse-dialed connector.** An Android phone runs an embedded MCP server over its
Tailscale HTTPS endpoint (`tailscale-serve`). The hearth dials `https://<device-tailnet>/mcp` via
`custosd` HTTP transport using a per-device bearer token (internal/custos/mcp_http.go:41-46). NO new
protocol, NO gateway process, NO reverse tunnel in v1. V10 doctrine holds: direct dial with pinned
audience token; daemon never intercepts TLS. Verification on main confirms `MCPServerConfig` already
supports `bearer` (internal/custos/mcp_config.go:64); no config grammar amendment is required. The
device is a connector with `kind=device`; all T2 lifecycle mechanisms (probe, store, tombstones,
states, last_used, status SSE, deny-lists, vault promotion) apply unchanged.
**D2 — Registration.** Admin-scoped `POST /v1/devices/register` (phone must already be paired to the
hearth, presenting bearer auth from `<hearth>/tokens.json`) carries `{device_id, display_name,
endpoint_url, rails_declared}`. It validates: `device_id` slug `^[a-z0-9][a-z0-9_-]{0,31}$`,
`rails_declared` ⊆ first-party vocabulary (D4), and `endpoint_url` resolves on-tailnet (`100.64.0.0/10`
RFC 6598 or `*.ts.net`). Literal loopback is refused; ordinary connector loopback/RFC1918 guards
(internal/custos/mcp_config.go:217-253) do not apply because a phone is a private mobile endpoint by
design. Registration runs `MCP-PROBE`; failure or undeclared/misflagged rails return 400. On probe
success, surface raises a custody card via `ApprovalHub.registerCustosNamed` (internal/surface/approvals.go:255)
reviewing device name, userinfo-stripped endpoint, tool list with flags, and fingerprint (A6). Approve
→ `MCP-ADD` + persist + broadcast; deny → probe reaped, zero residue, audit `device_registration_denied`.
409 `duplicate_name` or `pending_add` semantics match T2.
**D3 — Device registry and identity.** Persists at `<hearth>/custos/devices.json` (mode 0600) via
atomic temp+rename+fsync (`writeSyncTempNamed`, internal/custos/vault.go:36-68), storing registered
devices (`device_id`, `display_name`, `endpoint_url`, `bearer_cred_name`, `rails_declared`,
`registered_at`) and tombstones. Boot merge mirrors connectors store law (registry entries restore,
tombstones stay gone). The per-device bearer token is minted at registration, stored ONLY in the vault
under connector-scoped name `mcp-<device_id>-bearer` (internal/custos/mcp_manage.go:139-147), and returned
EXACTLY ONCE in the create response body; it appears nowhere else ever (CREDHANDLE D5 never-echo
doctrine; GET bodies carry credential NAME only). Re-registration of a tombstoned `device_id` requires
a fresh card and mints a fresh token.
**D4 — Rail vocabulary and tool grammar.** Rails are a closed first-party vocabulary in v1: read rails
(`calendar`, `contacts`, `notifications`, `calllog`, `health`), whose tools MUST carry `read_only: true`;
and action rails (`dialer`, `sms`), whose tools MUST NOT carry `read_only`. Undeclared tools or misflagged
read rails reject probe with 400 `rail_mismatch`. Tools surface model-facing as `mcp__<device>__<rail>_<verb>`
under the 64-char cap with FNV-32a hashing on overflow (cmd/hearthd/mcpwiring.go:67-88). Runtime schema
drift puts the connector to backoff; re-registration re-attests.
**D5 — Approval path for action rails.** Action-rail tools ride the loop untrusted tool approval path
(internal/loop/tools.go:69-85; internal/loop/session.go:291-328). Turn-approval cards (internal/surface/approvals.go:171-187;
internal/surface/turns.go:539-559) must show device name, rail, tool, and the FULL arguments JSON
(dialer number, sms recipient and message body). Verdicts follow existing once/deny/always semantics:
"always" mints an exact rule `<device>/<tool> -> auto` (cmd/hearthd/custoswiring.go:178-190;
cmd/hearthd/policysrc.go:188-206) silencing future cards for that tool.
**D6 — Devices view + rail toggles.** `GET /v1/devices` returns device rows: `device_id`, `display_name`,
`state` (reusing internal/surface/connectors_row.go:106-120 app state machine, plus `offline` per D7),
`rails` array `[{rail, enabled, tools:[{name, flags}]}]`, `last_used`, and `registered_at`. Endpoints
strip userinfo; tokens never shown. `PATCH /v1/devices/<id>/rails` `{rail, enabled}` toggles a rail
by rewriting tool deny-lists via carded T2 PATCH flow (internal/surface/connectors_crud.go:190-200, 202-304).
`DELETE /v1/devices/<id>` forgets the device without cards (internal/surface/connectors_crud.go:409-429):
`MCP-REMOVE`, vault token revoked, tombstone persisted, `device_gone` event emitted. `GET /v1/devices/<id>/events`
and `GET /v1/devices/events` stream SSE status updates (`device_state` upsert, `device_gone` per internal/surface/connectors_events.go:11-33).
**D7 — Offline semantics.** Phone disconnects are normal operational life. Dial failures record
transport failures (internal/custos/mcp_http.go:119-131); unreachable phones transition to `not-connected`
with reason `offline`. Dispatched calls fail fast with `device_offline` within bounded timeout.
Reconnecting restores connectivity automatically without re-registration; token and endpoint persist.
**D8 — Audit kinds.** Closed audit kind set (internal/custos/mcp_catalog.go:24-41; internal/custos/types.go:76-115)
appends: `device_registered`, `device_forgotten`, `device_rail_toggled`, `device_registration_denied`.
Payloads record `device_id`, `name`, `fingerprint`, rail names, and credential NAME only — never tokens,
userinfo, or bodies. Verdict attribution follows existing `via` (`web`, `telegram`).

## Requirements

R1. `custosd`: dial-failure-to-offline status mapping and fast-fail `device_offline` execution path;
    bearer authentication uses existing `MCPServerConfig.Bearer` without grammar amendment.
R2. `surface`: REST routes (register, GET, PATCH rails, DELETE, and SSE stream) behind admin bearer auth;
    device store `<hearth>/custos/devices.json` with tombstones; full-arguments JSON card review.
R3. Gate: probe-then-card registration pipeline via `ApprovalHub.registerCustosNamed` with cleanup on deny
    or timeout (zero residue, audit `device_registration_denied`).
R4. Rail enforcement: strict probe validation rejecting undeclared or misflagged tools (400 `rail_mismatch`).
R5. Audit kinds: append `device_registered`, `device_forgotten`, `device_rail_toggled`, `device_registration_denied`.
R6. Exit criterion: V-T3 suite in CI job `device-rails`, green on main, with fake-phone HTTPS fixture
    (dialed via `WithHTTPRoundTripper`) and test-only env override `LARARIUM_TEST_ALLOW_DEVICE_HOST=1`.

## Exit criterion (V-T3 suite, all in one CI job)

- V-T3a: fake phone registers → card on phone rail → approve transitions to `read+act` with declared rails,
  tools carry `mcp__<device>__` prefix, and `device_state` event arrives over SSE without test polling.
- V-T3b: read-rail call end-to-end through session loop → result; `last_used` stamps; NO card for read-only tools.
- V-T3c: action-rail call parks custody card displaying device, rail, tool, and full args JSON; allow-once
  executes at fixture; deny aborts call and audit records `worker_call_denied`.
- V-T3d: phone endpoint unreachable → row shows `not-connected` reason `offline`; call fast-fails with
  `device_offline`; endpoint restored → ready again, no re-registration, token still valid.
- V-T3e: PATCH disables contacts rail → deny-list card → approve drops tool from catalog and GET shows
  disabled; re-enable round-trips; both hops audited `device_rail_toggled`.
- V-T3f: DELETE forgets → gone from GET, `device_gone` event emitted, daemon restart keeps tombstoned;
  re-register with same `device_id` requires fresh card and mints fresh token.
- V-T3g: minted token appears in NO subsequent response body, SSE event, audit line, or daemon log (raw or
  base64); registry stores `${vault:}` reference; probe-registered tools authenticate at the wire.
- V-T3h: mis-flagged fixture (read rail advertising non-read_only tool) → register = 400 `rail_mismatch`,
  nothing persisted, no card raised.
- V-T3i: pre-T3 door client and pre-T3 `GET /v1/mcp/connectors` consumers see consistent catalog when
  device registers; device rows are additional and never re-type legacy entries.

## Out of scope

Android client rail implementation (android-client repo: permissions, providers, Health Connect, sideload
flavor); Google Workspace and home-stack waves (roadmap waves 2–3); multi-cell device sharing; push wake
(sleeping phone is offline; v1 accepts it); new wire protocols or reverse gateway processes (rejected by
D1); non-first-party device registration; TLS interception (permanent V10); altering pairing flow.

## Changelog

- v1 (2026-11-01): initial freeze text per issue #217.
