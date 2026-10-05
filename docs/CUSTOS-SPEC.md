# CUSTOS-SPEC — the security envelope: vault, surrogation, policy, audit

Status: **DRAFT v4 — round 3 fully folded (pass A 7B/4M, pass B
3B/9M+; union ~11 distinct, incl. the conflict-parse deadlock both
passes caught). Suite V1–V26. Awaiting round 4.**
Proposes `custosd`, the credential daemon that sits between the agent
and every secret: an encrypted vault, surrogate tokens instead of real
credentials, a per-request policy engine sharing the existing approval
machinery, and a hash-chained audit log. Nothing here is built yet;
every behavior statement is a requirement, not an observation.
Companion specs: KEYS-SPEC (the interim credential path; amendments
CA-1; this spec delivers the takeover KEYS-SPEC §0 promised — no
command or endpoint changes for users), CELL-SPEC §4 (the dumb egress
proxy; CA-2 formalizes custos taking its policy layer and amending the
floor's range list), SURFACE-SPEC (approval hub; CA-3 adds the `custos`
source and one bare reason token), PENATUS-SPEC (the session log records
*that* a credential was used, never the credential). ARCHITECTURE §2.4
is the doctrine this implements; where they differ, this spec refines.

## Amendments to frozen companions

**CA-1 → KEYS-SPEC (frozen) §K2/§K3/§K4.** Amendments (a)–(e); nothing
else touched:
(a) **Vault occupies K2's layer 2.** With custody configured, the vault
*replaces* `keys.json` as the stored-credential layer: resolution
remains (1) `api_key_env`, (2) vault-then-keys.json (a name in both is
impossible post-migrate; if one appears, vault wins and logs a new
custos-owned startup line `custos: vault entry wins over keys.json for
"<name>"` — frozen here, because K2's shadow warning is
env-specific and must not be repurposed), (3) config. Env still
outranks the vault; when env shadows a vault-resident name, K2's
frozen shadow warning fires with the vault copy as the shadowed one —
K2's mechanics are inherited, not inverted.
(b) **Status enum gains** `set (custos)`. The other four status strings,
all CLI commands, all endpoints, and all failure strings are unchanged
frozen text — except as amended here. `sha256_8` for a `set (custos)`
name comes from `fingerprints.json` (§4.6), so locked instances keep
reporting it.
(c) **Name set for `GET /v1/keys` and `PUT` validation becomes**
`config ∪ keys.json ∪ fingerprints-name-mirror`, the mirror standing in
for the vault (§4.6: same critical section as every vault mutation,
reconciled at first unlock and after restore) — the same no-ghosts
rule, one more source. A migrated name therefore never becomes a ghost.
(d) **Locked custody responses (new frozen pair):** `GET /v1/keys` on
a locked instance returns entries for mirror names with
`sha256_8: "<stored hex>"` when `fingerprints.json` carries the name,
`null` only for names the mirror lacks (locked is visible, never a
500); `PUT`/`DELETE` touching a custody-managed name while locked
returns 423 with `custos locked — run: custos unlock` (frozen).
**Custody-managed = vault entry ∪ fingerprint-mirror name** (a name
that exists only in `config`/`keys.json` layers keeps frozen K4
behavior, 423 never applies to it).
(e) **Name gate and cap are inherited verbatim from K4**
(`^[a-z0-9][a-z0-9_-]{0,63}$`, 64 names counting vault ∪ keys.json —
the `key store full` 409/CLI error counts both stores). The vault
imposes *no* additional name grammar, so every legal `keys.json` name
migrates 1:1. With custody configured but a name absent from both
stores' layers, K2's remaining layers apply unchanged (no big-bang
migration).

**CA-2 → CELL-SPEC (frozen) §4.** The dumb proxy's contract is
inherited, not replaced: limits, destination floor, per-cell listeners,
DNS doctrine, and access log keep their sections verbatim, plus:
(i) surrogate inspection in the plaintext-HTTP request path (§5.2);
(ii) policy consult per new flow (§6.3/§7) — **the destination floor is
not a policy**: no `auto` rule, present or future, may relay to a
floor-blocked destination; the floor runs before any policy lookup and
its deny is unconditional; consults bind to the **forward-time dial
target** (§7);
(iii) **floor range list gains** CGNAT `100.64.0.0/10` and the
documentation ranges (v6 `fc00::/7` already covered; add `0.0.0.0/8`
"this host" block). Never a legitimate direct egress for a homelab
agent; every cloud metadata endpoint we know lives inside the floor's
covered blocks after this addition, plus **v6 link-local `fe80::/10`**;
(iv) **single door:** when custody is configured, hearthd binds *no*
proxy listener at all (loud line naming custosd as the owner; refuses
to double-bind). Custosd binds each per-cell listener *dynamically*: a
cell gateway address (`10.91.<n>.1:<proxy_port>`) exists only after
hearthd creates the cell's veth, so hearthd sends custosd a
`bind_listener`/`close_listener` event over `ctl.sock` on
cell start/stop, and custosd does the bind (an event for an address it
cannot bind is retried briefly, then audited `listener_bind_failed` —
the cell starts without egress, which is fail-closed, not a hole).
There is never a cell-reachable proxy that does not do the swap;
(v) **family honesty:** the floor is evaluated against the **resolved
socket address in every family**, with IPv4-in-IPv6 mapped addresses
un-mapped before comparison (`[::ffff:169.254.169.254]` **is**
`169.254.169.254` and is floored); §6.3 IP-form rules match the same
normalized socket address textually-and-numerically. A floor over
textual IPv4 forms alone is not the floor. The proxy's ingress is
DNAT'd 80/443 plus CONNECT only: **the proxy sees ports {80, 443},
period**; port-qualified rules and bindings exist for the cooperative
worker-dispatch path and the phase-2 taint plan, and a port-less rule
matches every observed port. At custosd **start**, custosd pulls the
live-cell listener set from hearthd (`list_listeners` over the control
connection) and binds each — `bind_listener` events are then edge
increments; this pull is what keeps a custosd respawn from leaving
healthy cells egress-dark (V19). Two retries, named separately:
hearthd retries *delivery* of an un-acked `bind_listener` for up to
30 s; custosd retries the *bind* of a received address briefly before
auditing `listener_bind_failed`.

**CA-3 → SURFACE-SPEC (frozen) §6.** Approval `source` enum gains
`custos`; `reason` enum gains the bare tokens `custos_locked`,
`credential_revoked` and `flow_gone` (no prefix — matching the
existing bare-token style): the first is used when the vault locks
mid-flight (C3) and settles pending cards, the second when a revoke
cancels a pending card
(hub gains a `CancelByCredential(cred, cell)` hook — cancel, not
settle: no user verdict is implied). Cell-visible denial strings
become a frozen pair: `approval denied by user` (human deny/timeout)
and `credential custody locked` (reason `custos_locked`); no path may
report a lock as a user denial. Hub semantics (state machine, timeouts,
both-doors rule, MarkUndeliverable) are referenced, not restated.

**CA-4 → PENATUS-SPEC (frozen) §2.** No schema change; this spec
asserts the rule in the credential direction: a worker-lane tool_result
persisted to a session carries the typed result (closed schemas, §5.1)
and a scrubbed error code/detail (§5.1a) — never tokens, refresh
material, upstream bodies, or URLs containing credentials.

## 0. The problem

Today a provider key reaches the model loop as a resolved string and
rides along on every request the loop makes. Any prompt-injected tool
output that can coax the agent into one crafted URL fetch — or any
connector with broader read scope than its task — has the credential
available, because the process doing the risky thing is also the process
holding the secret. "Be careful" is not a boundary.

Custos moves the secret out of reach: the agent loop and every cell
process hold **surrogates** — opaque handles bound to one credential and
one destination — and `custosd`, running outside the cell, performs the
swap at the network boundary after a policy check, or executes the whole
connector call itself. The milestone this spec exists to make true:

> **An agent with Gmail access physically cannot leak the Gmail token,
> because no byte of it ever enters the loop, the cell, the session log,
> or the audit log.**

**Honest delta, stated up front (same rule as KEYS-SPEC §0).** Custos
protects against the agent (and anything steering it), against files at
rest, and against casual exfiltration. It does **not** protect against:
a host-level attacker (root reads the unlocked process's memory — use
disk encryption; KMS is a named extension point); a malicious *built-in
worker* (trusted-by-us code outside the cell, covered by §5.1's rules,
not the cage); the user typing a secret to a model in chat; and —
stated exactly — an **upstream server that echoes the request's auth
header back in a bearer-lane response**: §5.2's response scrub
replaces the contiguous secret bytes with `[redacted]` before the body
reaches the cell, which defeats the naive echo but not an encoder of
it. Worker-lane calls have no such hole (the response never leaves the
worker as raw bytes — only typed results do). TLS sites are outside
the bearer lane entirely (§1 OUT), which means HTTPS APIs get custody
via workers, not swaps.

## 1. Scope

IN (v1): `custosd` as a separate process (hearthd-spawned or
standalone); age-encrypted vault with master passphrase or keyfile;
credential kinds `api_key` and `oauth2` (fields named, §4.5);
**worker lane** (typed built-in connectors executed inside `custosd`,
v1 built-in: Gmail — the primary lane for everything sensitive) and
**bearer lane** (surrogate header swap at the custody egress proxy for
plaintext-HTTP BYO endpoints); policy auto/ask/deny per credential
action and per egress destination, enforced through the existing
ApprovalHub so cards appear on web + Telegram like tool approvals;
hash-chained append-only audit with WAL, prune anchors, and a verify
command; KEYS-SPEC takeover per CA-1 (same CLI, same endpoints, one new
status string).

OUT (roadmap, listed so nobody "forgets to forget"): TLS-intercepting
MITM lane (user-CA enrollment; renegotiated from the old BUILD-PLAN
Phase-4 sketch — PO Q3); per-process egress taint (phase-2); eBPF
attribution; KMS-backed master keys; multi-tenant custody;
WhatsApp/Drive/Calendar built-ins (v1 ships exactly one — Gmail — so
the worker contract is proven by a real OAuth giant, not a toy); wallet
hooks (ARCHITECTURE §2.7 note stands).

## 2. Principles (load-bearing)

P1. **The swap point is the boundary; everything else is plumbing.**
No component other than `custosd` may hold, cache, log, or forward a
real credential value — including hearthd, nuntius, penatus, the web
page, and the audit log. V3 greps every artifact a turn produces for a
canary secret; one hit fails the build. "Swap point" is singular by CA-
2(iv): exactly one cell-reachable egress path exists, and it swaps.

P2. **Surrogates are capabilities, not identifiers.** A surrogate grants
exactly one thing: "a request from this host's cells, to this bound
destination, may carry this credential." Useless off the host, useless
cross-destination (binding checked per request, forward-time match is
authoritative, §7), revoked per S7's decision-point semantics. Surrogate
expiry by time is a named extension point, not v1.

P3. **Policy is evaluated per request, not per session.** An approval
that allowed one Gmail send authorizes *that action shape*, not a
session of Gmail. `ask` means every matching request needs a fresh
decision unless it matches a §6.5 narrow always-rule.

P4. **Fail closed, loudly.** Custos down, vault locked, policy store
corrupt, unknown lane value, or worker crashed ⇒ the request is refused
with a typed error; nothing falls back to "send without the credential"
or "skip the check." Locked means *nothing loaded*: lock and crash
zeroize every loaded secret, table, and token (§3 C3).

P5. **The audit log is the whole story about credentials.** Every swap,
worker call, policy decision, unlock, and vault mutation is one
hash-chained record — credential *names* and prefixes only, never
values — written write-ahead (§8.1a) so committed mutations cannot
outrun their record. If it isn't in the log, it didn't touch a
credential.

P6. **The user interface does not change.** `hearthd keys set/list/rm`
and the `/v1/keys` endpoints keep their verbs, shapes, and frozen
strings; custos changes what happens behind them (CA-1). A user who
never configures custos keeps today's behavior byte-for-byte.

## 3. Process model

**C1 — Separate process, single door.** `custosd` is its own binary.
When `hearthd serve` starts with a `custos:` config block (or a vault
file present), hearthd spawns it; thereafter the **dynamic listener
protocol of CA-2(iv)** (`bind_listener`/`close_listener` over the
control connection) is the only way egress listeners exist — hearthd
binds none itself. Web surface talks to it over the **control socket**
`<hearth>/custos/ctl.sock` (0600, token file `<hearth>/custos/ctl.token`)
and proxies `/v1/keys` (+ roadmap `/v1/vault`) through it. Standalone
mode (`custosd serve --config …`) binds the same sockets itself; there
it binds **only** the control socket and per-cell egress listeners — no
administrative TCP port, ever. Workers live inside the process (v1
tradeoff, §1).

**C2 — State root.** `<hearth>/custos/` (0700): `vault.age`,
`vault.age.mac`, `vault.key`, `surrogates.age` (encrypted, §4.3),
`fingerprints.json`, `policy.json`, `audit/custos-YYYYMMDD.jsonl`,
`audit/anchors.json`, `snapshots/`, `ctl.sock`, `ctl.token`,
`custos.lock` (flock file serializing daemon and file-direct CLI,
mirroring KEYS-SPEC `keys.lock`). Per-instance anchoring exactly like
K1; no env-var global home.

**C3 — Unlock model.** Unlock paths: (a) `custos unlock` — CLI connects
to the control socket and prompts locally on the CLI's TTY (the daemon
child has no TTY; unlock always arrives through ctl.sock with the token
file), (b) keyfile mode (`custos.unlock_keyfile: <path>`, read once at
spawn and zeroed; the keyfile *is* the boundary in that mode and the
config comment says so). **Keyfile mode is unlocked-from-boot:** the
spawn-time read IS the unlock; the derived identity lives in memory for
the process lifetime (that is the documented trade of the mode), so §4.5
refreshes and file-direct CLI mutations always have it; `custos lock` in
keyfile mode refuses (`keyfile mode: always unlocked — remove config to
change`). **What locked mode may hold** (the complete list; nothing else
touches process memory): `fingerprints.json`, `vault.key`, and HKDF
outputs derived from `vault.key` (envelope + anchor keys — restore needs
them while locked, §8.4). **Locked is a loaded-state statement:** while
locked, no credential value, surrogate table row, OAuth token, or
fingerprints-adjacent key material exists in process memory — the
surrogate registry is *encrypted* and parsed only at unlock, zeroized
at lock. A locked daemon cannot swap because it holds nothing to swap
with. `custos lock` (or SIGTERM) zeroes loaded state and settles every
pending credential-bearing card with reason `custos_locked` (CA-3). No
auto-unlock-at-boot ever. **Recovery runs at first unlock, not boot:**
the daemon cannot read age-encrypted state while locked; startup appends
`custos_restarted_after_crash` whenever the WAL file exists and is
non-empty (file-level dirtiness is readable locked — the audit chain
append needs no vault), and serves locked. At first unlock: registry/orphan reconciliation,
fingerprint-mirror recomputation, and WAL recovery (§8.1a) all run
before serving credential traffic. `custos audit verify` is the sole
locked-capable audit action; an unrecovered WAL is a reportable state,
not a crash.

**C4 — Crash doctrine.** A `custosd` crash is a locked vault plus zero
memory: requests in the crash window are refused (P4); restart re-locks
(keyfile mode: re-opens); the audit log appends
`custos_restarted_after_crash` iff the previous run left no clean
`custos_stopped` tail. Because the proxy is *inside* custosd
(CA-2(iv)), a crash takes the swap door with it — there is no stale
half-loaded proxy to fail open; cells see connection refused until
restart, which is the correct posture. Pending approvals settle via
the hub's MarkUndeliverable/timeout paths.

## 4. The vault

**4.1 — Format.** `vault.age` is an age file (`filippo.io/age`,
borrowed-and-pinned) around JSON:
`{ "version": 1, "generation": <N>, "credentials": { <name>: Credential } }`
where Credential is `{ "kind": "api_key", "secret": … }` or kind
`oauth2` (fields §4.5). Scrypt parameters are fixed versioned
constants (bumped via `version`), so nothing decryptable-or-integrity-
relevant sits outside. At init, custosd generates `vault.key`: 32
random bytes, 0600, in the state root — the **instance key**. Every
write of `vault.age` produces `vault.age.mac` = HMAC-SHA256 over the
envelope bytes under HKDF(vault.key, "custos-env"); every unlock
verifies it before decrypting: mismatch ⇒ `vault envelope corrupt`
(frozen) — this is what makes whole-file substitution (attacker
re-encrypts their own JSON under a passphrase they cannot read back)
fail, which age-under-a-shared-passphrase alone does not. Snapshots and
restore carry `vault.age` + `.mac` + `surrogates.age` as a set.
**Secret values exist in plaintext nowhere** (and names appear in
plaintext only in `fingerprints.json`, the same exposure class
KEYS-SPEC already ships on the wire).
Keyfile mode uses the same structure. `filippo.io/age` enters go.mod
as the first runtime dependency outside stdlib, pinned
(`filippo.io/age v1.3.2` in go.mod + go.sum at freeze time — the go.mod
diff is the pin; no floating versions).

**4.2 — Writes, generation, and the two-file protocol.** `generation`
is **persisted inside the vault document** (age-authenticated: an
attacker cannot roll it back without breaking the envelope); it starts
from the document on unlock, so restarts cannot collide snapshot names.
Mutation protocol for the vault: acquire `custos.lock` flock
(**acquisition timeout `custos.lock_wait_timeout` default 10s**, typed
`lock_timeout` error on expiry — no indefinite hangs) → decrypt →
modify → generation+1 → WAL intent (§8.1a, includes the new generation,
a **random nonce**, and an ordered list of the names it touches) →
encrypt → temp in `<hearth>/custos/` (0600) → **fsync(temp)** → rename
→ **fsync(dir)** → WAL commit (**flushed, not buffered**) — the
durability chain inherits KEYS-SPEC's atomic temp+rename doctrine and
makes every arrow power-loss honest. **The in-memory loaded-state swap
happens inside the flock, before release** (K6 doctrine inherited):
after the commit line lands, no reader can observe tables that
disagree with the committed files, and a revoke's memory removal is
visible to every later swap-decision. The
mutation critical section is
in-process only and never spans a network forward or an approval wait —
revocation semantics are S7's decision-point rule, not a global lock.
Mutations that touch the registry (credential removal, surrogate
revocation) follow **vault-first commit order**: vault commits, then
registry temp+rename; if the process dies between, first-unlock
reconciliation (§C3) drops registry entries whose credential no longer
exists and appends `registry_reconciled` per drop.
One-way safe: orphan bindings die, credentials never do.
**WAL recovery is ordered by identity, never file dates:** an intent
nonce matching a commit nonce closes the pair. A dangling intent is
*never* auto-applied — `custos restore` is the remedy path (§8.4).
An intent is closed `vault_mutation_superseded` in exactly two ways:
the vault's persisted generation is greater than the intent's (a
later checkpoint rewrote the journal forward), or **`custos restore`
closes every currently-dangling intent at swap time** (restore runs
offline under the flock and is itself a checkpoint operation: no
post-restore boot can ever apply the superseded mutation, and the
restore audit line is the proof).
Recovery's own vault rewrite runs its *own* intent/commit pair marked
`recovery`, so a crash during recovery survives a further boot.

**4.3 — Surrogate registry.** `surrogates.age` (same age identity,
separate envelope so rotation doesn't rewrite bindings) maps
`sur_<22 base62>` → `{ credential, lane: "bearer", host,
ports: [80] default, path_prefix: "/" default, added_at }`. `host` is
validated at issuance by §6.3's host grammar (lowercase, no userinfo,
no `*`, no `/`, no bare IPs unless literal-IP form — IPv4/IPv6 accepted
as bracketed/exact forms; an **IP-form binding is ask-always**: the
§6.1/§6.5 write path refuses to store an `auto` rule whose pattern is
an IP-literal (`ip bindings are ask-only`), no Always, because an IP gives
the card's human no
registrable domain to judge); `crypto/rand` values; 0600 enforced at
startup like tokens files. Unknown `lane` values on parse: entry
**refused at load** with audit `surrogate_rejected`, never defaulted to
bearer. The registry file leaks nothing without unlock (encrypted) and
holds nothing while locked (C3).

**4.4 — Takeover (CA-1 in action).** `custos migrate` (new verb in
custos' own namespace — KEYS-SPEC's frozen command list gains nothing;
§0's "same verbs" promise holds because the day-to-day `keys` family is
untouched): with custos configured and unlocked, for each `keys.json`
entry apply the K2 layer-2 resolution and move the resolved value into
the vault under the same name (kind `api_key` — name grammar inherited,
so no entry can be "unmigratable"; cap overflow uses the frozen
`key store full`), then delete the `keys.json` entry. Frozen output:
one line per move `migrated <name>`; `nothing to migrate` (exit 0) when
none; any failure mid-list ⇒ already-moved entries stay moved (re-running
finishes the job; vault-first + delete-after is safe to replay) and the
command exits non-zero with `migrate incomplete: <moved> moved,
<skipped> skipped` (frozen). Values never print.
`custos export <name>` prints the value once to a TTY after
confirmation, refuses when piped (`refuse: pipe — export to a
terminal`), and deletes the vault entry — the escape hatch back to
plaintext. `keys rm` under custody removes vault entry + its surrogates
under §4.2's two-file protocol, audits both kinds, one generation.

**4.5 — OAuth2 credentials (v1: Google/Gmail).** Kind `oauth2` fields,
all inside the vault envelope: `client_id`, `client_secret`,
`refresh_token`, `access_token`, `access_expiry`, `scopes`,
`token_uri`, `auth_uri`, `granted_at`. Entry path:
`custos login gmail --client-id <id>` (client secret prompted on the
TTY, never a flag/argv) — prints the consent URL **with a random `state`
parameter** (generated, stored in memory, validated on callback;
mismatch ⇒ refused, audited), binds an ephemeral `127.0.0.1:0`
listener for the redirect, exchanges the code, stores kind `oauth2`.
**The listener is one-shot** (first callback consumes it, then closes;
subsequent requests get connection-reset), it validates `state` (§0's
CSRF param), and the op is gated by `ctl.token` like every ctl.sock op.
Headless hosts (container/SSH — browser can't reach that listener):
`custos login gmail --manual` prints the URL, the operator completes
consent anywhere, and pastes the redirect URL (or bare code) back into
the TTY; `state` is still validated against the printed value.
Refresh happens inside the worker, under a **per-credential refresh
lock**: the token-endpoint forward is held **outside** the `custos.lock`
mutation flock (read state → forward → brief flock for the encrypt +
two-file write, per §4.2); concurrent refreshes of the same credential
coalesce onto the in-flight one. **Write-back revalidates existence:**
the flock-acquired step first checks the credential still exists in the
vault; if a `custos revoke` committed during the forward, the refresh
result is **dropped** (audited `credential_rotated` with
`dropped: revoked`) — a refresh in flight never resurrects a revoked
credential. A refresh that rotates only
`access_token` is an **ephemeral** vault write (no snapshot, §8.4) and
audits `credential_rotated` with `ephemeral: true`; rotation of
`refresh_token` or `client_secret` is material (snapshots). Refresh
tokens are thus snapshotted and audited like everything else.
`access_token` is persisted (encrypted at rest) but never serialized
into any worker response or cell-visible byte.

**4.6 — Fingerprints sidecar.** `fingerprints.json`: `{name: sha256_8, mac: "<anchor/instance MACs §8.3>"}`
— names + 8-hex hashes only; updated atomically inside every vault
mutation (same critical section; a crash between vault rename and
fingerprint write is reconciled at the first unlock after boot —
recomputed from the decrypted vault, and `GET /v1/keys` on a locked
instance reports the stored value (null only if the mirror lacks the
name — the mirror is authoritative for locked reads, CA-1(b)/(d)). It doubles as the **name
mirror** for CA-1(c): name set and `sha256_8` while locked come from
here. Plaintext-safe (8-hex hash of the secret; same exposure class as
KEYS-SPEC already ships on the wire).

## 5. Two lanes to a credential

**5.1 — Worker lane (typed, primary, strongest).** Built-in connectors
run inside `custosd` (v1: Gmail over OAuth2 + REST; no shell, no plugin
code). Cells reach workers over a **per-cell unix socket bind-mounted
into the cell at `/run/lararium/custos.sock`** (created by `cell
create`, owned by the cell uid — the kernel isolates sockets between
cells; the guest sees one socket, never another cell's). NDJSON
protocol: `{ "op": "call", "connector": "gmail", "tool": "<name>",
"args": {…}, "cell_token": "<per-cell secret from cell.json>" }`.
`cell_token` binds the audit `actor` and the ask card's requesting-cell
field to a verified identity (policy scoping per cell is roadmap —
the secret enforces attribution, not permission). The agent's view is
an ordinary tool schema; no surrogate exists here and no raw bytes of
any upstream exchange are ever returned.

**5.1a — Worker error contract (closed).** Response errors are
`{ "error_code": <closed enum>, "detail": <scrubbed> }` with
`error_code ∈ denied | locked | unknown_connector | unknown_tool |
bad_args | upstream | rate_limited | timeout | transport | internal`
(frozen). `detail` is built from a closed template per code — HTTP
upstream failures contribute a status class ("upstream returned 5xx")
only; transport failures (DNS, refused, no status in `*url.Error`) map
to `transport` ("connection failed before response"); deadline-exceeded
maps to `timeout`. **No URL, no upstream body, no wrapped `error`
chain** (the worker unwraps `*url.Error` and keeps only its class). Before send, `detail`
is scrubbed against **every secret string in C3's loaded set — each
field individually** (every `api_key` value, and for `oauth2`:
`client_secret`, `refresh_token` *and* `access_token` separately, plus
every loaded surrogate): any equal substring is replaced with
`[redacted]`. **Scope split (frozen):** the whole-set rule binds
worker error `detail` (short strings; O(set) is trivial). The §5.2
bearer **response** scrub scans only **the swapped secret** of that
request — the enforceable O(1) scope; echoes of *other* credentials
in a response body are out of scope there (each request only ever
carries its own credential to that upstream). Substrings shorter than
8 characters are never replaced in bodies (KEYS-SPEC scrub floor,
inherited; a <8-char secret echo is a documented limit). The tool_result the model and penatus see
carries exactly `error_code` + `detail`.

**5.2 — Bearer lane (header swap for plaintext-HTTP BYO endpoints).**
The custody egress proxy inspects plaintext request headers
(`Authorization`, `X-Api-Key`, `X-Goog-Api-Key`, plus configured extras)
for a registered surrogate. Pipeline per request: normalize authority
(lowercase host, strip default port, strip trailing FQDN dot) and path
(`path.Clean`; encoded dot-slices decoded before match) → look up
surrogate (must exist, `lane == "bearer"`, host **and ports** binding
must match the forward-time dial target) → policy (§6) → swap header,
**strip `Accept-Encoding`** (upstream is asked for identity encoding;
compression would put secret echoes beyond the scan), forward. Any step
failing: `403 surrogate` to the cell (frozen reason string), audit
`swap_denied`, nothing sent. The **forward-time match is the law** (the
binding is checked against the destination the socket actually dials,
§7). Query-string credential swaps do not exist in v1 (URLs leak through logs and error text — same lesson the surface
learned with token-bearing URLs). TLS (`CONNECT`) is blind passthrough:
no swap, documented, asserted by V10 so the honesty claim can't rot.
**Response scrub (bearer lane):** responses carrying **any**
`Content-Encoding` are refused as binary-typed (a compressed body is
unscannable, whatever the strip asked for). Text bodies up to 1 MiB are
scanned and every occurrence of the swapped secret replaced with
`[redacted]` before delivery to the cell — defeats header-echoing error
pages; encoding-variant echoes are the documented limit (§0). Bodies
over 1 MiB, and binary content types, are **not delivered partially**: the connection closes with audit `swap_denied`
reason `response_size` / `response_type` (no framing corruption — the
cell sees a closed connection, not truncated JSON) unless the binding
sets `allow_binary: true` (binary then bypasses scrub, binding-owner's
eyes-open choice).

**5.3 — Issuance.** `custos surrogate add <credential> --host
api.example.com [--path /v1/] [--port 8080]`: TTY prints the surrogate
once. Surface `PUT /v1/keys/{name}/surrogates`: because the requesting
surface cannot be distinguished from a prompt-injection-driven mint,
**every surface mint requires a confirmation card** (source `custos`,
showing credential name + exact bound destination + path/port) answered
like an approval before the surrogate is created; TTY issuance skips it
(you are at the keyboard). CLI `custos surrogate list|revoke`.
Revoking a credential atomically revokes its surrogates (§4.2 protocol,
one generation, both audit kinds).

## 6. Policy (the Vigil defaults)

**6.1 — Model.** Two rule tables — credential actions (§6.2) and egress
destinations (§6.3) — each `{ pattern → verdict }` with **verdict ∈
`auto | ask | deny`, identical in meaning on both lanes: approve-without-
card / require-a-card / refuse** (frozen; `auto` on a credential
pattern *is* the card-suppressing verdict — §6.5's Always writes
exactly this). **One normative comparator: the longest normalized
pattern string wins** (§6.3), always — more-specific overrides
general by construction, which is what makes `gmail/send auto` beside
`gmail/* ask` legal and deterministic (send auto, everything else
asks). Overlap with differing verdicts is therefore **not an error**;
the writer warns when a new pattern is strictly dominated by an
existing one whose verdict differs and could not take effect
(`note: overridden by <p2> (longer match wins)` — stored anyway,
`policy add` exit 0). No global conflict rejection: subsumption over
unbounded continuations is not computable as a write-time check, and
refusing overlaps deadlocks the Always mechanism. Defaults:
credential-bearing actions
**ask** (no rule required);
credential-less egress **auto** (today's dumb-proxy behavior, PO Q4);
the CA-2 destination floor is evaluated before policy and is not a
rule.
`policy.json` strict-parses at startup; empty-but-valid tables mean
"defaults", never "allow everything" (defaults are ask for credentials
by construction).

**6.2 — Credential action patterns.** Canonical form
`<name>` or `<name>/<tool>`; the `/tool` suffix **matches worker-lane
calls only** — a bearer-lane swap has no tool and is matched by the
bare-`<name>` pattern alone. Examples: `gmail/send auto`, `gmail/* ask`
(worker tools under gmail), `openai ask`. **Worker dispatch is the
consult point:** custosd checks §6.2 before executing a worker tool
call — no host is involved there; connectors additionally declare a
fixed API host set (gmail → `*.googleapis.com`) validated against §6.3
at install, printed on the card, and the worker forward refuses any
destination outside it (error `transport`). Evaluation order per
request:
exact request pattern → table longest match → default. An always-rule
(§6.5) writes the exact canonical form of *this request* (`openai` for
bearer, `gmail/send` for worker), which by construction outranks every
table pattern — no longest-match trap.

**6.3 — Egress host grammar (canonical, refusal-modeled).** Accepted
pattern forms: **exact host** `api.example.com`, **host:port**
`api.example.com:8080`, or **domain-with-subdomains**
`example.com/*` — the suffix form is written with a trailing `/*`, not
a leading dot, and matches the apex **and every proper subdomain**,
never `evilexample.com`. Canonicalization at both write time (rules,
bindings) and match time (requests): lowercase host (**IDNA2008
ToASCII with validation: disallowed/status-ERROR labels rejected at
write, `invalid pattern`; a rule whose unicode form and stored punycode
disagree is refused**, default port stripped, trailing FQDN dot stripped (a request to
`denyhost.com.` matches `denyhost.com` — no dot-bypass), userinfo and
path rejected outright, `*` accepted only as the exact trailing `/*`
suffix form, IPv4/IPv6 as exact addresses only. IP-form rule hosts are
canonically written (non-canonical forms like `203.0.113.007` or `::ffff:0:…`
rejected at write, `invalid pattern`). **Target side:** a
request authority whose normalization fails at all (IDNA error, control
characters) is refused outright (`403 surrogate` when a surrogate rode
on it, connection closed otherwise — never guessed at). An IP-literal
target **matches no host rule** — host rules are hosts; it matches
**IP-form rules** exactly (CA-2(v): the same normalized socket address
the floor uses), so `deny 203.0.113.7` stops a swap to that IP (V23).
Credential-less egress to an IP-literal with no IP-form rule follows
§6.1's default — and §0 says
plainly that hostname policy therefore cannot constrain IP-literal
dialing (the floor covers the dangerous ranges; full containment is
phase-2 taint).
**Pattern side:** the `/*` suffix form requires at least two labels —
`com/*` and `*` are refused at write (`invalid pattern`); one bare `*`
would be a wildcard and wildcards do not exist.
**Punycode rule:** an
`xn--` label compares equal to a rule only when the rule itself stores
that exact punycode — rules are normalized and, if a rule's host is a
non-ASCII form, it is stored punycode-canonical; a unicode-homograph
request (`аpple.com` → `xn--pple-43d.com`) therefore matches only an
explicit `xn--pple-43d.com` rule, never `apple.com`, and §6.5 always-
rules can only write what §6.3 accepts. Invalid patterns are refused at
write time with the frozen message `invalid pattern`. Match function
(single ordering law, used by both tables and referenced by §6.1):
collect all matching rules (exact host, exact host:port, apex/subdomain
`/*`); the winner is the **longest normalized pattern string**, ties
impossible because canonical forms are distinct strings; among
non-overlapping canonical strings the ordering is total. The
match runs **forward-time** (§7).

**6.4 — Ask = the hub, not a fork.** `ask` routes through the same
ApprovalHub the tool approvals and Telegram cards use (`surface` +
`nuntius` fan-out). Card content (frozen field set): credential or
connector name, tool (worker lane), destination host[:port] + path
prefix (bearer/egress), argument digest `sha256:<64 hex>` — plus any
review fields the connector declares on its tool schema (gmail/send
declares `to` and `subject` so a human can actually judge; declared
review fields are user-facing UI, never secrets). Requesting cell id.
Timeout/both-doors rules inherit SURFACE-SPEC §6. Decisions are
per-request (P3). Deny/timeout propagation: for worker calls the cell
receives `{ "error_code": "denied", "detail": "approval denied by user"
}`; for parked egress flows the proxy answers `403 surrogate` with body
`approval denied by user` (the **single** typed refusal status for all
lane denials; the parked flow itself is held with **no HTTP status** —
closing mid-park would look like a completed response to real clients)
and lock-settled flows answer `403 surrogate` body `credential custody
locked` (CA-3's frozen pair). A denial is a normal, explainable
outcome, matching the loop contract. **Revocation while parked:** an
`ask` decision has *not* happened while a flow or worker call is parked
— parking only enqueues the decision. `keys rm`/`custos revoke`
settles every pending card for that credential as a deny (reason
`credential_revoked`) and closes parked flows; the hub exposes a
`CancelByCredential(cred, cell)` hook (CA-3 amendment — cancel is not
settle: no user verdict is implied). A user clicking a race-lost card
gets the frozen `already answered` hub behavior, same as tool approvals.

**6.5 — Always rules.** Frozen button set: **Allow once / Always /
Deny**. Always writes the canonical exact-match form of *this request*
(§6.2's canonical form, or §6.3's exact host[:port] — never the
`/*` suffix form, never a floor address — both re-validated against
§6.3/§6.2 grammar after construction; construction reads the verified
request fields, never payload bytes). An Always write **always stores**
— the exact form outranks every overlapping pattern by the §6.1
comparator, so no conflict path exists for it. `custos policy add
<pattern> <verdict>` is the manual writer (same grammar validation,
same §6.1 domination warning, audit `policy_written`);
`custos policy list` shows every
rule with an `always` marker and source event; `custos policy rm
<pattern>` deletes one; `custos policy reset` wipes all `always`
entries (frozen confirmation `always-rules cleared`), keeping
config-declared rules. Table writes: same flock + temp/rename, audit
`always_rule_added`/`policy_written`.

**6.6 — Corruption posture.** Strict-parse failure of `policy.json` ⇒
custosd runs **locked** (P4/P5) with the loud remedy line `custos
policy reset` (policy has no snapshots — §8.4); failure of
`surrogates.age` or the vault ⇒ locked with the loud remedy line
`custos restore <gen>`; it never boots a guessed-empty policy (an empty *tables* file that validates is "use
defaults" — which is still deny-biased for credentials by §6.1's
construction; a file that fails validation is corruption). Unknown
`lane`, unparseable binding host, or credential pointing at a missing
vault name ⇒ that entry refused at load, one audit `surrogate_rejected`
per entry, daemon still serves (degraded, counted in
`custos status output` as `surrogates_dropped: <n>`).

## 7. Egress proxy custody (CA-2)

The Phase-3 proxy keeps every contract (limits, floor, access log,
per-cell listeners) and gains: (a) the §5.2 surrogate pipeline; (b) a
policy consult per **dialed destination**, forward-time: the proxy
resolves, checks the floor (CA-2 amended list), then matches §6.3
against the *actual dial target* — approve-time metadata is advisory,
forward-time match is the law (rebinding doctrine, V16). `ask` flows:
the proxy reads headers, parks the flow in ask state (socket held open,
**no read deadline consumed** — parked flows are tracked in daemon
memory, bounded by `custos.ask_hold_timeout` default 330s = hub's
5-minute approval timeout + 30s margin), then either dials or answers
per §6.4 (parking is **not** a decision; Allow re-validates vault,
registry, **and the current policy verdict** at decision time — a
`policy rm`/revoke landing while parked kills the flow at the dial). If the daemon dies while parked the flow
dies with it (P4). Parked flows are capped **per cell**
(`custos.max_parked_per_cell`, default 16; **global** cap default 128
as resource ceiling): the global ceiling sheds from the cell holding
the most parked flows first, so one cell cannot starve another's.
While parked the proxy reads **headers only** — body bytes are never
consumed before a decision (backpressure sits with the sender).
Parking gate: only HTTP/1.1 requests with a valid absolute-form URI
(or `Host`) may park; anything else is closed immediately (a 1.0
client must not hold a 330 s slot). **Denial delivery rule (frozen
wire mechanics):** before writing any parked-flow refusal the proxy
must **drain the socket's unread receive buffer (bounded read: to EOF
or 64 KiB, 250 ms cap)** and only then write the response and close —
a bare `close()` with unread body bytes makes the kernel emit TCP RST
and the cell would see `ECONNRESET` instead of the 403. After a
parked-flow response the connection is closed — no pipelined
continuation is parsed. If the
cell closes the connection mid-park, the flow is dropped, its slot
released, and its card **cancelled** (`credential_revoked`-style
cancel: no user verdict is implied, CA-3; reason `flow_gone` — added
to CA-3's reason enum). An Allow that arrives after the flow is gone
is recorded on the hub as a settle against a dead flow (audit kind
`stale_verdict`; nothing dials; the decision is recorded, not
re-judged). `ask_hold_timeout` expiry closes the flow and settles the
card with the **frozen `timeout` reason** (SURFACE-SPEC §6 — no new
token; the audit `reason` is `timeout`), and per-cell cap overflow
answers `403 surrogate` body `too_many_asks` — a typed refusal under
§6.4's single-status rule, not a 429 — without touching another
cell's parks (S6).

## 8. Audit log

**8.1 — Records.** One line, one event, hash-chained (previous-line
SHA-256; genesis line names vault generation) — surface audit's scheme
and crash-tail rule, separate file family. Kinds (frozen):
`custos_started`, `custos_stopped`, `unlocked`, `lock_failed`,
`vault_migrated_keys`, `credential_added`, `credential_rotated`,
`credential_removed`, `surrogate_created`, `surrogate_revoked`,
`surrogate_rejected`, `registry_reconciled`, `vault_mutation_intent`,
`vault_mutation`, `vault_mutation_recovered`,
`vault_mutation_aborted` (emitted when a §4.2 mutation is refused
after its intent line — `reason: lock_timeout` or a failed
encrypt/write — so a credential-touching refusal is never absent
from the chain), `vault_mutation_superseded`, `stale_verdict`,
`listener_bind_failed`,
`swap_allowed`, `swap_denied`, `egress_allowed`,
`egress_denied`, `worker_call_allowed`, `worker_call_denied`,
`policy_written`, `always_rule_added`, `policy_reset`,
`custos_restarted_after_crash`, `audit_pruned`. `credential_added` is
emitted by `custos login` success and by `hearthd keys set` under
custody (the custody-routed write audits it with `actor` surface/cli).
Fields: `t`, `kind`,
`cred` (name), `sur` (8-char prefix), `actor` (cell id | `cli` |
`surface`), `host`, `tool`, `verdict`, `reason`, `gen`, `prev_hash`.

**8.1a — Write-ahead rule.** Every vault/registry mutation appends
`vault_mutation_intent` (with nonce + target generation, §4.2)
**before** the rename and `vault_mutation` after; `custos audit verify`
treats a dangling intent as a reportable state (`uncommitted mutation
at <file:line>`) and **first unlock** runs §4.2's identity-ordered
recovery, appending `vault_mutation_recovered` /
`vault_mutation_aborted` / `vault_mutation_superseded` as the pairing
dictates. Recovery never guesses from mtimes. An intent is **resolved**
when its nonce has a paired `vault_mutation` / `recovered` / `aborted` /
`superseded` line — resolved intents are silent to `verify`, so
crash-recovered states converge to a **clean verify** (V7 asserts this
quiescence, not just detection).

**8.2 — Secrecy.** Audit lines pass the canary test (V3). Argument
digests are sha-256 (64-hex), never truncated plaintext. No record
contains a credential value, full surrogate, worker argument bytes, or
upstream response content.

**8.3 — Verify, rotation, prune anchors.** `custos audit verify` walks
the chain from the newest anchor; prints `chain ok (<n> records)` or
first-break `file:line` (frozen shapes). Daily rotation;
`audit_retention_days` (default 90, PO Q5). **Prune writes an
authenticated anchor checkpoint** `audit/anchors.json`:
`[{file, line, hash}]`, each entry `anchor_mac`-ed under
HKDF(vault.key, "custos-anchor") — a root-elsewhere attacker who edits
log lines cannot forge a new anchor without the state-root key, so
"anchor then ignore tampered history" fails verification (V7). Verify
validates the newest anchor's MAC **and** its continuity (the anchor's
predecessor line must be the prune point's recorded `audit_pruned`
kind) before trusting it, and always validates anchor-hash → chain-end.
Prune only ever moves anchors forward (V21). The genesis file is never
pruned while it is the only file; the newest anchor is never pruned.

**8.4 — Snapshots & restore.** Before every vault mutation: the
pre-image set (`vault.age`, `vault.age.mac`, `surrogates.age`) is copied
to `snapshots/gen-<G>/` (keep 8, G = persisted generation, no collision
across restarts; **ephemeral access-token refreshes write no snapshot**,
§4.5 — routine hourly churn can never evict the history that matters).
`policy.json` is **not** snapshotted (no secret material; §6.6's remedy
for policy corruption is `custos policy reset` + re-declare, not
restore — the snapshot set stays exactly the two-file credential set
plus MAC).
`custos snapshots list` prints generations. `custos restore <gen>
--yes` runs **offline under the `custos.lock` flock** — it verifies the
snapshot's `.mac` with `vault.key` (no passphrase needed; the daemon
being down or locked is exactly when restore exists), renames the set
into place atomically, and appends the `vault_mutation_intent`/
`vault_mutation` pair with reason `restore` to the audit chain — the
next unlock's first-unlock pass reconciles registry and mirror
afterwards. Restoring the pair means vault and registry land on the
same generation: no surrogate ever outlives its credential via restore.

## 9. Security invariants (each maps to a test)

S1. No artifact a turn produces (session log, blobs, audit, proxy
access log, card text, Telegram messages, worker error strings)
contains a vault secret. → V3
S2. A surrogate presented to the wrong host, wrong port, or wrong path
yields `403 surrogate` + audit; no rewrite, no packet. → V4
S3. Locked vault ⇒ every credential-bearing request refused; **zero
egress dials while locked** (dialed-destination counter on the proxy).
→ V5
S4. Always-answers write canonical exact-match rules only — payload
bytes, wildcard fragments, and injection-shaped hosts cannot widen a
rule; rule construction consumes only grammar-validated request
fields. → V6
S5. Chain tamper (mid-file byte flip) and truncation ⇒ `audit verify`
reports exact `file:line`; append-after-tamper still detects; prune
never breaks verify-from-anchor. → V7
S6. Two cells: cross-cell socket/secret use refused; ask cards name the
requesting cell; a cell cannot consume another cell's asks
(approval_id encodes the requesting actor). → V8
S7. **Decision-point revocation, decision = swap/dial commit.** The
decision instant is when the proxy commits the swap and dials (or the
worker commits the upstream call) — never earlier. A parked `ask` flow
has **no decision yet**; Allow re-validates vault + registry + current
policy verdict at that
moment (revoked ⇒ refuse per §6.4, card cancelled). After
`custos revoke <cred>` commits, no *later commit* can succeed: swap and
worker decisions re-read the in-memory state under the mutation lock.
A request whose decision landed before the revoke may finish
transmitting — the network cannot be unsent — and is never re-evaluated
afterward. Revocation latency is therefore bounded by the decision
point, and pending cards never outlive the credential. → V9
S8. CONNECT to a surrogate-bound host is blind passthrough: the echo
server past the proxy sees the surrogate byte-identical (nothing was
swapped), asserted so the honesty claim can't rot. → V10
S9. Corrupt `policy.json` or `surrogates.age` ⇒ locked + one remedy
line; web stays up; zero swaps; entry-level corrupt rows are dropped
with `surrogate_rejected`, not fatal. → V11
S10. KEYS-SPEC surface contract holds with custody on: endpoints, all
five status strings, CA-1's amended name set (migrated names never
ghost), frozen failure strings, idempotent DELETE, `sha256_8` from the
fingerprints sidecar while locked. → V12
S11. Single door: with custody configured, exactly one process holds
the cell egress listeners; hearthd's proxy refuses to bind; no
cell-reachable path reaches the network without the swap pipeline. → V19
S12. Bearer response scrub: an echo server reflecting the real
credential in a text body arrives at the cell as `[redacted]`;
binary-typed responses without `allow_binary` are refused. → V20

## 10. Verification suite (numbered; CI where fake-able, live otherwise)

V1. `custos init/unlock/lock` lifecycle (keyfile mode as TTY
substitute) — state file modes/paths asserted; second `init` refuses
(`vault exists`, frozen); double `unlock` is a no-op with audit `unlocked`
once.
V2. `custos migrate` moves K4-gated names 1:1 (fuzz: underscores, digit
starts, 64-char), frozen `migrated <name>` lines, `nothing to migrate`
exit 0, values never in stdout, `custos export` refuses when piped;
re-run after partial finishes the job.
V3. Canary sweep: seed vault with `CANARY-<rand>` secrets; drive a full
fake turn (worker call + bearer swap + denial + lock settle); grep
every S1 artifact for the canary — build-failing.
V4. Binding matrix through the real proxy against a loopback echo:
right host, wrong host, right host wrong port, `/v1/` prefix vs
`/v1/../admin` (normalized → mismatch), uppercase Host header,
`host:80` vs rule without port — all per §6.3 canonical rules.
V5. Locked-vault refusal with dial counting (locked: zero dials; swap
and worker lane both).
V6. Always-rule writing: payloads containing `*`, newlines, `.evil.com`
fragments; host fields containing `*`, `@`, `/`; written rules always
re-validate against §6.2/§6.3 grammar and the file re-reads strict.
V7. Audit chain: happy path, byte-flip, truncation, multi-day files,
prune-with-anchor then verify, WAL intent-without-commit detected;
verify output strings exact.
V8. Two-cell harness (two fake cells, two cell_tokens, two sockets):
cross-use refused, ask card actor fields correct, per-cell socket
ownership asserted at the filesystem.
V9. Revoke race, decision-point semantics: instrument the decision
point; harness interleaves revoke with swap attempts and asserts (a)
no decision reads the registry after revoke commit, (b) decisions
before commit proceed, (c) audit sequence is consistent — the test
fails if *any* post-revoke decision succeeds, which is the non-tautological
version of this assertion.
V10. CONNECT passthrough byte-preservation (echo past the proxy sees
the surrogate, no rewrite) — documents S8/§5.2's TLS honesty.
V11. Corruption postures: unparseable policy/registry (locked, one
line, web up); entry-level corrupt registry rows (dropped,
`surrogate_rejected`, serve continues); unknown lane value (entry
dropped, never bearer-defaulted).
V12. KEYS-SPEC §4 contract re-run against a custos-backed instance
(same table-driven test, both backends), plus CA-1(c): migrated names
present in `GET /v1/keys`, `PUT` on a migrated name routes to the vault
(not `Unknown provider`), CA-1(b): locked instance reports stored
`sha256_8`.
V13. Gmail worker against a local fake OAuth server: consent dance via
`custos login` (ephemeral listener asserted 127.0.0.1 + :0-based, dies
after), **`state` param present in URL and enforced on callback (forged
code with wrong/missing state refused + audited)**, `--manual` paste-
back path round-trips, token storage, send, refresh-with-vault-write
(ephemeral flag set, **no snapshot dir created**), error-contract fuzz
(§5.1a: upstream 401/5xx detail carries no URL/body; DNS-fail maps to
`transport`; deadline maps to `timeout`), canary refresh token never in
any cell-visible byte. Live-verify once against real Gmail (bench
checklist, manual).
V14. CLI frozen strings byte-exact: `audit verify`, `policy add|list|rm|reset`,
`snapshots list`, `surrogate list`; exit codes per §11 conventions
(`policy add` prints the domination `note:` line to stderr, exit 0).
`custos restore <gen>` behavior test: mutate vault, restore prior gen,
assert vault AND registry both land on the restored generation (a
surrogate created after gen G is gone after restore), audits present.
V15. Crash window: `SIGKILL` custosd mid-turn ⇒ refusals/connection
refusals; restart ⇒ locked, zero loaded tables; audit tail
`custos_restarted_after_crash`; hub settles cards; WAL recovery path
runs (intent-without-commit reconciled).
V16. Rebinding guard both directions: fake DNS flips approved host
public → 169.254.169.254 between park and dial ⇒ forward-time floor
refuses; reverse (approve-time floor-adjacent IP, forward-time public)
dials; floor amended-list test: `100.64.0.0/10` and `0.0.0.0/8`
targets refused under an `auto` rule (CA-2(iii) — the floor is not a
policy).
V17. Standalone: `custosd serve` without hearthd — CLI over ctl.sock
works; `keys` CLI is **daemon-first, file-direct only with keyfile
mode or TTY unlock** (frozen output lines identical; an unlock prompt
may add TTY lines — it is not part of the frozen output); file-direct
serializes on `custos.lock` flock; daemon + CLI concurrently ⇒ no
clobber (fuzz interleaving).
V18. Docker reference compose: `custos status` reports `locked` at boot
(not an error); `hearthd serve` with custos present-but-locked keeps
web + REPL fully alive (refusals only on credential paths).
V19. Single door (S11): with custody configured, assert the egress
port is held by custosd's pid only; attempting to start hearthd's
standalone proxy fails with the loud refuse line; no DNAT path exists
that reaches a non-custody listener.
V20. Response scrub (S12): text response echoing the swapped secret
arrives `[redacted]`; gzip-echo (upstream ignoring identity-encoding
after `Accept-Encoding` was stripped — assert the strip holds *and* a
forced `Content-Encoding: gzip` body is refused as binary-typed) never
delivers the secret; `application/octet-stream` echo refused without
`allow_binary`; >1 MiB text **connection-closed** (no partial body,
`response_size` audit), never framed as delivered.
V21. Revocation-vs-park race: park an `ask` flow, `custos revoke` the
credential mid-park ⇒ card cancelled (`credential_revoked`), parked
flow closed, a late Allow click cannot swap (assert the dial counter
stays zero — the non-tautological version).
V22. Anchor integrity: forge/replace `anchors.json` after tampering
earlier lines (attacker without `vault.key`) ⇒ verify fails; prune
then verify passes; anchors never move backward (V22 pairs with S5).
V23. Floor family honesty: `http://[::ffff:169.254.169.254]/` refused
by the floor (mapped un-map, CA-2(v)); `deny 203.0.113.7` rule matches
`http://203.0.113.7:80/` (normalized socket-address match).
V24. Ordering law over overlap (no conflict rejection): with
`auto cdn.tracker.com/*` stored, `policy add deny tracker.com/*`
stores with the `note: overridden by auto cdn.tracker.com/*` warning
(pairwise domination warning, §6.1); a canonical request to
`cdn.tracker.com` still resolves `auto` (longer match wins) while
`www.tracker.com` resolves `deny` — the test asserts the comparator
and the warning, never a write refusal. Same-verdict overlap stores
silently; Always (canonical exact form) always stores and outranks
(§6.5).
V25. Durability + quiescence: crash-injection at each §4.2 arrow
(intent/commit, fsync-before-rename kill -9) ⇒ first unlock recovery
⇒ `custos audit verify` exits clean (no dangling-intent alarm); WAL
lines flushed (kill between intent and rename leaves the intent
readable). Post-restore quiescence included: crash with a dangling
intent, `custos restore` a prior gen, first unlock ⇒ verify clean
(restore closes the dangling intent, §4.2).
V26. Parked knobs: cell A filling its per-cell cap never sheds cell B's
flows (S6) and never starves the lane; `ask_hold_timeout` expiry closes
the flow, settles the card with reason `timeout`, and a late Allow
audits `stale_verdict` with the dial counter at zero; a POST parked
with an unread body receives the 403 (no `ECONNRESET` — denial-drain,
§7). Custosd respawn mid-cell-life: after restart, booted cells'
listeners are re-pulled and live again (CA-2(iv), V19-adjacent).

## 11. CLI surface (frozen shapes)

`custos init | unlock | lock | status | migrate | export <name> | login
<connector> [--manual] | surrogate add|list|revoke | revoke <name>
(`keys rm <name>` under custody — one owner; `keys rm` performs,
`revoke` is its alias, §4.4) | policy
add|list|rm|reset
| audit verify | snapshots list | restore <gen> --yes` — all new verbs
live in the `custos` namespace; KEYS-SPEC's frozen `keys` family gains
nothing (CA-1). `status` is machine-parseable JSON
(`{state: locked|unlocked|degraded, credentials: n, surrogates: n,
surrogates_dropped: n, door: custosd|none, listeners_failed: n}` —
`degraded` whenever any listener bind failed or a WAL recovery is
unresolved; `door` reports daemon liveness only, not per-cell bind
health — that is `listeners_failed`). List commands
(`surrogate list`, `policy list`, `snapshots list`) are line-oriented:
one record per line, tab-separated columns in
header-declared order, no decoration — V14 asserts the headers and one
round-trip parse, not decoration. Errors to stderr, data to stdout,
exit 0 on success/ack, non-zero on refusal/parse — the same
conventions KEYS-SPEC established (stdout data / stderr errors / exit
codes).

## 12. Open questions (PO)

Q1. **Unlock friction.** v1 requires unlock after every restart
(P4/C4). Keyfile mode is the documented opt-in comfort path
("protection level: file-permissions-only", honest framing per
KEYS-SPEC §0). Recommend: ship keyfile well-documented, no auto-default.
Q2. **Gmail as v1 built-in** — right horse? Alternatives: IMAP/SMTP
(every user has it, uglier OAuth) or Calendar (smaller scopes).
Recommend Gmail: biggest wow, biggest scary-proof; the OAuth tax gets
paid now either way.
Q3. **Bearer lane TLS.** Honest passthrough (this draft) vs MITM CA
lane in v1 (user-CA wizard + trust enrollment + support load).
Recommend roadmap; worker lane covers the sensitive majors meanwhile.
Q4. **Egress default for credential-less flows.** default-auto (this
draft; zero migration surprise) vs default-ask on new destinations.
Recommend auto + a prominent `custos egress strict` toggle in
onboarding.
Q5. **Audit retention 90d** — short for the moat story? Recommend 365d
(names + hashes are cheap).
Q6. **Surface-mint confirmation card (§5.3)** — right posture, or
should surface minting be TTY-only (no card path)? Recommend the card:
it keeps headless ops workable while keeping the human in the loop;
TTY-only is the fallback if the card path proves awkward in dogfood.

## Changelog

- v4 (draft): round-3 review folded (pass A 7 blocking / 4 minor;
  pass B 3 / 9+ — both passes independently caught the v3
  conflict-strict-parse deadlock). Headlines: **conflict rejection is
  gone** — one comparator (longest normalized pattern wins), overlaps
  store with a domination `note:` warning, `custos policy add` added
  as the manual writer, Always always stores (exact form outranks);
  **WAL supersede fixed** (restore closes dangling intents at swap —
  post-restore verify quiesces, V25); **scrub scope split** (worker
  detail = whole loaded set; bearer response = swapped secret only +
  8-char scrub floor inherited); **denial-drain rule** (drain unread
  bytes before writing parked-flow refusals — bare close = RST = cell
  never sees the 403, V26); CA-3 reason enum gains `flow_gone`,
  hold-expiry uses frozen `timeout`; `stale_verdict` added to kinds,
  `vault_mutation_aborted` bound to refused mutations; CA-1 lettering
  fixed (d/e swapped) and locked `sha256_8` = stored hex everywhere;
  §6.3 IP-literals match IP-form rules (V23 now follows from the
  text); **CA-2(iv) start-pull** (`list_listeners` on custosd boot —
  respawn no longer leaves cells egress-dark, V26) + named the two
  retries separately + ports {80,443} closed; C3 keyfile mode =
  unlocked-from-boot, locked-mode memory list closed (fingerprints,
  vault.key, HKDF outputs); refresh write-back revalidates credential
  existence (no resurrection race); startup crash-tail line readable
  locked (WAL file non-empty); §6.6 remedies split per file
  (policy→reset, vault→restore; policy not snapshotted); §4.2
  in-memory swap inside the flock (K6); status gains
  `listeners_failed`; V2 renamed to `custos migrate/export`; V21 split
  (V26 for park knobs); V22 self-ref fixed; S7 Allow re-validates
  policy verdict too.
- v3 (draft): round-2 review folded (pass A 16 blocking / 4 minor;
  pass B 9 / 9 — independent fresh-context reviewer). Headlines
  from pass B: **fsync chain** in §4.2 (temp fsync before rename, dir
  fsync, flushed WAL) + flock acquisition timeout `lock_wait_timeout`;
  refresh runs under a **per-credential refresh lock with the forward
  outside the mutation flock** (coalesced); verdict vocabulary closed
  (`auto|ask|deny` identical meaning both lanes) + **write-time
  conflict strict-parse over table ∪ always-rules**; `custos login`
  listener one-shot + `ctl.token`-gated; **worker dispatch as the
  §6.2 consult point** with connector-declared host sets; parked flows
  read headers-only, disconnect cancels card (`flow_gone`), stale
  Allow audits `stale_verdict`, V-cases for both knobs; floor
  **family honesty** CA-2(v) (mapped-v4 un-map, v6 link-local added,
  IP-form rules match normalized socket address, V23); scrub set =
  every loaded secret field individually (oauth2 sub-fields named);
  IDNA2008 validation at rule write; `credential_added` wired to login
  + custody-routed `keys set`; WAL intents **resolve** to clean verify
  (V25 quiescence); Allow re-validates policy verdict, not just
  vault/registry.
  Pass-A headlines: recovery moved from
  boot to **first unlock** (locked daemon can't read age state — the
  old boot-reconcile text was impossible); WAL intents paired by
  **nonce + generation, never mtimes**, with `aborted`/`superseded`
  closing kinds and recovery-crash-safe self-WAL; decision = swap/dial
  **commit** — parked asks hold no decision, Allow re-validates,
  revoke cancels pending cards via a new hub `CancelByCredential` hook
  (CA-3); scrub requests identity encoding (`Accept-Encoding` stripped,
  `Content-Encoding` responses refused) and >1 MiB / binary responses
  close the connection instead of corrupting framing; §6.3 gains FQDN
  trailing-dot strip + punycode homograph rule and a single longest-
  normalized-pattern ordering law shared with §6.1; single-door
  listener ownership made dynamic (bind/close events over ctl.sock —
  gateway addrs don't exist until cell create) with `listener_bind_-
  failed` fail-closed audit; `vault.age.meta` replaced by an
  instance-key envelope MAC (`vault.key` + `vault.age.mac`, kills the
  meta-corrupt restore deadlock and whole-file substitution); restore
  is offline, MAC-verified, `--yes`-gated; anchors are MAC'd and
  forward-only (V22); OAuth login gains `state` enforcement, `--manual`
  headless path, client-id flag + prompted secret; access-token-only
  refreshes are ephemeral (no snapshot eviction); parked-flow caps are
  per-cell with fair global shedding (S6); locked `/v1/keys` GET/PUT
  behavior frozen (CA-1(d), name mirror = fingerprints sidecar);
  worker error enum gains `timeout`/`transport`; `migrate`/`export`
  moved to the `custos` namespace (KEYS-SPEC's frozen list gains
  nothing — CA-1's own words); deny-path status unified to `403
  surrogate` + CA-3 body; K2 shadow-warning misuse replaced with a
  frozen custos-owned line; suite now V1–V25; citation sweep
  (KEYS-SPEC §4 not §9, V9 mis-ref gone, §C4-adjacent gone).
- v2 (draft): round-1 review folded (two independent hostile reviews,
  15 blocking + 16 minor, ~20 distinct defects). Headlines: vault
  inherits K4 name gate/cap and occupies K2 layer 2 (CA-1 rewritten);
  GET name set amended (no ghosts); single-door listener ownership
  (CA-2(iv), C1, S11/V19); locked = zero loaded state (C3/C4);
  generation persisted in the vault document; two-file commit order +
  boot reconciliation; WAL intent/commit + prune anchors (§8.1a/§8.3);
  OAuth2 fields + `custos login` + refresh-as-mutation (§4.5); worker
  error closed enum + scrub (§5.1a); bearer response scrub + binary
  refusal (S12/V20); full host/pattern canonical grammar, forward-time
  matching authoritative, port bindings (§5.3/§6.3); ask-hold timer
  decoupled from RequestPhaseTimeout (§7); decision-point revocation
  replacing the impossible "no-grace mid-flight" claim (S7); surface
  mint confirmation cards (§5.3); fingerprints sidecar for locked-mode
  sha256_8 (§4.6); bare `custos_locked` reason + frozen cell-string
  pair (CA-3); floor gains CGNAT/this-host ranges (CA-2(iii)); V-suite
  grew to V20 with V9 made non-tautological and restore behavior
  tested.
- v1 (draft): initial text. Doctrine from ARCHITECTURE §2.4; takeover
  promise from KEYS-SPEC §0; proxy custody from CELL-SPEC §4 +
  internal/proxy's Phase-4 comments; approval reuse per SURFACE-SPEC §6
  (CA-3); floor-is-not-a-policy per erratum E3 lineage.
