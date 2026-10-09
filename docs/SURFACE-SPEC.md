# SURFACE-SPEC — the HTTP/SSE API and the web chat

Status: FROZEN — G1 approved by the Product Owner 2026-10-02 (port,
serving, model_pin, timeout decisions in §10). Freezes the transport between
the hearth (hearthd) and its clients, and the first client: a built-in web
chat. Changelog at bottom. Post-freeze amendments A1, A2, A4–A7 (approval
channels, audit enums, cancel semantics, MarkUndeliverable) and K-A1 (the
three /v1/keys routes) applied 2026-10-04 as approved at NUNTIUS-SPEC/
KEYS-SPEC G1 pass — see changelog; CA-5 (the `ctl` audit source,
CUSTOS-SPEC amendment v8) applied 2026-10-07 — see changelog v5.1;
CA-6 (custody-card fan-out: two /v1/custos/cards routes, the global
custos card stream, the render rule, V16; CUSTOS-SPEC amendment v9)
applied 2026-10-08 — see changelog v5.2; S10 (amendment A4 §S10.1, PO
sign-off 2026-10-09) adds §3A — whole-hearth backup/restore CLI shapes,
the control-socket `backup` verb, and the explicit no-REST-verb stance —
see changelog v5.3.
Companion specs: PENATUS-SPEC (the file layer — this API never contradicts
it), ARCHITECTURE §2.7 (surfaces), CELL-SPEC (execution stays sandboxed).

## 0. Scope

IN:  a loopback HTTP/1.1 + SSE API; token auth; session list/create/read;
     streaming turns; tool-approval round-trips; one embedded static page.
OUT: TLS termination (a reverse proxy or the tailnet does it), multi-tenancy,
     web-push, PWA manifest, connectors, custos, WebSocket, OAuth, CORS.

## 1. Principles (load-bearing)

P1. **The API is the tree.** Every operation maps to a penatus primitive:
    sessions append events; files replace atomically (§PENATUS 5). The API
    adds no hidden state of its own — no server-side queues, no drafts
    table, no shadow session. If it isn't in the tree, it didn't happen.
P2. **The CLI is just a client.** The REPL keeps working unchanged; the web
    chat uses only endpoints a third party could call with a token.
P3. **Persistence stays hearth-side.** Streaming deltas are never persisted
    mid-stream (PENATUS §5); a client that drops mid-turn loses nothing and
    the completed turn lands in the log exactly once.
P4. **Single trust domain, zero trust transport.** One owner, but the wire
    is still authenticated: every request needs a bearer token, including
    loopback. No cookies anywhere → no CSRF surface.

## 2. Lifecycle & transport

- `hearthd serve --config <path> [--listen 127.0.0.1:7717]` starts the API
  + embedded page. Default bind is **127.0.0.1 only**; any other bind
  requires the operator to pass it explicitly (loud log line at startup).
- HTTP/1.1, no TLS inside the process. Remote use = tailnet or a reverse
  proxy that terminates TLS; the docs say so, the code does not pretend.
- **Host gate:** every request's `Host` header, after stripping the port,
  must name `localhost`, `127.0.0.1`, `::1`, or an entry of
  `serve.allowed_hosts` (config list; required to be non-empty when
  binding non-loopback — the daemon refuses to start otherwise). Any port
  is accepted on those hosts (browsers send `Host: localhost:7717`).
  Anything else → 403. This closes DNS-rebinding: a rebinding attack
  arrives with the attacker's domain in `Host`, which is never listed.
  Reverse-proxy/tailnet use = put your hostname/tailnet IP in
  `allowed_hosts` (documented in the serve help). Proxy caveat: the
  ready-URL fragment never reaches a proxy log (fragments are not sent to
  servers), but the bearer token DOES appear in `Authorization` headers —
  proxies must be configured not to log request headers. Proxies must also
  not log **request bodies** for `PUT /v1/keys/{name}`: those bodies carry
  provider keys (K-A1, KEYS-SPEC).
- **Concurrency gate:** an atomic per-session check-and-set performed at
  request acceptance, before any I/O: at most one in-flight
  `POST …/messages` per session; a second concurrent POST gets 409. The
  gate covers ONLY that route — approval decisions, event reads, and other
  sessions are never blocked by a running turn. Different sessions may run
  concurrently. A stalled turn cannot hold a session forever:
  `serve.turn_timeout` (default 10 min) aborts the turn (engine `error`,
  gate released), and `POST …/cancel` (§4) ends it on demand.
- All bodies ≤ 1 MiB (413 otherwise); JSON `application/json` UTF-8.
- **Shutdown:** SIGTERM stops accepting, lets in-flight turns reach their
  terminal event for up to 60 s, then force-exits. A force-exit (or crash)
  discards uncommitted partial turns — the append-only log never holds a
  half-written assistant event. Pending approvals resolve `denied:shutdown`
  immediately at SIGTERM (no waiting out their timeout).

## 3. Auth

- Tokens: `hearthd token create <label>` prints `lar1_<32 base62>` once
  (plus the ready URL `http://127.0.0.1:7717/#lar1_…` for the operator to
  open) and stores only SHA-256(token) + label + created in
  `<hearth>/tokens.json` (0600, atomic replace). No listing of values,
  ever. The plaintext exists exactly twice: that terminal, and the
  operator's browser — the daemon (`serve`) never sees or prints it.
- Requests carry `Authorization: Bearer lar1_…`. Compare in constant time
  (`crypto/subtle`). Failure → 401 `{"error":"unauthorized"}` (no hints why).
- `hearthd token revoke <label>`; the daemon re-reads `tokens.json` per
  request, so revocation takes effect on the **next request**. An
  already-streaming SSE connection is not retroactively cut (documented
  trade-off: cutting mid-turn would violate P3; the turn completes and
  persists, later requests 401).
- The embedded page reads its token from the URL fragment once
  (`/#lar1_…`), immediately strips it from the address bar via
  `history.replaceState`, and keeps it in `sessionStorage` — tab-scoped,
  gone when the tab closes, so a reload (F5, mobile tab reclaim) keeps
  working and catch-up via §5 stays true. Persistent storage
  (`localStorage`) and cookies remain banned outright.

## 3A. Whole-hearth backup & restore CLI (amendment S10.1, 2026-10-09)

Format, consistency, restore, and swap law live in PENATUS-SPEC §4.5;
this section freezes only the surface shapes. Command set (frozen):

```
hearthd backup create [--out <path>] [--no-config]
hearthd backup list <file>
hearthd backup verify <file>
hearthd backup extract-config <file> <path>
hearthd restore <file> --to <dir>             # fresh root; <dir> must not exist
hearthd restore <file> --replace <dir> --yes [--no-safety]
```

Stdout data / stderr errors / exit codes per frozen conventions (§0 area
rules). `list` emits `#columns: path size sha256 mode` then one row per
manifest entry (dirs: size/sha256 = `-`). `verify` output schema per
PENATUS §4.5 (`#columns: class path detail`, findings on stdout only).
`extract-config` writes the bundled yaml **mode 0600**, refuses an
existing `<path>`, and errors clearly on `--no-config` bundles.

**Control-socket verb.** With the daemon running, `backup create`
resolves `--out` to an absolute path and invokes the control-socket verb
`backup <out_abs> [--no-config]` — single request line, ack, progress
lines, done/error — per PENATUS §4.5 §3.1. With the daemon stopped the
CLI walks the tree itself under the §4.5 §3.3 flocks; a
present-but-dead `hearthd.sock` refuses with the operator step.

**No REST endpoint, no web verb, ever in v1** — restore replaces the
whole state root, so it cannot be safely driven from the very surface it
replaces. Stated here so future PRs must amend this spec, not creep.

## 4. Endpoints (v1 — frozen set)

| Method | Path | Effect |
|---|---|---|
| GET  | /v1/health | 200 `{"ok":true,"version":…}` — the only unauthenticated `/v1/` route |
| GET  | /v1/sessions | list session.json headers, newest first |
| POST | /v1/sessions | create side chat `{title?, model_pin?}` → header |
| GET  | /v1/sessions/{id}/events?after_seq=N&limit=M | `{"events":[…], "in_flight":bool}` (§5 catch-up); default M=200, hard cap M=1000 (clamp); invalid limit/after_seq → 400 |
| POST | /v1/sessions/{id}/messages | `{text}` → SSE stream (§5) |
| POST | /v1/sessions/{id}/cancel | abort the in-flight turn: engine stops, gate released, NO assistant event persisted; 404 if none in flight |
| POST | /v1/sessions/{id}/approvals/{approval_id} | `{decision:"allow"|"deny"}` → 200; 410 if already resolved/expired |
| GET  | /v1/keys | `[{"name","status","sha256_8"}]` — provider-key inventory; names = config ∪ keys.json computed at request time (K-A1, KEYS-SPEC K4) |
| PUT  | /v1/keys/{name} | `{"key":"…"}` → 204; 404 unknown provider unless `?force=true`; body ≤ 4 KiB; `{name}` gate per S4, 404 `{"error":"invalid provider name"}` (K-A1, KEYS-SPEC K4) |
| DELETE | /v1/keys/{name} | 204 — removes the keys.json entry only; idempotent (K-A1, KEYS-SPEC K4) |
| GET  | /v1/custos/cards | `{"cards":[…]}` — hearthd's door-registry pending set, CUSTOS-SPEC §6.4a wire shape; catch-up after refresh/new tab (CA-6, CUSTOS-SPEC §6.4b) |
| POST | /v1/custos/cards/{id}/resolve | `{"verdict":"once"|"always"|"deny"}` → 200 `{"state":"approved"|"denied"}`; 404 no_such_card; 409 already_answered\|stale_verdict; 423 locked; 400 ip_ask_only. id gate: `^a_[0-9A-Za-z]{10,32}$` (same as S4). hearthd forwards the verb to doors.sock with `via web` and relays the synchronous ack — door `OK` maps to 200 `{"state":"approved"}` for `once`/`always` and `{"state":"denied"}` for `deny`; every `ERR` maps to its frozen status, 1:1 (CA-6) |
| GET  | / | embedded page HTML — static, unauthenticated (it contains no secret; every API call it makes needs the token) |
| GET  | /app.js, /app.css | embedded page assets — static, unauthenticated, immutable (same rule: no secret in them) |

- Static routes (`/`, `/app.js`, `/app.css`) are the only non-`/v1/`
  routes; they are served read-only from the embedded FS and carry the CSP
  header. "Unauthenticated" here means "no bearer token" — the Host gate
  (§2) still applies to every request.

- Session ids match `^s_[0-9A-Z]{26}$` or the literal `main`; approval ids
  match `^a_[0-9A-Za-z]{10,32}$`. Anything else → 404, never a path lookup
  (no traversal).
- Creating a session writes the penatus header (`kind:"side"`,
  `parent:"main"`, `branch` event per PENATUS §2) — the API never invents
  a format the file spec doesn't already define.
- `main` cannot be created or deleted over the API; it exists or the
  daemon misconfigured. No delete endpoint in v1 (archive is a file-layer
  rename, PENATUS §2, out of scope here).
- Errors: `{"error": string}` + correct status (400/401/404/409/413/500).
  500 never echoes internals.

## 5. Streaming protocol (SSE)

`POST …/messages` with a valid token returns `text/event-stream`; each
event is `event: <type>` + `data: <json>`. Types (frozen):

| event | data | when |
|---|---|---|
| `delta` | `{"text":…}` | each streamed chunk (NOT persisted) |
| `tool_start` | `{"call_id", "name", "args_summary"}` | dispatch begins |
| `approval_request` | `{"approval_id", "name", "args_summary"}` | untrusted tool awaits decision |
| `tool_done` | `{"call_id", "ok", "approval"}` | outcome (digest only, ≤ 1 KiB) |
| `turn_done` | `{"seq", "in_tokens", "out_tokens"}` | turn persisted; seq = assistant event's |
| `error` | `{"error":…}` | terminal for this stream |

- Exactly one of `turn_done` / `error` closes every stream; the server
  closes the connection after it. The server writes an SSE comment line
  (`: ping`) every 5 s while a stream is open — liveness for proxies, and
  the hook by which dead clients are noticed; server-side disconnect
  detection is the request context's cancellation (mandated, not left to
  chance).
- **Global custody-card stream (CA-6, CUSTOS-SPEC §6.4b):** the frozen
  table above covers turn streams only. Custody cards are daemon-wide,
  so they ride a **global** SSE stream —
  `GET /v1/custos/cards/events` (same bearer-token gate, same `: ping`
  liveness rule) carrying event types `custos_card` (= the doors.sock
  `CARD` frame, §6.4a wire shape) and `custos_gone` (= the `GONE`
  frame, `{id, state, reason}`). This stream never carries turn
  content and never closes on a turn boundary; it lives for the
  connection's lifetime.
- **Approval state machine:** each approval is `pending → approved | denied
  | timed_out | shutdown`, transitions guarded by a mutex, first-wins.
  Approval ids are session-scoped: `{id}` in the URL must belong to the
  session in the URL — unknown or foreign → 404; known-but-resolved →
  410 (no double-resolve, no false 200). Timeout is chosen at creation
  from the live approver channel set (A6): a web SSE listener is live →
  `serve.approval_timeout` (web wins ties); a paired nuntius bridge is the
  only live channel → `nuntius.approval_timeout` (default 5 min, same
  bounds and test-shortening rule). (A1: "live approver channel"
  generalizes "SSE listener" — a paired nuntius bridge counts, unless
  that approval's card was marked undeliverable (`card_dead`,
  NUNTIUS-SPEC §7.1); web-only behavior is unchanged when nuntius is
  disabled.) Timeout → `timed_out` (logged `denied:timeout`).
- **Cancel resolves pending approvals (A5):** when a turn is cancelled
  (`POST …/cancel` or nuntius `/cancel`), every approval still pending on
  that turn resolves to `denied` immediately, same mutex, same
  410-after-terminal semantics; the hub's internal transition event
  carries cause `cancelled` (`denied:cancelled`) so fan-out subscribers
  can render it distinctly. No audit `reason` value is needed: like every
  cancelled turn (V15), this writes no audit record — the tool never ran,
  and the absence of a tool_result IS the record. An approval whose turn
  no longer exists is meaningless; leaving it pending would invite a late
  click to authorize a tool call for a dead turn.
- **MarkUndeliverable (A7):** `MarkUndeliverable(approval_id)` —
  in-process bridge call — either resolves `pending → denied` with
  `source:"hub"`, `reason:"undeliverable"` when no other live approver
  channel exists for that approval, or marks the card dead (`card_dead`)
  and leaves the state pending. `denied:undeliverable` is terminal like
  every denial; the mutex and first-wins rules are unchanged.
- **Disconnect policy:** an approval can only be pending while a client is
  listening — if the SSE connection is not open at the moment an approval
  would be created, or drops while one is pending, it resolves
  `denied:disconnected` immediately (no wait, no freeze). The turn
  continues (the model call completes and persists per P3) with that tool
  denied. A reloaded client catches up via `GET …/events`.
  **Custody-card exemption (CA-6):** this policy does NOT apply to
  custody cards on the global stream — S6/S9/V6b never fire for them.
  Custody cards live in custosd's hub, not a turn; closing a browser tab
  must not (and cannot) settle a vault decision. The card survives the
  disconnect and re-renders from `GET /v1/custos/cards` on the next
  load; the hub's own hold timeout remains the only auto-deny path.
- **Runaway cap lives in the turn engine, not the writer:** the engine
  counts generated bytes (deltas + tool payloads) and aborts the turn at
  4 MiB with an `error` event — enforced even if nobody is watching, so a
  disconnected client cannot let a runaway loop burn tokens forever. An
  aborted turn persists NO assistant event (same no-half-written rule as
  §2) and releases the session gate immediately.
- **Catch-up protocol (the reload path — this is what makes disconnect
  recovery real):** persisted vocabulary = the penatus event set (user,
  assistant, tool_call, tool_result); stream-only = `delta`,
  `approval_request`, `tool_start` (their outcomes persist as
  tool_result). `GET …/events` responses carry
  `{"events":[…], "in_flight": bool}` — `in_flight` is the live gate
  state, so a reloaded client distinguishes "turn still running" from
  "daemon died mid-turn" (absent assistant event + `in_flight:false` =
  turn was lost, nothing more to wait for). Protocol after reload: replay
  from `after_seq=0` (clamped), render, then poll `…/events` every 2 s
  while `in_flight` is true until the assistant event appears. Reattaching
  to a live SSE stream is NOT supported in v1; polling is the contract.

## 6. Approvals, honestly

The web approver replaces the REPL's stdin prompt 1:1 — same Tool engine,
same deny-by-default, same audit events in the log. The API adds a waiter
between `loop.Approver` (sync) and the HTTP decision endpoint: `Approver`
registers the approval in the §5 state machine, emits the SSE event, and
blocks on a channel with timeout. No approval state crosses process restart (a
restart kills the turn; the log shows the tool never ran).

**Audit schema (frozen):** every tool_result event carries
`"approval": {"decision": "allowed"|"denied", "reason": "ok"|"timeout"|
"disconnected"|"shutdown"|"undeliverable"|"not_required", "source":
"repl"|"web"|"telegram"|"ctl"|"timer"|"shutdown"|"hub"}` (A2: `timer` and
`shutdown` are hub-caused terminal states, not client surfaces; `hub`
covers hub-initiated denials with a policy cause, e.g. `undeliverable`.
A4: `reason:"undeliverable"` pairs with `source:"hub"` so a delivery-
failure denial is schema-valid and self-describing. CA-5 (CUSTOS-SPEC
amendment v8): `ctl` is a verdict delivered through custos' control
socket — the standalone approval door, CUSTOS-SPEC §6.4a). Both
clients write through the same engine; verification diffs each client's
log lines against THIS schema (field set + types), not against each
other's bytes — `source` is the only field allowed to differ.

**Custody cards on the page (CA-6, CUSTOS-SPEC §6.4b).** Custody cards
render on the main chat page in a distinct card style (one place to
answer everything), with the frozen CUSTOS-SPEC §6.5 button set —
Allow once / Always / Deny — wired to `POST /v1/custos/cards/{id}/resolve`
(`once|always|deny`). They are not turn approvals: no session scope, no
§5 state machine, no disconnect auto-denial (see §5's exemption).
**No optimistic resolution:** the UI settles a card only on the resolve
ack or an incoming `custos_gone`; a lost race (409) re-renders the card
as already answered from the ack/GONE payload — a card never silently
vanishes and never shows a verdict the hub did not record.

## 7. The embedded page

Three static files (Go `embed`: HTML, `/app.js`, `/app.css`), vanilla JS,
no build step, no framework, no CDN. Left rail: session list (main +
sides, `+` button). Main pane: transcript (rendered from events,
markdown-lite: code fences + bold/italic only), input box, streaming
deltas. Tool lines render collapsed with status; `approval_request`
renders Allow/Deny buttons wired to §4.
**Rendering is XSS-safe by construction:** model/tool text is inserted via
`textContent` only (no `innerHTML` on untrusted data, ever); the server
serves the page with `Content-Security-Policy: default-src 'self';
style-src 'self'; frame-ancestors 'none'` and `X-Content-Type-Options:
nosniff` — JS and CSS live in the separate embedded files (`/app.js`,
`/app.css`), so no inline script or style exception is needed. The only
secret in the page is the token held in `sessionStorage` (tab-scoped);
page is useless without it, which is the point.

## 8. Security invariants (each maps to a test)

S1. Bind default is loopback; non-loopback bind logs a warning every start.
S2. Every `/v1/` route except `/v1/health` 401s without/with-wrong token;
    static routes (§4) carry no secrets and no session state. Token
    compare uses `crypto/subtle` (unit-checked, not curl-checkable).
S3. No Set-Cookie anywhere; no CORS headers at all; `Origin` ignored;
    `Host` header gated (§2) against DNS rebinding.
S4. Session/approval id regex gate → no filesystem traversal possible
    (404 fuzz).
S5. Token file 0600; values never on disk, in daemon logs, or in SSE
    payloads.
S6. Approval timeout denies; disconnect denies immediately; double-resolve
    returns 410; SIGTERM resolves pending as `denied:shutdown`. No tool can
    stay permanently pending.
S7. Body cap 1 MiB; the turn engine aborts at 4 MiB generated per turn
    (client-connected or not) with an `error` event.
S8. The page never sends the token to any origin but its own (no absolute
    URLs, no external refs — grep-enforced in CI); CSP
    `default-src 'self'; style-src 'self'; frame-ancestors 'none'` +
    `X-Content-Type-Options: nosniff` served on `/`; untrusted text
    rendered via `textContent` only; token lives only in `sessionStorage`
    (tab-scoped) — never `localStorage`, never cookies, never logs.
S9. A session is never frozen: pending approvals resolve on disconnect
    (≤2 s), turns end by `turn_timeout` or `POST …/cancel`, and a reloaded
    client always learns the truth via `in_flight` (§5 catch-up).

## 9. Verification suite (numbered; CI + live)

V1. `hearthd serve` default bind: `ss -ltn` shows 127.0.0.1 only; with
    `--listen 0.0.0.0:7717` the bind is open AND the warning line appears
    in stdout. (S1)
V2. curl matrix: no token → 401; wrong token → 401; health → 200;
    `Host: evil.example` → 403. Constant-time compare proven by unit test
    on the compare helper (crypto/subtle), not by curl. (S2/S3)
V3. `curl -v` on / and one /v1 route: no Set-Cookie, no Access-Control-*;
    `/` carries `Content-Security-Policy: default-src 'self'; style-src
    'self'; frame-ancestors 'none'` and `X-Content-Type-Options: nosniff`
    (exact header match). (S3/S8)
V4. ids `..`, `..%2f`, `s_../..`, empty, bad approval id → 404. (S4)
V5. token create prints the plaintext once; `grep lar1_ tokens.json`
    empty; file mode 0600; the plaintext token appears in NEITHER the
    `serve` daemon's stdout/stderr NOR any SSE frame of a subsequent turn.
    (S5)
V5b. revoke round-trip: create → use (200) → revoke → same token → 401. (S2)
V6. trigger untrusted tool, no decision, wait past `approval_timeout`
    (shortened in the test config) → tool_done ok:false approval
    denied:timeout; log shows the denial. (S6)
V6b. disconnect mid-approval → approval resolves `denied:disconnected`
    within 2 s (not the full approval_timeout); turn still persists. (S6)
V6c. double-resolve: allow, then allow again → second POST → 410. (S6)
V7. 2 MiB body → 413; fake model emitting > 4 MiB deltas with NO client
    attached → turn aborts with error, engine stops generating. (S6/S7)
V8. Live: POST message with fake-LLM (httptest) → event order
    delta*→turn_done; events.jsonl gains exactly user+assistant events;
    deltas absent from the log. (P3)
V9. Live: two sessions stream concurrently; same session, two POSTs fired
    CONCURRENTLY (no sleep between) → exactly one 200-stream, one 409
    (atomic gate); approval POST to a busy session is NOT 409 (gate
    covers messages only). (§2)
V10. Live: approval allow round-trip → tool runs; tool_result carries the
    §6 audit schema; a REPL-run and a web-run of the same tool both
    validate against the schema (field set + types; only `source`
    differs). (§6)
V11. Page source: zero external URLs (grep also catches protocol-relative
    `//host` and `fetch(` with non-relative literals), zero `localStorage`
    usage (only `sessionStorage` for the token), zero `innerHTML`
    assignments, token never passed to `console.*`, `replaceState` present.
    (S8)
V12. SIGTERM mid-turn → pending approvals resolve denied:shutdown
    immediately; process exits ≤ 60 s; log has no partial assistant
    event (append-only integrity holds). (S6/§2)
V13. Host gate with config: bind loopback, request `Host: localhost:7717`
    → 200; bind `0.0.0.0` with empty `allowed_hosts` → startup refused;
    with `allowed_hosts: [hearth.example.com]`, that Host → 200,
    `evil.example` → 403. (S1/S3)
V14. Catch-up: fake-LLM turn with slow deltas; disconnect mid-turn, reload
    path = `GET …/events` → `in_flight:true` while running, assistant
    event appears after completion, `in_flight:false` after; kill daemon
    mid-turn, restart → `in_flight:false` and no assistant event (lost
    turn distinguishable from running turn). (S9/§5)
V15. Cancel: mid-turn `POST …/cancel` → stream closes with `error`, no
    assistant event persisted, immediate follow-up POST to same session →
    200 (gate released); cancel with no in-flight turn → 404. (S9)
    Hub-side assertion (A5): a cancel with an approval pending on that
    turn resolves it to `denied` (internal cause `cancelled`) before the
    turn's own terminal event; a later decision POST → 410.
V15b. Turn timeout: config `turn_timeout` shortened, fake model that never
    finishes → turn aborts with `error`, gate released (follow-up POST →
    200). (S9)
V15c. Events clamp: seed >1000 events, `?limit=5000` → 200 with ≤1000
    events; `?limit=0`/negative/garbage → 400. (§4)
V15d. Approval id scope: well-formed approval id from session A posted
    under session B → 404; heartbeat: open stream with idle model emits
    `: ping` comment ≥ once per 6 s. (§5)
V16. Custos cards (CA-6, CUSTOS-SPEC §6.4b): `GET /v1/custos/cards` →
    200 `{"cards":[…]}` (§6.4a wire shape); `POST /v1/custos/cards/{id}/resolve`
    status matrix — 200 `{"state":"approved"}` (once/always, door ack `OK`),
    200 `{"state":"denied"}` (deny), 404 no_such_card, 409
    already_answered|stale_verdict, 423 locked, 400 ip_ask_only; malformed
    id (fails `^a_[0-9A-Za-z]{10,32}$`) → 404 (S4 gate);
    `GET /v1/custos/cards/events` is global (survives turn boundaries,
    never carries turn events) and delivers `custos_card` on attach-
    concurrent register and `custos_gone` on every terminal transition;
    disconnect-auto-denial exemption: close the stream while a card is
    pending → the card stays pending in the hub (no `denied:disconnected`
    anywhere), and `GET /v1/custos/cards` after reconnect still lists it.
    (§4/§5/§6)

## 10. PO decisions (settled 2026-10-02)

Q1. Port **7717** — approved as default.
Q2. Page served at `/` on the same origin as the API — approved.
Q3. `model_pin` API-only in v1 (header field exists; UI later) — approved.
Q4. Approval auto-deny timeout **5 minutes** (`serve.approval_timeout`,
    default 300 s) — PO-set.

---

## Changelog

**v5.3 (amendment S10.1 / A4, 2026-10-09).** PO sign-off on amendment
A4/V10/S10 (whole-hearth backup & restore; four hostile review rounds,
converged 0 blockers): new §3A freezes the `hearthd backup …` /
`hearthd restore …` CLI shapes, the control-socket `backup` verb, and
the explicit no-REST-verb stance for v1. No frozen HTTP/SSE behavior
changed.

**v5.2 (amendment CA-6, 2026-10-08).** CUSTOS-SPEC amendment v9's
fan-out applied per its PO sign-off: §4 route table gains
`GET /v1/custos/cards` and `POST /v1/custos/cards/{id}/resolve`
(status-code contract 200/400/404/409/423, id gate same as S4);
§5 gains the global `GET /v1/custos/cards/events` stream
(`custos_card`/`custos_gone`) and the custody-card exemption from
disconnect auto-denial (S6/S9/V6b do not apply); §6 gains the
custody-card render rule (main page, distinct style, §6.5 button set,
no optimistic resolution); §9 gains V16. No frozen turn-stream behavior
changed.

**v5.1 (amendment CA-5, 2026-10-07).** §6 audit `source` enum gains
`ctl` per CUSTOS-SPEC amendment v8 CA-5: a verdict delivered through
custos' control socket (the standalone approval door, CUSTOS-SPEC
§6.4a), parallel to `web` and `telegram` — the symmetric completion of
CA-3. No frozen web behavior changed.

**v5 (amendments, 2026-10-04).** Applied the amendments approved at
NUNTIUS-SPEC G1 (A1, A2, A4–A7) and KEYS-SPEC G1 (K-A1): §5 presence rule
generalized to live approver channels incl. paired nuntius bridge, with
per-approval `card_dead` refinement (A1); §6 audit `source` enum extended
`telegram|timer|shutdown|hub` (A2) and `reason` enum `undeliverable` (A4);
§5 cancel resolves the turn's pending approvals before its terminal event,
V15 gains the hub-side assertion (A5); §5 timeout selection by live channel
set (A6); §5 `MarkUndeliverable` transition (A7); §4 route table gains the
three `/v1/keys` routes and §2 proxy note gains request-body logging for
them (K-A1). No frozen web-only behavior changed.

**v4 (freeze).** Second-opinion reviewer (fresh context) verified all
round-1 areas FIXED and added: catch-up protocol was undefined — now
specified (persisted vs stream-only vocabulary, `in_flight` flag on
`GET …/events`, poll contract, V14); session gate made atomic with
`turn_timeout` + `POST …/cancel` escape hatches (V15/V15b); SSE 5 s
heartbeat + mandated context-cancellation disconnect detection (V15d);
approval ids scoped per session, 404-vs-410 defined (V15d); events
`limit` clamp + invalid-input behavior tested (V15c); audit schema frozen
in §6 (V10 rewritten to diff against the schema, not the REPL); CSP gains
`frame-ancestors 'none'` + `nosniff` (V3); proxy header-logging caveat
(§2); V9 fires POSTs concurrently; V11 grep hardened. PO settled Q1–Q4
(§10). Status → FROZEN.

**v3 (G1 round 3).** Round-3 review: all 6 round-2 fixes verified; 2 new
blocking + 3 minor, all applied: Host gate now uses an explicit
`serve.allowed_hosts` allowlist (wildcard bind + tailnet/reverse-proxy
Hosts were 403-locked out; non-loopback bind without the list refuses to
start); token survives reload via `sessionStorage` (in-memory-only +
banned storage + stripped fragment bricked the page on F5 — catch-up
guarantee was dead); health-route "ONLY" wording scoped to `/v1/`; S8/V3
CSP strings aligned with §7; §7 file count corrected; V13 added for the
allowlist matrix.

**v2 (G1 round 2).** Round-2 review (independent model): all 8 round-1
areas verified FIXED; 4 new blocking + 2 minor found, all applied: static
asset routes added (`/app.js`, `/app.css` — the CSP-forbids-inline +
frozen-set-with-no-asset-route deadlock made the page dead on arrival);
ready-URL printing moved from `serve` to `token create` (the daemon cannot
and must not print plaintext tokens); Host gate now strips the port before
matching (browsers send `Host: localhost:7717`); disconnect policy covers
approvals created AFTER a disconnect (no 120 s freeze window); S2 scoped
to `/v1/` (static page is legitimately tokenless); CSP gains explicit
`style-src 'self'` (inline styles would otherwise be blocked with no CSS
route). V5 rewritten to match the token-create flow.

**v1 (G1 round 1).** Independent review returned 8 blocking + 9 minor
findings; all triaged and applied: concurrency gate narrowed to the
messages route (approvals must never 409); shutdown semantics unified
(SIGTERM → deny-pending immediately, 60 s drain, no partial events);
approval resolution is a mutex state machine with 410 on double-resolve;
disconnect auto-denies at once (no 120 s session freeze); revocation
semantics pinned to next-request; runaway cap moved from SSE writer to
turn engine (works with no client); Host-header gate added (DNS
rebinding); CSP + textContent-only rendering mandated (XSS via model
output); S2 health-route exception; fragment token stripped via
replaceState; limit clamp; approval_id regex; V-suite extended to match
(V5b, V6b, V6c, and strengthened V1–V3, V5, V7, V9, V11, V12).

**v0.** Initial draft.
