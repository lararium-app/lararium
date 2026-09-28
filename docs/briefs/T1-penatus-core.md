# Task Brief T1 — `internal/penatus`: file-layer core

Spec: `docs/PENATUS-SPEC.md` (APPROVED v0.1 — the contract; read it fully first).
Module: `github.com/lararium-app/lararium`, Go 1.24, **stdlib only** (golangci-clean, no
third-party deps in this task; YAML frontmatter is flat key:value so a hand-rolled
parser is required — do NOT add a YAML library).

EXACT DESIGN (do not deviate):

## Files

1. `internal/penatus/doc.go` — package comment citing the spec file + section numbers.
2. `internal/penatus/frontmatter.go`
   - `type Doc struct { Meta map[string]string; Body string }`
   - `ParseDoc(b []byte) (Doc, error)` — leading `---\n` block of `key: value`
     lines (no nesting, no lists; unknown keys preserved verbatim), then body.
     A file with no frontmatter → error `ErrNoFrontmatter`. A frontmatter block
     not closed by `---` → error. Values with embedded colons: split on FIRST colon.
   - `func (d Doc) Marshal() []byte` — round-trip exact for any Doc parsed from
     valid input (key order = original order; store order in a parallel slice).
   - `Required(d Doc, kind string) error` — checks `penatus: 1` present and integer
     1; `updated` present when kind != "memory-line". Error values name the file kind.
3. `internal/penatus/memory.go`
   - `type MemLine struct { Priority string; Since string; Text string; Raw string }`
   - `ParseMemoryBody(body string) []MemLine` — LENIENT per spec §6.2: lines
     starting `- ` are entries; leading `[p:high|med|low]` and `[since:YYYY-MM-DD]`
     tags (either order, either present) extracted; anything malformed → Priority
     "med", Since "", Text = full trimmed line text after `- `, Raw kept. Never errors.
   - `SerializeMemoryBody([]MemLine) string` — canonical form `- [p:X] [since:Y] text`
     (omit absent tags). Must be idempotent with Parse above.
4. `internal/penatus/events.go`
   - `type Event struct { Seq int64; T string; TS string; Fields map[string]json.RawMessage }`
     with `func (e Event) MarshalJSON/UnmarshalJSON` flattening Fields alongside
     `seq`, `t`, `ts`. Unknown fields on read are KEPT in Fields (spec rule 4).
   - `OpenLog(dir string) (*Log, error)` on `sessions/<id>/events.jsonl`: reads,
     validates strict `seq` +1 from 1, gap/duplicate → `CorruptError{Got,Want}` and
     the Log opens read-only.
   - `(*Log).Append(Event) error` — assigns Seq (next) and TS (UTC now, RFC3339Nano)
     if empty, O_APPEND + fsync every line. Refuses on read-only Log.
   - `(*Log).Live() []Event` — events with seq in no tombstone's `seqs`, and if a
     `compact` event exists: assembly per spec §3 (summary replaces its `covers`
     range, `kept` members ride along, transitive over nested compactions).
5. `internal/penatus/session.go`
   - `CreateSession(root, id, kind string) (*Log, error)` — writes `session.json`
     per spec §2 (fields exactly: penatus,id,created,kind,title,parent,model_pin;
     title/parent/model_pin nullable). Refuses kind not in {main,side}.
   - ULID-ish id helper `NewID() string`: `"s_"` + 26 chars Crockford base32
     (time-prefixed 10 chars ms-epoch + 16 random) — sortable, no deps.
6. Tests `internal/penatus/*_test.go`, table-driven, stdlib testing only:
   frontmatter round-trip incl. value-with-colon; lenient memory lines (malformed,
   both tag orders, non-entry lines); log append→reopen→seq validation; corrupt gap
   detected; tombstone excluded from Live; compaction assembly incl. one nested
   compaction; Marshal/Unmarshal keeps unknown fields.

HARD RULES:
- NO third-party imports. NO placeholder/TODO bodies — every function fully implemented.
- Do not touch anything outside `internal/penatus/`.
- `go vet ./... && go test ./internal/penatus/ -v` must pass before you finish.
