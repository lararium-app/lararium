# KEYS-SPEC — provider credentials, entered once, never fumbled

Status: **DRAFT v4 — G1 review PASSED (round 5: zero blocking, zero
minor).**
Proposes the credential entry path for `hearthd` (config layer +
surface). Nothing here is built yet; every behavior statement is a
requirement, not an observation. Companion spec: SURFACE-SPEC (frozen; this spec carries one amendment,
K-A1: its §4 route table gains the three `/v1/keys` routes and its §2
reverse-proxy note gains request-body logging for them). The real vault
(`custos`, [ARCHITECTURE.md](ARCHITECTURE.md) P2)
remains the endgame; this is the honest interim, designed so custos can
take over underneath without changing a single command or endpoint.

## 0. The problem

A provider key today must arrive via YAML (`api_key_env: VAR` plus an
environment variable, or a literal `api_key:` in the config file). That
means: knowing shell env mechanics, editing YAML, restarting the daemon,
and — with the literal form — a secret sitting in a file people are
trained to paste into bug reports. The first thing every new user needs
to do (paste their OpenRouter key) is the second-hardest thing in the
setup. This spec fixes the entry path.

**Honest delta over env vars.** This is an ergonomics change, not a
security upgrade, and the spec says so: `keys.json` is a plaintext file
at a predictable path that survives logout and may ride backups, where
env vars lived only in process memory; and the HTTP endpoints give every
bearer-token holder a credential-write surface that env vars never had.
What it buys: one file with enforced `0600` (vs. whatever mode your
shell history and dotfiles have), no secret in YAML that gets pasted
into bug reports, no secret in shell history, and a reviewable
`keys list` instead of `env | grep -i key`. Custos is what buys actual
security at rest.

## 1. Decisions (frozen on G1 pass)

**K1 — One file, boring format, per-instance.** Keys live in
`<hearth>/keys.json` — anchored to the same instance root SURFACE-SPEC
uses for `tokens.json`, so two daemons with different `--config` never
contend for one file. A JSON object mapping provider name → secret.
Written atomically: temp file **created inside `<hearth>/`** (never
`$TMPDIR` — a cross-mount `rename` fails `EXDEV`), mode `0600` set on
the temp file before rename, parent dir `0700`, `rename(2)` over the
target. No encryption at rest in v1 (see §0's honest delta).

**K2 — Resolution order, with shadowing made visible.** At startup and
on reload, a provider's key resolves: (1) `api_key_env` environment
variable if set and non-empty **after the K3 strip** (a whitespace-only
env var is a miss, not a `set (env)` lie), (2) `keys.json[provider.name]`,
(3) literal `api_key:` in YAML. First hit wins; layers (2) and (3)
count as hits only when non-empty after the K3 strip — an empty
`api_key: ""` is a miss, not a `set (config)` lie. Layer (3) logs a
one-line startup warning (`provider "x": key in config file — prefer
'hearthd keys set x'`). **Shadow warning:** whenever layer (1) shadows
a *different* value stored in `keys.json` — the only true shadow,
since (2) outranks (3) by construction — startup and every reload log
`provider "x": keys.json entry shadowed by api_key_env — the web/CLI
editor updates the shadowed copy` (one line per provider per
resolution, no more). Backward compatible: every existing config keeps
working untouched.

**K3 — CLI entry.** Four commands, no daemon required:

```
hearthd keys set <provider>            # prompts, hidden input (no echo, no history)
hearthd keys set <provider> --file -   # read from stdin for scripting
hearthd keys list                      # names + status + sha256 prefix, never values
hearthd keys rm <provider>
```

`keys set` validates non-empty and strips surrounding whitespace
(newlines from pasted keys are the #1 silent failure). Input that is
empty or strips to empty — interactive prompt or `--file -` — is
refused: print `empty key`, exit non-zero, **no write and no reload
attempted** (the CLI's failure messages are frozen strings:
`empty key`, `invalid provider name`, `key store full`). It does **not**
validate the key's format — providers change formats; a 401 at first
use with a clear message is honest, a regex gatekeeper is not.
`keys rm` is **idempotent like DELETE** (K4): removing an absent name
prints `not set` and exits 0 — **short-circuit: no write, no socket
contact** (with no daemon running this must not print the
"will apply at next start" line; nothing was written).
`keys list` prints, per provider name (config ∪ keys.json):
`set (keys.json) | set (env) | set (config) | missing`, plus an 8-hex
SHA-256 prefix when a value resolves. It does **not** print key
lengths (length + fingerprint is a verification oracle for leaked-key
databases; the prefix alone is a display affordance, and no policy is
built on it).

**K4 — Web entry.** The chat page gains a small **Keys** drawer (same
origin, same bearer token, same CSP): lists provider names
(**config ∪ keys.json** — CLI-added and `force=true` keys must be
visible and deletable), shows the K2 status per provider, and offers
paste-and-save. Endpoints on the existing surface:

```
GET    /v1/keys          → [{name, status, sha256_8}]   (names = config ∪ keys.json, computed at request time; status ∈ "set (env)" | "set (keys.json)" | "set (config)" | "missing" per K2 resolution; sha256_8: string when a value resolves, null when missing)
PUT    /v1/keys/{name}   → body {"key": "..."}  204; 404 unknown provider unless ?force=true
DELETE /v1/keys/{name}   → 204 (removes the keys.json entry only; 204 even when absent — idempotent)
```

- **"Unknown provider" for PUT** means: not in config **and** not in
  keys.json. A name that exists in either accepts a plain PUT — the
  drawer's paste-and-save works on CLI-added and `force=true` keys
  without a force flag; force is only for names the store has never
  seen.
- **No ghosts:** `GET /v1/keys` computes its name set at request time
  from config ∪ keys.json — nothing else. A `force=true` key that is
  deleted leaves the list entirely (it is in neither set); a config
  provider whose keys.json entry is deleted, with no env backing,
  correctly shows `status: "missing"`. No tombstones, per P1.

- **Name validation** (SURFACE-SPEC §4/S4 doctrine): `{name}` must
  match `^[a-z0-9][a-z0-9_-]{0,63}$` or the request is a 404 with
  `{"error": "invalid provider name"}` — S4 freezes 404 for
  path-parameter gate failures, and these routes follow it; the
  message string is frozen here so both doors can be asserted
  byte-equal. This kills traversal, `%2f`, control characters, and
  `__proto__`-style JSON key injection at the gate. **The CLI applies
  the identical gate** to `hearthd keys set/rm <provider>` (reject
  printing `invalid provider name`, exit non-zero): one rule, both
  doors, so no malformed name can enter the store and become
  undeletable over HTTP.
- **Body validation:** the body must parse as JSON with exactly a
  string `key` field, non-empty after the same whitespace strip K3
  applies; violations → 400 `{"error": ...}`. 4 KiB body cap → 413.
  The PUT path stores the **trimmed** value, same as the CLI.
- **Entry cap:** at most 64 keys in `keys.json`; a `PUT` for a *new*
  name at the cap → 409 `{"error": "key store full"}` (overwrites of
  existing names always allowed). **The CLI enforces the identical
  cap** (`keys set` of a new name at 64 → refuse with the message
  `key store full`, exit non-zero). No unbounded monolith.
- **Serialization:** all key-store mutations (HTTP and CLI, §K6)
  serialize through an `flock` on `<hearth>/keys.lock` spanning the
  read-modify-write, so concurrent set/rm cannot lose an update.
  **One lock hold per mutation — no re-entrancy:** the in-process
  mutation path is a single critical section (acquire flock → read →
  modify → write → swap in-memory map → release), so the HTTP handler
  never re-acquires a lock it holds; the socket-triggered reload is a
  *separate* acquirer (acquire → re-read → swap → release) and only
  ever runs when no write hold is outstanding on that path (the CLI
  writes and releases *before* sending `RELOAD-KEYS`).
- **Transport honesty:** the page warns once per session, *before the
  first save*, when the connection is not provably local: the check is
  `location.hostname` ∈ {localhost, 127.0.0.1, [::1]} **or**
  `location.protocol === 'https:'` — an HTTPS page is encrypted
  end-to-hop and must not nag (telling a TLS user to "use HTTPS" is
  the bug this clause fixes). When neither holds, the warning reads:
  saving sends this key over the network in cleartext to this origin;
  load the page over HTTPS or use the CLI if that is not your
  machine. SURFACE-SPEC's reverse-proxy rule (§2: no header logging)
  extends to **request-body logging** for these routes — stated in
  both specs' operator notes.
- **Audit:** there is no route-level audit log in v1 (SURFACE-SPEC has
  none; keys routes do not invent one). The daemon logs key **names**
  and outcomes only (`keys: set "openrouter" by web`), never values —
  S5-style grep of daemon logs for any stored secret must come up
  empty.

**K5 — Never echo, never log.** No endpoint, CLI path, or log line ever
returns or writes a key value. Turn failures that are 401s get the
message `provider "x" rejected the key (401) — check with: hearthd
keys list` and nothing more.

**K6 — Reload without restart, done explicitly.** The daemon opens a
Unix socket `<hearth>/hearthd.sock` (0600) while serving. **Every
mutation path reloads, both doors:** the HTTP `PUT`/`DELETE` handlers
perform their write and in-memory swap in **one flock critical
section** (K4 serialization — no second acquire, no re-entrancy; the
swap happens before the lock releases, so a 204 never precedes a
visible key); `hearthd keys set/rm` writes `keys.json` under the same
flock, **releases it**, and only then sends `RELOAD-KEYS\n` on the
socket **and waits for the daemon's `OK\n` ack** (the socket reload is
a fresh flock acquirer: re-read, swap, then ack — so a script that
runs `keys set` and immediately starts a turn cannot race a stale
map). No ack within 5 s → the CLI reports the write succeeded but
warns `daemon did not confirm reload — check it is running` and
**exits 0** (the write — the CLI's contract — succeeded; the ack is a
best-effort convenience, and a non-zero exit would tell scripts the
write failed when it did not).
**Daemon discovery is connect-or-absent:** the CLI treats *any* socket
failure — missing file, `ECONNREFUSED` on a stale file left by a
crashed daemon, permission error — as "no daemon": the write already
succeeded, the CLI prints `no daemon running — will apply at next
start` and exits 0. On reload the daemon re-reads `keys.json` and
swaps its in-memory map **for new turns only**: a turn in flight keeps
the provider client it started with (SURFACE-SPEC P3 turn atomicity —
revoking a key never mutates a running turn mid-stream). A new turn
whose provider key was removed fails **locally** — no keyless request
is ever sent — with `provider "x" has no key — hearthd keys set x`
(the K5 no-secret rule applies to this message too). No SIGHUP: it kills daemons that
don't handle it and doesn't exist on Windows.

**K7 — First-run nudge, token-honest.** The first-run path cannot skip
auth: SURFACE-SPEC S2 401s every `/v1/` route without a token, and the
page gets its token from the URL fragment that `hearthd token create`
prints. So: `hearthd serve` with zero providers having resolvable keys
prints, after the banner:
`no provider keys yet — hearthd token create web, open the printed URL, and use Keys (or: hearthd keys set <name>)`.
The web page, **when loaded authenticated** with zero resolvable keys,
shows the Keys drawer open on first load instead of an empty chat.
Setup becomes: install → `token create` → open URL → paste key → talk.

## 2. Non-goals (v1)

- Encryption at rest, OS keyrings, KMS — custos' job, not this one.
- OAuth flows (Google etc.) — separate spec when connectors land.
- Editing a key in place — set-over-writes; there is no "view".
- Per-key scopes or expiry.
- A route-level audit trail — see K4; penatus covers turns, not admin
  routes, in v1.

## 3. Security notes

- keys.json is a **secret file**: documented in README, mode-enforced
  on every write, default path outside repos — and the README states
  the quiet part: it survives logout and rides backups.
- `PUT /v1/keys/{name}` bodies are capped at 4 KiB (keys are not
  novels) and the store at 64 entries (K4).
- Unknown provider names are refused unless `?force=true` — typo'd
  provider names silently doing nothing is the failure mode K2's
  warning also covers. Forced names still pass the K4 regex.
- The 8-hex fingerprint is a *display affordance*, not an integrity
  check; do not build policy on it; lengths are not shown (K3).
- `keys.lock` and `hearthd.sock` live in `<hearth>/` and are 0600;
  a stale socket file is unlinked at startup if no daemon answers.

## 4. V-suite (verification)

- **V1** resolution order: env beats file beats YAML literal; each
  layer alone resolves; removing layers falls through correctly; a
  keys.json entry shadowed by env logs the K2 shadow warning once per
  resolution.
- **V2** `keys set` on a paste with trailing newline stores the
  trimmed value — assert by reading `keys.json` bytes directly (the
  8-hex prefix is display-only and cannot byte-compare); same strip
  applies to `PUT /v1/keys/{name}` (store via web, byte-compare the
  file).
- **V3** file modes: fresh keys.json is 0600, dir 0700, even when the
  process umask is 022; the temp file is created inside `<hearth>/`
  (assert no rename crosses mounts: strace/`EXDEV` never observed).
- **V4** no secret in outputs: for a stored secret S, the exact string
  S appears in no `keys list` output, no `GET /v1/keys` body, and no
  daemon log line (grep the full value; substrings of ordinary JSON
  are not findings).
- **V5** PUT unknown provider → 404; with `?force=true` → 204 and the
  name appears in `GET /v1/keys` with `status: "set (keys.json)"`;
  DELETE of a **config** provider with no env backing → subsequent GET
  shows `status: "missing"`; DELETE of a `force=true` key with no
  config backing → the name disappears from the list entirely (no
  ghosts, K4); with an env layer present, DELETE removes the
  keys.json entry and status correctly reports `set (env)` — K2.
- **V6** 401 from provider surfaces the K5 message and never the key.
- **V7** first-run nudge: serve with no keys prints the K7 line
  including `token create`; with a token created and the page loaded
  from the printed URL, the Keys drawer is visible (V-playwright);
  saving a key against a **mock provider fixture** (CI stands up a
  local HTTP stub as the provider base URL) then yields a working
  chat round-trip. No live provider in CI.
- **V8** atomic write + concurrency: keys.json survives a kill during
  save (old content intact or new content complete, never mixed); two
  concurrent `keys set` for different providers (one CLI, one PUT)
  both persist (flock; assert final file has both keys).
- **V9** YAML literal still works and logs the K2 warning exactly once
  per provider per startup.
- **V10** validation matrix: `{name}` `__proto__`, uppercase, 65
  chars → 404 `{"error": "invalid provider name"}` via HTTP **and CLI
  `keys set` printing `invalid provider name`, exit non-zero**
  (byte-equal message, both doors); `..` and `%2f` → **any 4xx/3xx,
  body not asserted** (mux path-cleaning runs before routing — same
  doctrine as SURFACE-SPEC V4; the gate must not be *bypassed*, which
  is what the assertion proves); body missing `key`, non-string,
  empty → 400; CLI empty/stripped-empty input (prompt and `--file -`)
  → `empty key`, exit non-zero, keys.json bytes unchanged; 5 KiB body
  → 413; 65th new key → 409 `key store full` via HTTP **and CLI
  (message `key store full`, exit non-zero)**; overwrite at cap →
  204.
- **V11** reload, both doors: daemon running, `keys set` over an
  existing provider → CLI blocks until the `OK\n` ack, then the next
  new turn uses the new key (mock provider observes the changed
  Authorization value) with no daemon restart; a turn already in
  flight completes with the old key (K6 new-turns-only). Same
  assertion for `PUT /v1/keys/{name}` (web save → next turn uses the
  new key without restart — the in-process reload path).
- **V12** no-daemon CLI: `keys set` with the daemon stopped succeeds
  and prints the "will apply at next start" line; **stale
  `hearthd.sock` (file present, no listener): `keys set` still exits 0
  with the same line (ECONNREFUSED = no daemon, K6)**; the stale
  socket is unlinked at next startup, which then binds cleanly;
  `keys rm <absent-name>` with no daemon prints `not set`, exits 0,
  and never prints the "will apply at next start" line (K3
  short-circuit).
- **V13** transport warning (V-playwright): page served over HTTP on
  a non-loopback hostname → first Keys-drawer interaction shows the
  cleartext warning before any save, once per session; same page on
  `localhost` → no warning; an `https:` page on any host → no warning
  (mock `location.protocol`).
- **V14** whitespace-only env var (`export KEY="  "`) → resolution
  treats it as a miss (status falls through to keys.json/config), no
  K2 shadow warning fires, and `keys list` never reports
  `set (env)`.

## 5. Changelog

- **v4 + G1 pass.** Round 5 returned ZERO BLOCKING, ZERO MINOR —
  review closed; spec approved for the build queue (unimplemented as
  of this changelog).
- **v4.** G1 round 4 applied (3 blocking, 3 minor): flock
  re-entrancy self-deadlock removed — HTTP mutation is *one* critical
  section (write + swap before release; 204 never precedes a visible
  key), socket reload is a separate acquirer that only runs after the
  CLI released its hold; V10 traversal inputs (`..`, `%2f`) assert
  "any 4xx/3xx, body not asserted" (mux path-cleaning runs before
  routing — SURFACE-SPEC V4 doctrine) instead of an untestable
  byte-equal 404; CLI empty-input failure frozen (`empty key`, exit
  non-zero, no write — V10 covers the CLI door); whitespace-only env
  var is a resolution miss (K2 layer 1 strips; V14 asserts no
  `set (env)` lie, no phantom shadow warning); transport warning gets
  V13 (playwright, incl. HTTPS-quiet clause); `keys rm` of an absent
  name short-circuits before socket discovery (no bogus "will apply"
  line; V12 asserts).
- **v3.** G1 round 3 applied (3 blocking, 3 minor): CLI cap/gate
  messages frozen as strings (`key store full`, `invalid provider
  name`) so V10's byte-equal both-doors assertion is testable (exit
  *codes* never carry HTTP semantics); reload-timeout exit defined
  (0 — the write is the contract); K2 layers (2)/(3) require non-empty
  after strip (no `set (config)` lie for `api_key: ""`); `keys rm`
  idempotent like DELETE; V2/V11 route paths corrected to
  `PUT /v1/keys/{name}`; K6 revocation wording fixed — a new turn with
  no resolvable key fails locally with a clear message, never sends a
  keyless request to fish for a 401.
- **v2.** G1 round 2 applied (4 blocking, 5 minor): HTTP
  `PUT`/`DELETE` now reload in-process — web saves no longer serve
  stale keys until restart (K6 "both doors"); CLI treats any socket
  failure incl. `ECONNREFUSED` on a stale socket as "no daemon" (V12
  asserts exit 0); K2 shadow warning scoped to the only true shadow
  (env over keys.json — layer 3 can never shadow layer 2); `GET
  /v1/keys` computed request-time from config ∪ keys.json — deleted
  `force=true` keys leave the list, no ghosts (V5 covers both delete
  cases); "unknown provider" for PUT defined (config ∪ keys.json —
  drawer re-saves CLI-added keys without force); name gate aligned to
  S4 (404, not 400) and applied identically to the CLI; CLI enforces
  the 64-entry cap; reload ack (`OK\n`, 5 s) closes the
  set-then-turn race; transport warning stays quiet on HTTPS.
- **v1.** G1 round 1 applied (12 blocking, 8 minor): dropped the
  concurrency-gate "rate limit" claim (it is a per-session mutex, not
  a rate limiter) and the phantom "audit rules" claim (no route audit
  exists; names-only logging stated instead); first-run flow made
  token-honest (K7 routes through `token create`; drawer only when
  authenticated); keys.json mutations serialized under flock (no lost
  updates); SIGHUP replaced by a `RELOAD-KEYS` socket command with
  new-turns-only swap (no signal-kills-daemon hazard, no mid-turn
  mutation, no daemon-discovery gap); shadowed-keys.json warning added
  so web/CLI edits can't silently no-op behind `api_key_env`;
  `force=true` bounded by a 64-entry cap; `{name}` gets the SURFACE-SPEC
  regex gate; K4's drawer contradiction resolved (config ∪ keys.json);
  temp file pinned inside `<hearth>/` (EXDEV); cleartext warning
  rewritten to what the client can actually know, plus the
  reverse-proxy body-logging rule; `set`/`sha256_8` semantics defined
  as K2 resolution status (V5 corrected); PUT trims and validates;
  V2 byte-compares the file not the fingerprint; V4 reworded to the
  exact-string assertion; V7 gets its mock-provider fixture; key
  lengths dropped from list output; store moved from `$LARARIUM_HOME`
  to `<hearth>/` (per-instance).
- v0 (this draft): initial spec for G1.
