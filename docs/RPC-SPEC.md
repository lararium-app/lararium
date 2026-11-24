# RPC-SPEC — WebSocket JSON-RPC transport

Status: **RATIFIED v1** — proposed 2026-11-20, all four §7 questions
ratified by PO 2026-11-20 (following every leaning proposal as drafted —
see §7 for the ratified text). Supersedes `docs/SURFACE-SPEC.md` (FROZEN, ratified
2026-10-02) under a **hard-cutover** plan: no REST compatibility shim, no
parallel-run window. PO decision (2026-11-20): acceptable because current
usage is single-user testing; both dogfood phones take a fresh APK in the
same release this ships.

This document does not relitigate *why* — see the chat record 2026-11-20
(pros/cons/risk discussion) for the reasoning. It specifies *what*: the new
transport, the method catalog (a 1:1 remap of every SURFACE-SPEC v1 route,
same semantics, same validation, same audit behavior — only the wire shape
changes), and what explicitly stays out of it.

Companion specs unchanged by this document: PENATUS-SPEC (file layer),
CUSTOS-SPEC (policy/custody semantics — this transport carries the same
verbs, doesn't redefine them), KEYS-SPEC. ASSISTANT-PHASE-SPEC §2
(model switching, issue #301) and the persona-file proposal (issue #302) are
folded into this document directly per PO decision 2026-11-20 — neither
ships as a REST route first.

## 0. Why one connection

SURFACE-SPEC v1 is N independent REST routes plus one SSE stream per open
session. This spec replaces that with one authenticated WebSocket per
client, carrying JSON-RPC 2.0 requests/responses for everything that used to
be a REST call, and JSON-RPC *notifications* (server push, no `id`, no
reply expected) for everything that used to require a client to open an SSE
stream and watch it. A client holds exactly one socket; multiplexing is
by request `id`, not by HTTP endpoint.

## 1. What stays plain HTTP (bootstrap only)

Two things predate authentication and don't belong on an authenticated
socket — both unchanged from SURFACE-SPEC v1:

| Method | Path | Behavior |
|---|---|---|
| GET | `/v1/health` | 200 `{"ok":true,"version":…}` — unauthenticated, unchanged. Liveness probe has to work before a WS handshake makes sense (LAN discovery, monitoring, `hearthd token create` preflight). |
| GET | `/`, `/app.js`, `/app.css` | Embedded web page + assets — unauthenticated, immutable, unchanged. Contains no secret; the page itself opens the authenticated WS connection using a token from the URL fragment, same as it reads today's bearer token. |

Everything else in SURFACE-SPEC v1 moves onto the socket.

## 2. Connection and auth

`GET /v1/ws` upgrades to a WebSocket. The token model is **unchanged** —
this spec migrates transport, not auth, per PO decision 2026-11-20 (recorded
on #302): single real user today, a ticket/grant system solves a problem
that doesn't exist yet. `lar1_<32 base62>` tokens, SHA-256(token) at rest in
`tokens.json` (0600), constant-time compare, no owner binding,
`hearthd token create/list/revoke` CLI untouched.

Handshake:

```
→ client: {"jsonrpc":"2.0","id":0,"method":"auth.hello","params":{"token":"lar1_…"}}
← server: {"jsonrpc":"2.0","id":0,"result":{"ok":true,"version":"1.0.0-betaN"}}
```

- No `auth.hello` within 5s of connect → close code `4001` (`auth_timeout`).
- Bad/revoked token → close code `4003` (`unauthorized`); mirrors today's 401.
- Any other method called before a successful `auth.hello` → connection
  closed `4003` immediately (no error reply first — don't let an
  unauthenticated peer probe the method catalog).
- Exactly one live `auth.hello` per connection; a second one closes `4002`
  (`already_authenticated`) — reconnect for a new token instead of
  re-authing in place.
- Every server frame write has a 10s deadline. A write only blocks once the
  client has stopped reading and the socket buffers on both ends are full;
  when the deadline passes the server closes the connection (no close frame,
  since the stream may hold a partial frame) and tears it down exactly like a
  client disconnect. Without the deadline, one stalled client could block
  the turn that is emitting to it.

## 3. Envelope

Request (client → server), standard JSON-RPC 2.0:
```json
{"jsonrpc":"2.0","id":<int>,"method":"<namespace>.<verb>","params":{…}}
```

Response (server → client), success:
```json
{"jsonrpc":"2.0","id":<int>,"result":{…}}
```

Response, error:
```json
{"jsonrpc":"2.0","id":<int>,"error":{"code":<int>,"message":"<human>","data":{"reason":"<machine string>", …}}}
```
`error.data.reason` carries the exact string SURFACE-SPEC v1 used as its
JSON error body (`"invalid_model_pin"`, `"model_catalog_unavailable"`,
`"policy_corrupt"`, etc.) — nothing in the audit/log/client-handling layer
that keyed off those strings has to change. `error.code` is informational
only — ratified generic/uninformative (always `-32000`), real signal lives
entirely in `error.data.reason` (§7.1).

Notification (server → client, push, no `id`, no reply):
```json
{"jsonrpc":"2.0","method":"<event>.<type>","params":{…}}
```
Replaces every SSE stream and every "catch-up GET" from SURFACE-SPEC v1.
A client that wants history-then-live for a resource calls that resource's
`*.subscribe` method; the result carries the catch-up snapshot and the
subscription is now live — matching events arrive as notifications with no
further polling.

## 4. Method catalog — 1:1 remap of SURFACE-SPEC v1

Every row preserves its v1 semantics (validation, lock states, audit actor
strings, idempotency) exactly; only the shape changes from
verb+path+body to method+params. Read "same as v1 §X" as binding — this
table does not re-derive rules already frozen in SURFACE-SPEC v1 history.

| v1 route | v2 method | params | result | notes |
|---|---|---|---|---|
| GET /v1/sessions | `sessions.list` | `{}` | `{"sessions":[…]}` | same header shape, newest-first |
| POST /v1/sessions | `sessions.create` | `{title?, model_pin?, private?}` | session header | same validation as v1 (incl. the model_pin 400 fix from #301); `private` per SURFACE-SPEC §4b, reason `private_requires_identity` when the connection's token names no caller |
| GET /v1/sessions/{id}/events + implicit SSE | `sessions.subscribe` | `{id, after_seq?, limit?}` | `{"events":[…],"in_flight":bool}` then live `session.event` notifications `{"session_id","event":{…}}` | collapses catch-up GET and the open-ended SSE stream into one call; default/cap M=200/1000 unchanged. Ordering: no `session.event` for that id is written to the connection before the subscribe result. Events emitted while the call is between registering and replying are held and written right after the result, in emit order, so none is lost and none arrives first. Live events can overlap the catch-up array (a turn whose persisted message is already in it may still be streaming); `turn_done.seq` names that persisted message |
| POST /v1/sessions/{id}/messages | `sessions.send` | `{id, text}` | `{"accepted":true}` immediately | turn output arrives as `session.event` notifications on the id's existing `sessions.subscribe`, not inline in the response — a session must be subscribed before `sessions.send`, else 409 `not_subscribed` |
| POST /v1/sessions/{id}/cancel | `sessions.cancel` | `{id}` | `{"ok":true}` | 404 `no_turn_in_flight` unchanged |
| POST /v1/sessions/{id}/approvals/{approval_id} | `sessions.approvals.resolve` | `{session_id, approval_id, decision, scope?}` | `{"ok":true}` | 410 `already_resolved` unchanged; `scope` (only valid value `"connector"`) is only accepted when `decision:"allow"` — same validation as v1, the v1 table row above omitted the existing `scope` param, corrected here (closes a spec-table omission found during S-RPC-2 source grounding, not a new feature) |
| DELETE /v1/sessions/{id} | `sessions.delete` | `{id}` | `{"ok":true}` | **Added 2026-11-20: v1 route existed in SURFACE-SPEC v1 but was missing entirely from this table — an omission, not a deliberate exclusion. Found during S-RPC-2 source grounding (hard rule: wire contracts come from handler source, not planning docs). Mechanical 1:1 port, zero open design question: 404 `no_such_session` (unknown id), 403 `session_is_protected` (the main session can't be deleted), 409 `session_busy` (turn in flight) — exact same three-way switch `deleteSessionHandler` already implements.** |
| GET /v1/keys | `keys.list` | `{}` | `[{"name","status","sha256_8"}]` | unchanged |
| PUT /v1/keys/{name} | `keys.set` | `{name, key, force?}` | `{"ok":true}` | 404 `unknown_provider` (unless force) unchanged; body ≤4KiB unchanged — with no HTTP body on the socket, the 4 KiB cap applies to the `key` param value (`key_too_large`). Full v1 error set, same gate order (name first): `invalid_provider_name`, `unknown_provider`, `key_too_large`, `invalid_request` (unknown params rejected, matching v1's `DisallowUnknownFields`), `empty_key`, `key_store_full`, `key_store_write_failed`, `key_reload_failed`. Key material never appears in any error, log line, or echo. When the keys door is not attached, every `keys.*` call answers `method_not_found` — the socket analog of v1's "404 like any unknown path" |
| DELETE /v1/keys/{name} | `keys.remove` | `{name}` | `{"ok":true}` | idempotent, unchanged |
| GET /v1/custos/cards + catch-up | `custos.cards.subscribe` | `{}` | `{"cards":[…]}` then live `custos.card` and `custos.gone` notifications | replaces per-tab catch-up polling (CA-6, V16) with one subscription. Snapshot is exactly v1's: door-registry cards merged with pending turn-approval cards, de-duplicated by id, sorted by id. Live notifications mirror v1's two SSE event kinds 1:1 — `custos.card` (params = the frozen CustosCard shape) and `custos.gone` (params = `{id, state, reason}`); the table previously named only the first, but without `gone` a client can never retire a settled or expired card. Ordering: no custody notification is written before the subscribe result. A second subscribe on the same connection replaces the first (no double delivery). Per-connection buffer cap 64, as in v1: on overflow the server drops that connection's custody subscription and sends one `custos.resync` notification (no params) — the client re-calls `custos.cards.subscribe` for a fresh snapshot. v1 closed the SSE stream to force the same resync; closing the shared socket here would also tear down unrelated session subscriptions and deny their pending turn approvals. The subscription also serves as the device-approver presence gate (A9): while any subscription is live, headless-turn approvals mint and fan to this stream; when the overflow drop (or any detach) removes the **last** custody subscription, orphaned pending approvals with no other live approver settle `denied:disconnected` per A9(c). |
| POST /v1/custos/cards/{id}/resolve | `custos.cards.resolve` | `{id, verdict}` | `{"state":"approved"|"denied"}` | 409 `already_answered`/`stale_verdict`, 423 `locked` unchanged. Complete v1 error set (the row previously listed only those three): `not_found` (id fails the card-id gate), `invalid_verdict` (verdict not one of `once`/`always`/`deny`), `ip_ask_only` and `bad_source` (400), `no_such_card` (404, neither the door registry nor the turn-approval hub knows the id). Resolution order unchanged: door registry first, turn-approval hub fallback; an `always` verdict settled via the hub mints the card-minted rule after settling, and a mint failure degrades the verdict to once without flipping the ack |
| GET /v1/policy/rules | `policy.rules.list` | `{}` | `{"egress_strict","rules":[…]}` | unchanged, `rules` is `[]` never null |
| PUT /v1/policy/rules | `policy.rules.set` | `{lane, pattern, verdict}` | `{"ok":true,"notes":[…]}` | `always` still never settable here (card-minted only); audit actor `web` unchanged — both are enforced inside the wiring-layer PolicySource adapter (`always=false`, actor `web` hard-coded), so the method must call the same PolicySource, never the engine directly. v1's lane coercion is preserved (any lane other than `egress` becomes `credential`). Errors: `locked` (no source attached, or vault locked), `policy_corrupt`, `invalid_request` (unknown params rejected), `invalid_rule` (engine validation failure — v1 sent the engine's text as a 400 body; the same text rides `error.message`, `invalid_rule` is the stable machine reason). `policy.rules.remove` uses `invalid_rule` for the same engine-error case |
| DELETE /v1/policy/rules/{pattern} | `policy.rules.remove` | `{pattern, lane?}` | `{"ok":true,"removed":bool}` | unchanged. Same gates in the same order: pattern must match `^[\x21-\x7e]{1,256}$` else `not_found` (v1 answered 404 for a bad path segment), `lane` absent / `credential` / `egress` else `invalid_lane`, then `locked` / `policy_corrupt` from the engine |

### 4a. Folded in from #301 (model switcher — never built as REST)

| v2 method | params | result | notes |
|---|---|---|---|
| `models.list` | `{}` | `{"models":[{"ref","provider","status","probed_at"}]}` | replaces the never-shipped `GET /v1/models`; backed by the caps.json probe cache (S-MOD-1b scope — cmd/hearthd catalog wiring, 60s refresh — unchanged engineering, just exposed over RPC instead of REST) |
| `sessions.configure` | `{id, model_pin?, private?, expected_revision?}` | `{"ok":true, "applied":{"model_pin":"ok","private":"ok"}, "session":{…}}` | folds the pin set/clear from the never-shipped `PATCH /v1/sessions/{id}`; **fixes the CAS gap found on #301** — `expected_revision` is checked against the session header's revision counter, mismatch → error `reason:"revision_conflict"`, no silent clobber between two paired devices. `private` (bool) changes the session audience per SURFACE-SPEC §4b: creator only (`not_creator`), fixed on main (`session_shared`), non-boolean → `invalid_request`; checked before the revision, nothing written on failure |

### 4b. Folded in from #302 (persona/profile API — never built as REST)

| v2 method | params | result | notes |
|---|---|---|---|
| `profile.describe` | `{}` | `{"soul","soul_bytes","soul_budget","identity","identity_bytes","identity_budget","user","user_bytes","user_budget","memory","memory_bytes","revisions":{"soul":int,…}}` | single read, everything the editor needs, following the single-read/single-write profile-editor pattern established in prior art (see issue #302 discussion) |
| `profile.configure` | `{soul?, identity?, user?, memory?, expected_revisions?:{field:int}}` | `{"ok":true,"applied":{"soul":"ok",…}}` | any subset, per-field apply report, per-field CAS via `expected_revisions` — same pattern. `applied[field]` can be `"ok"` / `"over_budget"` / `"invalid"` / `"revision_conflict"` — a bad field doesn't fail the whole call. Budgets unchanged from `internal/loop/prompt.go` (SOUL 4096B / IDENTITY 2048B / USER 4096B / MEMORY ~8192B). Audit: actor=web, which fields changed, byte deltas — same discipline as `policy.rules.set`. |

### 4c. Session audience (SURFACE-SPEC §4b)

The caller id is bound once, from the token accepted by `auth.hello`, and
applies to every method on that connection. `sessions.list` omits sessions the
caller may not see. A hidden session answers as an unknown id:
`sessions.subscribe` / `sessions.send` / `sessions.approvals.resolve` →
`not_found`, `sessions.cancel` → `no_turn_in_flight`, `sessions.delete` /
`sessions.configure` → `no_such_session`. `sessions.subscribe` checks the
audience after registering, and a scope-down writes the header before
withdrawing registrations, so a subscribe racing a scope-down either sees the
new audience or is withdrawn by it; events held for a withdrawn subscription
are discarded.

## 5. What explicitly does NOT move

- **Backup/restore stays control-socket only.** SURFACE-SPEC v1 §3A's
  explicit no-REST-verb stance for `hearthd backup`/`hearthd restore`
  carries over unchanged — these were never a REST or RPC surface and
  this migration gives no reason to make them one.
- **`/v1/health` and static page assets** — see §1. Deliberately kept off
  the authenticated socket.

## 6. Slice table

| Slice ID | Scope | Repo | Effort |
|---|---|---|---|
| S-RPC-1 | WS transport skeleton: upgrade handler, envelope codec, `auth.hello`, connection lifecycle/close codes. No business methods yet. | `lararium` | M |
| S-RPC-2 | `sessions.*` methods + `session.event` notifications (the highest-value piece — replaces SSE). | `lararium` | L |
| S-RPC-3 | `keys.*`, `policy.rules.*`, `custos.cards.*` methods + `custos.card` / `custos.gone` / `custos.resync` notifications. | `lararium` | M |
| S-RPC-4 | `models.list` + `sessions.configure` (folds #301, incl. the revision-conflict fix) and `profile.describe`/`profile.configure` (folds #302). | `lararium` | M |
| S-RPC-5 | Android client: OkHttp+SSE → WS client, JSON-RPC envelope, reconnect/backoff, request/notification correlation. | `android-client` | L |
| S-RPC-6 | Web client: `fetch()` calls in `KeysDrawer.tsx`/`App.tsx`/etc → WS RPC client wrapper. Fill `MemoryViewer.tsx` and `Header.tsx`'s `modelPin` chip against the new methods as part of this slice, not separately. | `android-client`\* | M |
| S-RPC-7 | Remove every SURFACE-SPEC v1 REST handler, retire `docs/SURFACE-SPEC.md` (mark SUPERSEDED, point at this doc), bump daemon to `v2.0.0` (§7.4, major bump — wire-breaking for every existing client). Ships same release as S-RPC-5/6 — this is the hard-cutover point; there is no intermediate release with both transports live. | `lararium` | S |

\* `web/` lives in the daemon repo (`lararium`), not `android-client` — table
column kept consistent with ASSISTANT-PHASE-SPEC's existing repo-tag
convention; correcting here: S-RPC-6 touches `lararium` (the `web/` tree).

## 7. PO ratification (2026-11-20)

All four decided by following the leaning proposal as drafted — no overrides.

1. **Error code numbering — RATIFIED generic.** `error.code` stays
   generic/uninformative (always `-32000`, JSON-RPC's reserved
   "application error" range); all real signal lives in `error.data.reason`
   as the exact same string SURFACE-SPEC v1 used in its JSON bodies
   (`"invalid_model_pin"`, etc.) — minimizes churn in anything downstream
   that pattern-matches on those strings. No per-domain code range.
2. **`sessions.send` back-pressure — RATIFIED 409.** Calling `sessions.send`
   on a session the client hasn't subscribed to returns `409 not_subscribed`.
   No auto-subscribe — a client that forgot to subscribe gets a loud error,
   not silently-working behavior that hides the bug.
3. **Reconnect replay window — RATIFIED sufficient as-is.** Resuming via
   `sessions.subscribe {after_seq}` on reconnect is sufficient; no separate
   resume-token / crash-durable create-intent journal concept at this scale.
4. **Version bump — RATIFIED v2.0.0.** Major bump, not `v1.1.0` — this is a
   wire-breaking change for every existing client, and a major bump makes
   every `v1.0.0-betaN` tag unambiguously mean "REST". Applied at S-RPC-7
   (§6), the hard-cutover slice, not at spec-ratification time.
