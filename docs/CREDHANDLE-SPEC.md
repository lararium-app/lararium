# CREDHANDLE-SPEC — credentials an agent may use without seeing (T1)

Status: **FREEZE-PROPOSED (v1)** — merging this document freezes the design
(issue #196; PO directive 2026-10-30). Amendments follow the CUSTOS-SPEC
append-only protocol. Implementation is tracked separately and MUST land the
V-T1 suite below as its exit criterion.

Companions: CUSTOS-SPEC (vault, surrogates, policy, audit), SURFACE-SPEC
(phone admin), PENATUS-SPEC (events as context).

## Purpose

T1 (ROADMAP): a credential an agent may **USE without SEEING**. The phone UI and the model context must never contain the secret; a named handle does the work at the doors, with audit and per-handle rate limits. This issue freezes the design so the drafting lane implements one coherent shape instead of inventing one per file.

## Inventory (verified on main, 2026-10-30)

- ALREADY: age-encrypted vault with passphrase+keyfile envelope (internal/custos/vault.go:255,369; crypto.go:75) — credentials keyed by NAME, `${vault:X}` resolved at connector compose (mcp_stdio.go:150-158, mcp_http.go:66-80).
- ALREADY: opaque surrogates (`sur_`+22 base62) bound to credential name + host/path (surrogates.go:26-44); swapped into plaintext-HTTP bearer requests only (proxy.go:634-673,1182-1217). CONNECT is documented blind passthrough asserted by V10 (proxy.go:548-558). **This stays true — no TLS MITM, ever.**
- ALREADY: hash-chained audit writing names + 8-hex fingerprints, never tokens (audit.go:20-33; surrogates.go:41-44).
- ALREADY: needle scrub on the MCP rail: resolved secrets become needles, results + structured content + stderr scrubbed (mcp_rail.go:412-416; mcp_result.go:29-128; mcp_lifecycle.go:132-155).
- GAP-A (the real hole): session events record tool args and result text VERBATIM (loop/session.go:290-340) and events ARE the model context (session.go:64-160) — zero scrub there.
- GAP-B: no per-handle rate limiting anywhere in custos.
- GAP-C: no phone admin surface for credentials (`/v1/credentials` does not exist in internal/surface/handlers.go); admin is CLI-only.

## Decisions (design freeze)

**D1 — Substitution happens at the ACTION door, not the network door.** The agent never receives a secret and never authenticates itself. A connector tool call may name a handle; the daemon resolves handle→secret inside its own process and performs the authenticated action, exactly as `${vault:X}` resolution works today — per-call, audited, rate-limited. HTTPS logins run from the connector's HTTPS client inside the daemon/worker trust boundary. The proxy keeps V10 blind CONNECT; we do not MITM TLS and we do not install CAs.
**D2 — The handle is the grant record, the name stays the policy identity.** A handle (opaque, surrogate-shaped) wraps: credential name + scope pattern using the EXISTING grammar (`name|name/tool/name@host[:port]`, policy.go:452-453) + optional TTL + rate limit. Policy rules keep referring to names only; domination/comparator machinery is untouched. Handles never appear in policy rules.
**D3 — Loop-layer redaction via injected hook; no loop→custos import.** The Session gains an optional `Secrets func() []string` set by wiring; `appendEvent` (session.go:349) replaces every needle occurrence with `[redacted]` in the args and result-text fields before the event is committed. Loop stays mechanism-agnostic (frozen architecture unchanged); daemon wires the current vault needle set (credentials resolved in-session + surrogate tokens). Scrub is applied at WRITE time, so events.jsonl is clean on disk, not just on render — and every future rail inherits it.
**D4 — Rate limits: token bucket per handle, enforced at both doors** (rail resolve + surrogate swap): `RateLimit{n, window}` on the grant record, default unset = unlimited; denial audits `handle_denied_rate` and the rail returns a neutral refusal line (no retry hints).
**D5 — Phone admin surface, admin-scoped token only:** `GET /v1/credentials` → list of `{handle, name, kind, last4, created, expires, rate}`; `POST /v1/credentials` → write-only (secret in body, consumed once; response contains handle + last4 ONLY — the value never appears in any response, log, or event); `DELETE /v1/credentials/{handle}` → revoke. last4 captured at write time as a display field.
**D6 — "Daemon keystore backing" means the custos age vault.** internal/keystore (plaintext keys.json) is provider-API-key plumbing and does NOT grow handles/encryption; do not unify the two in this item.

## Requirements

R1. Handle store in the vault (age-encrypted, alongside surrogates.age): create/list/revoke API in Vault + CLI (`custos handle add|list|revoke`), print handle + fingerprint only, never the secret.
R2. Rail resolve path accepts `credential: <handle>` in tool args (mcp path today); resolution consults the handle record: exists, unexpired, scope matches the connector/tool being called (reuse MatchesBearerAt/MatchesWorkerCall comparators), rate allows. Any failure → the tool call proceeds WITHOUT the credential and the audit records the denial kind; the failure text never contains the handle's secret or the resolved value.
R3. Loop redaction hook per D3; wiring in cmd/hearthd passes the live needle set. A redaction miss at appendEvent is a spec violation, not a bug class to tolerate.
R4. Admin endpoints per D5 behind the existing bearer auth; non-admin tokens get 403.
R5. Audit kinds: `handle_created`, `handle_used`, `handle_denied_scope`, `handle_denied_rate`, `handle_revoked`. Payload fields carry handle + name + fingerprint only.

## Exit criterion (V-T1 suite, all in one CI job)

- V-T1a: a web-login connector (fixture HTTPS test server) authenticates using a vault handle; a redaction test greps events.jsonl, daemon log, and the exact messages() payload sent to the model for the secret string (and its base64 form) → ZERO occurrences. This is the roadmap exit criterion; make the fixture the test.
- V-T1b: write-only POST: the create response body and all audit/event lines contain last4 but never the full value.
- V-T1c: rate limit: n+1th call in-window is refused without the secret, audits handle_denied_rate.
- V-T1d: scope: handle bound to tool A is refused for tool B (handle_denied_scope).
- V-T1e: V10 (blind CONNECT) still passes untouched.
- V-T1f: needle injected as a tool RESULT from a non-custos rail (fixture) appears in events.jsonl only as [redacted], and messages() replay shows [redacted].

## Out of scope

OAuth token refresh flows, per-device keystore migration, biometric-gated use, secrets in notifications, MITM of any kind, changes to internal/keystore.

## Out of scope (T1)

- TLS interception or CA installation — permanently out (V10 doctrine).
- Per-cell credential scoping (roadmap beyond T1).
- Unifying custos vault with internal/keystore provider plumbing (D6).

## Changelog

- v1 (2026-10-30): initial freeze text, verbatim from issue #196 decisions.
