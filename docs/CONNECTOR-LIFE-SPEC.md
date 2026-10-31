# CONNECTOR-LIFE-SPEC — connector lifecycle and dynamic management (T2)

Status: **FREEZE-PROPOSED (v1)** — merging this document freezes the design (issue #212;
PO directive 2026-10-31). Amendments follow the CUSTOS-SPEC append-only protocol.
Implementation is tracked separately and MUST land the V-T2 suite below as its exit criterion.

Companions: MCP-SPEC (rail mechanics, amendments), CUSTOS-SPEC (cards, doors, audit),
SURFACE-SPEC (phone surface), CREDHANDLE-SPEC (handles used by connector configs).

## Purpose

T2 (ROADMAP): schema-driven, card-gated dynamic lifecycle for external tool servers (MCP connectors).
Today connectors are declared in static configuration, requiring daemon restarts to mutate. This
specification freezes design for adding, editing, toggling, removing, re-authenticating, and observing
connectors dynamically over authenticated phone-facing REST/SSE API without compromising isolation or secrecy.

## Inventory (verified on main, 2026-10-31)

- ALREADY: `GET /v1/mcp/connectors` is inventory-only (internal/surface/server.go:71; internal/surface/custos_cards.go:444-459)
  returning `ConnectorCard` (internal/surface/custos_cards.go:33-39) distinct from `CustosCard` (internal/surface/custos_cards.go:18-27).
- ALREADY: `custosd` owns the rail (internal/custos/mcp_lifecycle.go:24-27 states
  starting/ready/backoff/disabled; `MCPReasonNameOccupied` at line 38; `RailRegistry` at
  lines 698-796 with RegisterConnector/Connector/ManualRefresh/Refresh/NotifyCredentialGone).
- ALREADY: door frames `MCP-STATUS`/`MCP-TOOLS` broadcast (internal/custos/mcp_doors.go:76-113;
  attach replay lines 42-72) and hearthd mirror types (internal/custosdoor/types.go:37-85).
- ALREADY: ctl verb dispatch `MCP-STATUS`/`MCP-TOOLS`/`MCP-REFRESH`/`MCP-CALL`
  (internal/custos/ctl_mcp.go:9-38; internal/custos/ctl.go:293, 460-466).
- ALREADY: config grammar (internal/custos/mcp_config.go:57-69 `MCPServerConfig`, name regex line
  101, unknown keys lines 22-30, 103-133, loopback/RFC1918 transport guards lines 165-180, 217-253).
- ALREADY: lazy `${vault:}`/`${env:}` resolution at compose (internal/custos/mcp_stdio.go:150-170;
  internal/custos/mcp_http.go:68-90; CREDHANDLE-SPEC inventory drift corrected from 150-158, 66-80).
- ALREADY: vault state root `<hearth>/custos` (internal/custos/vault.go:82-85, 124) and atomic
  temp+rename+fsync write pattern `writeSyncTempNamed` (internal/custos/vault.go:36-68).
- ALREADY: custody-card SSE without `Last-Event-ID` tracking (internal/surface/custos_cards.go:138-145,
  368-438); `ApprovalHub.RegisterCustos` card primitive (internal/surface/approvals.go:247-290).
- ALREADY: T1 write-only admin endpoints behind bearer auth (internal/surface/credentials.go:15-22,
  94-117; internal/surface/server.go:80-83) with never-echo create (internal/surface/credentials.go:138-194).
- ALREADY: closed audit kinds (internal/custos/mcp_catalog.go:24-32; internal/custos/types.go:76-115) and A7
  credential latch + refresh-as-re-ensure (docs/MCP-SPEC.md §1 A7, §6, §7, §11;
  internal/custos/mcp_lifecycle.go:42-73, 448-450, 485-488). Note: A9 was allocated to MCP-CALL; door verbs are A10.
- GAPS: no dynamic mutations, persistence/tombstones, form descriptor, or push status SSE rail.

## Decisions (design freeze)

**D1 — Execution plane vs gate plane.** `custosd` remains the ONLY process spawning or
registering connectors. New `doors.sock` verbs (MCP-SPEC amendment A10): `MCP-ADD <name>
<config-json>` (add-or-replace, starts connector), `MCP-REMOVE <name>`, `MCP-STATE <name>
<enabled|disabled>`, and `MCP-PROBE <config-json>` (boot child, tools/list once, kill; probe never
registers or persists). Frames are additive (A3 pattern). Policy gate lives in `internal/surface`.
**D2 — Runtime persistence.** Store at `<hearth>/custos/connectors.json` (mode 0600) via atomic
temp+rename+fsync (`writeSyncTempNamed`, internal/custos/vault.go:36-68). Stores API-mutated configs
and tombstones for API-deleted names. Boot merge: `lararium.yaml` seed loaded first, store overrides
by name, tombstones dropped (including YAML seeds); daemon never rewrites YAML. Literal secrets in
POSTed configs are PROMOTED to vault under connector-scoped names; stored configs keep only
`${vault:NAME}` refs; read path never echoes literals (CREDHANDLE-SPEC D5 never-echo doctrine).
**D3 — Add flow (schema-driven, card-gated).** `POST /v1/mcp/connectors` (admin token) runs in order:
strict config validation (internal/custos/mcp_config.go:22-30; 400 with existing reasons);
`MCP-PROBE` (failure = 400 with reason, no card); on probe success raise custody card via
`ApprovalHub.RegisterCustos` (internal/surface/approvals.go:247) reviewing name, transport, target
url/command+args, tools with read_only/destructive/open_world flags, fingerprint (A6), and env var
NAMES only (never values). Returns 202 `{name, card_id}` immediately. Approve → `MCP-ADD` + persist +
status broadcast; deny or `ask_hold_timeout` expiry → probe killed, nothing persisted, audit
`mcp_add_denied`. An already-committed name returns 409 `duplicate_name`; a pending add of the
same name returns 409 `pending_add`. Ordinary card on phone/web/Telegram.
**D4 — Edit, delete, enable, disable.** `PATCH /v1/mcp/connectors/{name}` runs full D3 flow (probe
new shape; card shows diff: tools added/removed, fingerprint change, env names touched; approve =
replace). Capability mutations are carded; no card-free edit exists. `DELETE
/v1/mcp/connectors/{name}` is ALWAYS allowed without cards: stop, deregister, persist tombstone,
push rail. `POST /v1/mcp/connectors/{name}/enable` and `/disable` are user toggles without cards
(restoring/reducing approved shapes), audited via `mcp_toggled`. All four admin-scoped.
**D5 — Status rail (push, never poll).** SSE stream `GET /v1/mcp/connectors/events` on surface
mirrors custody stream (internal/surface/custos_cards.go:368-438; 5 s `: ping`, no `Last-Event-ID`
tracking; clients bootstrap via snapshot). Two event kinds: `connector_state` with FULL row (upsert
semantics) and `connector_gone` with `{name}`. `hearthd` fans out from door `MCP-STATUS`/`MCP-TOOLS`.
`GET /v1/mcp/connectors` grows additively: `state`, `reason`, `tools`, `has_write_tools` (any tool
lacking read_only hint), `fingerprint`, `enabled`, `last_used` (RFC3339 or null), `url_or_command`
summary (userinfo stripped), and `env` names. No secret values in any field, ever.
**D6 — App-facing state machine.** Derived server-side into exactly: `not-connected` (starting,
backoff, or pending approval), `needs-re-auth` (reason `credential`: A7 latch down or referenced
handle gone), `read-only` (ready and every tool read-only-hinted), `read+act` (ready and at least
one tool not read-only), and `disabled` (user toggle). `last_used` stamped by `custosd` at successful
`MCP-CALL` dispatch; store persistence is best-effort (cold restart blank acceptable; state loss is not).
**D7 — Re-auth flow.** When referenced vault credential is deleted or A7 latch drops
(`NotifyCredentialGone` path, internal/custos/mcp_lifecycle.go:393-433), connector moves to
`needs-re-auth` (reason `credential`) and stops calling (A7 semantics). `POST
/v1/mcp/connectors/{name}/reauth` runs `ManualRefresh` (internal/custos/mcp_lifecycle.go:469-504): if user
writes new vault value under same name (T1 write-only POST), refresh re-ensures slot and state rail
returns connector to `ready`. Re-auth is a named door over existing refresh verb.
**D8 — Form schema endpoint.** `GET /v1/mcp/connector-form` returns JSON descriptor of connector
config shape: key, type (`string`/`string_list`/`secret_string`/`enum`/`int`/`duration`), required,
default, validation grammar (name regex, url rules), and allowed enum values (transports). Includes
`schema_version` for forward-compat; APK renders forms generically without daemon-dependent app
updates. Client duplicate-name detection is advisory; 409 responses (`duplicate_name`,
`pending_add`) are authoritative; `duplicate_name` directs clients to offer Edit instead of Add.
**D9 — Audit kinds.** Appended to A8 closed set (internal/custos/mcp_catalog.go:24-32) via amendment:
`mcp_added`, `mcp_removed`, `mcp_edited`, `mcp_toggled`, `mcp_add_denied`. Payloads carry connector
name, catalog fingerprint, and env var NAMES only — never config values or secrets. Card verdicts
record door attribution through the existing via-attribution mechanism; the via set stays frozen
(web, telegram) — a phone-side verdict settles as web, and no third via value is introduced.

## Requirements

R1. `custosd`: new door verbs (`MCP-ADD`, `MCP-REMOVE`, `MCP-STATE`, `MCP-PROBE`), dynamic registry
    ops, `<hearth>/custos/connectors.json` persistence with tombstones, vault promotion, and `last_used`.
R2. `surface`: REST routes (POST/PATCH/DELETE/enable/disable/reauth, `GET /v1/mcp/connector-form`,
    and additive GET growth); admin bearer auth enforced identically to `/v1/credentials`.
R3. Gate: probe-then-card execution pipeline with cleanup on deny or timeout (probe process group
    reaped via SIGTERM/SIGKILL, zero residue).
R4. Push status rail: `GET /v1/mcp/connectors/events` SSE stream per D5 fanning out from door broadcasts.
R5. Audit kinds: append `mcp_added`, `mcp_removed`, `mcp_edited`, `mcp_toggled`, `mcp_add_denied` per D9.
R6. Exit criterion: the V-T2 test suite, executed in one CI job, green on main.

## Exit criterion (V-T2 suite, all in one CI job)

- V-T2a: POST add of fixture connector raises card on phone rail; approve transitions to `ready`,
  tools count updates in snapshot, and `connector_state` event arrives over SSE without test polling.
- V-T2b: deny card → probe child reaped (no orphan process), store untouched, GET has no such
  connector, audit carries `mcp_add_denied` with denying via.
- V-T2c: second POST of existing name → 409 `duplicate_name`; POST while own add is pending → 409 `pending_add`.
- V-T2d: PATCH deny-listing one tool → card shows removed tool → approve → tools count drops and
  `MCP-TOOLS` no longer lists it.
- V-T2e: DELETE → gone from GET, `connector_gone` on rail, daemon restart (fresh process, same state
  dir) keeps it gone (tombstone over YAML seed proven).
- V-T2f: POST with literal secret → literal appears in NO response body, SSE event, audit line, or
  daemon log (raw and base64); store holds only `${vault:}` ref; connector still authenticates at wire.
- V-T2g: disable then enable round-trips with `connector_state` events both ways and no card.
- V-T2h: delete referenced vault credential → state becomes `needs-re-auth` (reason `credential`);
  write new value under same name via T1 endpoint; POST reauth → ready; state events at each hop.
- V-T2i: old door client (no new verbs understood) connects to new `custosd` and works; all new frames additive.

## Out of scope

OAuth flows and browser-based auth (reserved for future amendment); connector marketplace or
templates beyond generic form schema; per-cell connector scoping; altering `MCP-PROBE` execution
beyond child boot and single `tools/list` (tool execution forbidden); TLS interception or CA
installation (permanent V10 doctrine); editing `lararium.yaml` through daemon API (daemon never
rewrites user config); Android-side screen implementation (tracked in android-client repo).

## Changelog

- v1 (2026-10-31): initial freeze text per issue #212.
