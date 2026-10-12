# CONTEXT: frozen spec sections referenced by the draft (verbatim excerpts)

=== PENATUS-SPEC.md §1 tree + rules ===
---

## 1. The household tree

One directory per user (the "hearth root"), canonical name `agent/` inside the cell home:

```
agent/
├── SOUL.md            # values, boundaries, voice        (slow-changing)
├── IDENTITY.md        # name, emoji, avatar, self-facts   (slow-changing)
├── USER.md            # who the human is                  (slow-changing)
├── MEMORY.md          # curated long-term memory          (agent-maintained)
├── HEARTBEAT.md       # proactive standing orders         (user-authored)
├── memory/
│   ├── people/
│   │   └── <slug>.md
│   ├── projects/
│   │   └── <slug>.md
│   ├── notes/
│   │   └── YYYY-MM-DD.md
│   └── index.db       # REBUILDABLE: BM25 + sqlite-vec index (not in exports)
├── sessions/
│   └── <session-id>/
│       ├── session.json                    # session header
│       └── events.jsonl                    # the append-only transcript
└── archive/            # rotated / deleted-but-tombstoned material
```

Rules for all markdown files:

- **Header block:** YAML frontmatter (`---` delimited) is the *only* machine-parsed
  region. Body below is free markdown. Unknown frontmatter keys are preserved
  round-trip and ignored on read.

=== PENATUS-SPEC.md §2 integrity rules ===
**events.jsonl** — one event per line, append-only, fsync on every line that isn't
mid-stream token. Schema: every event has `t` (type), `ts` (ISO-8601 UTC),
`seq` (monotonic per session, starts 1), and type-specific fields. Parsers skip
unknown `t` values.

Core event types (v1):

| `t` | Payload fields | Meaning |
|---|---|---|
| `msg` | `role` (`user\|assistant\|tool`), `text`, `src` (provenance, below), `model`, `usage`, `update_id` (optional; A3, NUNTIUS-SPEC §7.4: integer, present on both `msg` events — user and assistant — of a nuntius-initiated turn; absent for web/repl. Additive: parsers and field-set verification treat it as optional) | a completed message (streaming deltas are NOT persisted) |
| `tool_call` | `call_id`, `name`, `args`, `approval` (`auto\|approved:<id>\|denied:<id>`) | dispatch record |
| `tool_result` | `call_id`, `ok`, `result_digest`, `result_ref` (path under `sessions/<id>/blobs/` if > 8 KB) | outcome |
| `compact` | see §3 | compaction checkpoint |
| `tombstone` | `seqs: [n…]`, `reason` | marks earlier messages deleted |
| `branch` | `from_session`, `from_seq` | side-chat provenance |
| `meta` | `key`, `value` | rename/title/pin changes |

**Provenance (`src` on `msg`):** `{channel: "pwa|telegram|android|api|heartbeat",
device: string|null}` — answers "where did this instruction come from" for the audit
story and for trust scoring later.

**Deletion semantics:** deleting a message appends a `tombstone`
event naming the target `seq`s. Renderers and the prompt assembler skip
tombstoned messages. Physical removal happens only in `archive/` during retention
sweep, which is itself an event. Rewriting `events.jsonl` in place is never valid.

**Integrity rule:** `seq` must increase by 1. A gap or a duplicate means corruption;
hearthd quarantines the session (opens it read-only, flags it in the UI) rather
than guessing.


=== PENATUS-SPEC.md §4 import/export (full) ===
## 4. Import / export

**Export** = zip of the hearth tree (all of §1–§2, minus `index.db`, minus blobs
unless `--full`), plus:

```json
export.json
{ "penatus": 1, "exported": "…", "product": "hearthd/0.x",
  "counts": {"sessions": 12, "memory_files": 34}, "sha256_manifest": true }
```

**Import contract:**

1. Read `export.json.penatus`. Major `1` → proceed. Unknown major → refuse with
   pointer to migration docs (never guess).
2. Minor newer than us → import anyway, dropping unknown fields/events (rule 4),
   report what was dropped.
3. Import never merges silently into an existing hearth: it lands as a new
   hearth directory, or into an existing one only with `--merge` and a full
   conflict report first (same-slug memory files = rename, not overwrite).
4. Importers for foreign formats (ChatGPT `conversations.json`, generic markdown
   dumps) live *outside* the spec — they target it, they don't bend it.

**Sync stance (v1):** no sync protocol. The tree is plain files, so `restic`/`syncthing`
work today; a first-party sync is a later client of this spec, which is exactly why
rule 1 (no hidden state) is non-negotiable.


=== CUSTOS-SPEC §4.2 protocol (excerpt) ===
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
encrypt → temps in `<hearth>/custos/` — `vault.age` **and**
`vault.age.mac` are written as a temp pair, both fsynced, then renamed
**mac first, envelope second** — so a crash mid-pair leaves (new mac,
old envelope), which first-unlock recovery resolves (§4.2
landedness), never a mismatched locked-out state → **fsync(dir)** →
WAL commit (**flushed, not buffered**, and fsynced only **after** the
rename-pair and dir fsyncs have returned — the inverse order is
forbidden: a commit line must never be durable ahead of the renames
it commits, or a power loss with commit-caching could pair a durable
`vault_mutation` with a lost envelope, falsifying a chain record) — the
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
nonce matching a commit nonce closes the pair. **Landedness doctrine:**
because `generation` lives inside the age-authenticated vault
document, a mutation has **landed** iff the decrypted document's
generation equals a dangling intent's generation. First unlock
(passphrase in hand — recovery never runs locked; **recovery runs
under `custos.lock` acquired before the first audit append, and a
concurrent second `unlock` is serialized behind it — recovery is
exactly-once; a locked daemon's `verify`/`status` needs no recovery
state: it reports the WAL's dangling-intent lines file-read-only and
resolution awaits the first unlock**): for each dangling
intent, landed ⇒ append `vault_mutation_recovered` (the rename was the
apply point; recognizing it is not auto-applying) and re-emit
`vault.age.mac` from the surviving envelope bytes if it lags; not
landed ⇒ append `vault_mutation_aborted` reason `crashed_pre_rename`,
unlink stale temps, and re-emit `vault.age.mac` over the surviving
envelope. Either way the intent is **resolved** and the pair on disk
is consistent before strict MAC verification ever rejects an unlock
(unlock-time verification runs *after* first-unlock recovery, §4.1);
nothing is ever *inferred without the pairing or generation-match
evidence*, and `custos restore` remains the only way to move the vault
backward (§8.4). An intent is closed `vault_mutation_superseded` in
exactly two ways:
the vault's persisted generation is greater than the intent's (a
later checkpoint rewrote the journal forward), or **`custos restore`
closes every currently-dangling intent at swap time** (restore runs
offline under the flock and is itself a checkpoint operation: no
post-restore boot can ever apply the superseded mutation, and the
restore audit line is the proof).
Recovery's own vault rewrite runs its *own* intent/commit pair marked

=== CUSTOS-SPEC §8.4 (full) ===
**8.4 — Snapshots & restore.** Before every vault mutation: the
pre-image set (`vault.age`, `vault.age.mac`, `surrogates.age`,
`fingerprints.json`) is copied
to `snapshots/gen-<G>/` (keep 8, G = persisted generation, no collision
across restarts; **ephemeral access-token refreshes write no snapshot**,
§4.5 — routine hourly churn can never evict the history that matters).
`policy.json` is **not** snapshotted (no secret material; §6.6's remedy
for policy corruption is `custos policy reset` + re-declare, not
restore). `fingerprints.json` **is** (plaintext, name-mirror rows):
a restore that rolled the vault back past a rename while mirror rows
stood forward would transiently misroute CA-1(d) locked `PUT`s as
new-name 423s — the snapshot set keeps vault and mirror moving
together.
`custos snapshots list` prints generations. `custos restore <gen>
--yes` runs **offline under the `custos.lock` flock** — it verifies the
snapshot's `.mac` with `vault.key` (no passphrase needed; the daemon
being down or locked is exactly when restore exists), renames the set
into place atomically, and appends the `vault_mutation_intent`/
`vault_mutation` pair with reason `restore` to the audit chain — **if
`anchors.json` is missing or empty, restore bootstraps it from the
genesis line before verifying (an empty anchor file is not "verified
against nothing")**, the
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

=== CUSTOS-SPEC §11 CLI conventions ===
## 11. CLI surface (frozen shapes)

`custos init | unlock | lock | status | migrate | export <name> | login
<connector> [--manual] | surrogate add|list|revoke | revoke <name>
(`keys rm <name>` under custody — one owner; `keys rm` performs,
`revoke` is its alias, §4.4) | policy
add|list|rm|reset
| approvals [watch] | audit verify | snapshots list | restore <gen> --yes` — all new verbs
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

## 12. Open questions — RESOLVED (PO sign-off, 2026-10-05)

=== SURFACE-SPEC §3 auth/tokens (excerpt) ===

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
