# Penatus Specification — v0.1 (DRAFT, for Gate G1)

> The file layer is the product's memory. This document freezes the on-disk formats
> that every Lararium component — hearthd, the PWA, importers, nuntius, the Android app —
> read and write. Anything not in this spec is not part of the contract.

Status: **APPROVED — Gate G1 cleared 2026-09-28 (Owner: draft picks on all four
open questions).** §6 records the resolved decisions; this document is now the
frozen contract. Changes require a version bump discussion.
Format version: `penatus/1`. Every bundle, file header, and event carries this tag.

Design rules (from ARCHITECTURE.md §1, restated as format law):

1. **Human-readable first.** Everything is UTF-8 markdown or JSONL. A user may open any
   file in `nano`, edit it, and have the agent still work. Any file that requires a
   custom parser to understand is a spec bug.
2. **Append-only transcripts, deletable facts.** Sessions are immutable logs;
   deletion is expressed as events (tombstones), never as rewriting history.
3. **Everything versioned, nothing assumed.** File headers carry explicit version
   tags; the loader never infers a version from structure.
4. **Forward compatibility by ignoring.** Parsers must skip unknown fields, unknown
   block types, and unknown event types without failing. Unknown *required* version
   prefixes fail loudly.
5. **Export is a tar/zip of the tree.** If it's not in the tree, it doesn't exist.
   No hidden state in databases that isn't rebuildable from the tree.

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
- **Required frontmatter keys:** `penatus: 1`, `updated` (ISO-8601 UTC).
- **Slug rule** (memory files): `^[a-z0-9][a-z0-9-]{0,62}$`, lowercase, hyphenated,
  derived from the entity name; collisions get `-2`, `-3`…
- **Concurrency:** hearthd is the single writer. Other tools edit via hearthd's API
  (the PWA memory editor, the Android editor) or while hearthd is stopped.
  Writers use write-to-temp + atomic rename. No file locking semantics are promised.

### 1.1 SOUL.md — values and boundaries

| Key | Type | Notes |
|---|---|---|
| `penatus` | int | `1` |
| `updated` | ISO-8601 | |
| `voice` | string | optional one-liner; free text for the rest |

Body: free markdown. Conventions (not enforced): sections named `## Values`,
`## Boundaries` (hard "never do X" list), `## Voice`. **Boundaries entries are the
only markdown content that approval-policy docs may reference** (via anchor link),
because they are authored/machine-merged and structurally stable.

Budget: ≤ 4 KB injected. Overflow is *not truncated silently* — hearthd refuses to
start a session over budget and surfaces an editor link.

### 1.2 IDENTITY.md — the agent's self

Keys: `name` (required), `emoji`, `avatar` (path under `agent/`), `tagline`.
Body: free markdown ("what I am", origin story, whatever the user wants the model to
know about itself). Budget ≤ 2 KB.

### 1.3 USER.md — the human

Keys: `name`, `timezone` (IANA, required — schedules and diaries depend on it),
`language` (BCP-47, default from locale). Body: relationship notes, preferences,
standing instructions. Budget ≤ 4 KB.

### 1.4 MEMORY.md — curated long-term memory

Keys: `penatus`, `updated`. Agent-maintained; user may edit anytime (the agent
treats user edits as ground truth). Structure is a flat list of **memory lines**:

```markdown
- [p:high] [since:2026-09-28] Owner's alpha gate = core stable AND Android pairable.
- [p:med] bench-gpu runs the Qwen drafter on a P40.
```

- `p:` priority ∈ `high|med|low` (default `med`). High survives every compaction
  verbatim until the file itself needs rotation (below).
- One fact per line. Lines > 500 chars should be promoted to a `memory/` topic file
  and replaced with a pointer line: `- [p:high] → memory/projects/lararium.md`.
- Budget: 2,000 tokens at the *router-probed* tokenizer; over budget, hearthd prompts
  the agent to rotate oldest `low` lines into `memory/notes/` as a dated batch.

### 1.5 HEARTBEAT.md — standing orders

User-authored (agent may propose diffs, never self-edit — this file is the human's
control surface). Keys: `penatus`, `updated`, `cadence` (`hourly|daily|off`,
default `daily`), `quiet` (cron-ish quiet-hours, e.g. `22:00-07:00`, user TZ).
Body: checklist of standing tasks the heartbeat loop may consider each wake.
Empty body = heartbeat off.

### 1.6 memory/ — the recall tree

Every topic file: frontmatter `penatus: 1`, `title` (required), `type`
(`person|project|note`), `tags` (list), `updated`. Body: free markdown, sections
encouraged (`## Now`, `## History`, `## Facts`).

- `people/<slug>.md` — one file per person the agent knows.
- `projects/<slug>.md` — one per ongoing thing.
- `notes/YYYY-MM-DD.md` — daily append-only journal; headings `## HH:MM <topic>`.

`memory/index.db` is generated from the tree on startup if missing or stale
(mtime/size manifest). It is **never** in exports, sync, or backups — the tree is the
source of truth (rule 5).

---

## 2. Session transcripts

`agent/sessions/<session-id>/` with:

**session.json** (written once at creation, updated on rename/archive):

```json
{
  "penatus": 1,
  "id": "s_01J8…",
  "created": "2026-09-28T20:10:00Z",
  "kind": "main | side",
  "title": "string | null",
  "parent": "session-id | null",
  "model_pin": "router-profile-name | null"
}
```

`id` is an k-sortable unique id (`s_` + ULID) so directory listing = chronology.

**events.jsonl** — one event per line, append-only, fsync on every line that isn't
mid-stream token. Schema: every event has `t` (type), `ts` (ISO-8601 UTC),
`seq` (monotonic per session, starts 1), and type-specific fields. Parsers skip
unknown `t` values.

Core event types (v1):

| `t` | Payload fields | Meaning |
|---|---|---|
| `msg` | `role` (`user\|assistant\|tool`), `text`, `src` (provenance, below), `model`, `usage` | a completed message (streaming deltas are NOT persisted) |
| `tool_call` | `call_id`, `name`, `args`, `approval` (`auto\|approved:<id>\|denied:<id>`) | dispatch record |
| `tool_result` | `call_id`, `ok`, `result_digest`, `result_ref` (path under `sessions/<id>/blobs/` if > 8 KB) | outcome |
| `compact` | see §3 | compaction checkpoint |
| `tombstone` | `seqs: [n…]`, `reason` | marks earlier messages deleted |
| `branch` | `from_session`, `from_seq` | side-chat provenance |
| `meta` | `key`, `value` | rename/title/pin changes |

**Provenance (`src` on `msg`):** `{channel: "pwa|telegram|android|api|heartbeat",
device: string|null}` — answers "where did this instruction come from" for the audit
story and for trust scoring later.

**Deletion semantics (Muse parity):** deleting a message appends a `tombstone`
event naming the target `seq`s. Renderers and the prompt assembler skip
tombstoned messages. Physical removal happens only in `archive/` during retention
sweep, which is itself an event. Rewriting `events.jsonl` in place is never valid.

**Integrity rule:** `seq` must increase by 1. A gap or a duplicate means corruption;
hearthd quarantines the session (opens it read-only, flags it in the UI) rather
than guessing.

---

## 3. Compaction events

Compaction = *an event in the log*, restorable and auditable, not a rewrite.

```json
{"t":"compact","seq":412,"ts":"…",
 "covers":[7,411],
 "summary_text":"…model-produced summary…",
 "kept":[{"t":"msg","seq":99}, …],
 "tokenizer":"probed:<hash>","tokens_before":118000,"tokens_after":9200}
```

- `covers` = the contiguous seq range the summary replaces (inclusive).
- `kept` = references to individual messages inside `covers` that survive verbatim
  (tasks, decisions, entity mentions — the summarizer prompt must preserve these;
  the set is validated against the prompt template's checklist before the event is
  written).
- **Assembly rule:** prompt = everything after latest `compact`, *plus* its
  `summary_text` in place of `covers`, *minus* anything tombstoned. Two-level
  (a compaction covering a range that contains earlier compactions) must resolve
  transitively; golden tests pin this.
- The compactor is **the prompted summarizer (decision #8)**. Summaries are stored
  exactly as produced; if the user distrusts one, "restore from here" replays raw
  events from any seq.
- Compaction trigger: router-probed token count of the assembled prompt > 80% of
  probed context window, checked after every completed turn (never mid-stream).

---

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

---

## 5. Transport sketch (for the Android pairing, Phase 3+)

Not frozen here — only the constraints the file layer imposes on it:

- Pairing = grant a device read-write to exactly one hearth root, revocable,
  scoped tokens held by custos (Phase 4) — transport carries tokens, never keys.
- The API is the tree: sessions append events; files replace atomically. No
  device ever holds a partial-write or a second writer.
- Streaming chat = SSE/WS of in-flight deltas; **persistence stays hearth-side**
  (§2: deltas are not persisted), so a device dropping mid-stream loses nothing.

---

## 6. Decisions (RESOLVED at G1, 2026-09-28)

1. **File split: five persona files** (SOUL/IDENTITY/USER/MEMORY/HEARTBEAT). Muse
   parity; SOUL (values/boundaries) stays separate from IDENTITY (name/self-facts)
   because boundaries are policy-referenced and identity is cosmetic.
2. **MEMORY.md line format: inline tags** (`- [p:high] …`) with a lenient parser.
   Nano-friendliness wins; ambiguous lines degrade to `p:med` + verbatim text, never
   a parse failure.
3. **Retention: manual-only in v1.** Tombstone sweeps and `archive/` pruning are
   user-initiated (UI flag shows how much is tombstoned). No automatic deletion of
   a user's history ever, until a spec revision says otherwise.
4. **Token budget: refuse-to-start** on over-budget persona files, with an editor
   link in the error. Silent context loss is the worse failure mode.

*The shrine keeps the penates. This file is what "yours" means, byte-for-byte.*
