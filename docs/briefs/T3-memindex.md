# Task Brief T3 — `internal/memindex`: rebuildable memory search index

Spec: docs/PENATUS-SPEC.md §1.6 ("memory/ — the recall tree").
Module root: github.com/lararium-app/lararium. Go 1.24.
Allowed dependency: `modernc.org/sqlite` (pure Go, FTS5 verified working).
Do NOT touch anything outside internal/memindex/.

## What it is

`memory/index.db` is a REBUILDABLE FTS5 index over the `memory/` tree
(`people/*.md`, `projects/*.md`, `notes/*.md`). The tree is the source of
truth; the index is a cache. It is NEVER in exports/sync/backups. On startup,
rebuild if missing or stale (manifest of path → mtime+size per file).

## EXACT DESIGN (do not deviate)

### Files (all in internal/memindex/)

1. `index.go`
   - `type Index struct { ... }` wraps `*sql.DB` opened at
     `<home>/memory/index.db` (create dirs as needed).
   - `Open(home string) (*Index, error)` — opens/creates the DB, applies
     schema:
     ```sql
     CREATE TABLE IF NOT EXISTS manifest (
       path TEXT PRIMARY KEY, mtime_ns INTEGER NOT NULL, size INTEGER NOT NULL
     );
     CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
       path UNINDEXED, title, body, tokenize='porter unicode61'
     );
     ```
   - `Sync(ctx) (reindexed int, err error)` — walk `memory/people`,
     `memory/projects`, `memory/notes` recursively for `*.md`; for each file
     compare against manifest (mtime_ns + size); reindex changed/new files
     (delete row by path, insert new); delete rows for vanished files;
     update manifest in one transaction. Files that fail frontmatter parse
     are SKIPPED with a warning (a bad file must never break the whole tree).
   - `Close() error`.

2. `search.go`
   - `type Hit struct { Path, Title string; Score float64 }` — Score from
     bm25(), LOWER IS BETTER (bm25 negative; ORDER BY score ASC = best first;
     keep raw value, do not negate).
   - `(ix *Index) Search(ctx, query string, limit int) ([]Hit, error)` —
     quote-escape the query for FTS5 MATCH (wrap each token in double
     quotes, internal `"` doubled — never pass raw user text to MATCH).
     Return empty slice, not error, on no matches. Empty query → error
     `ErrEmptyQuery`.

3. `parse.go`
   - Topic files carry frontmatter (`title`, `type`, `tags`) per spec §1.6.
     Reuse `penatus.ParseDoc` (internal/penatus) — import it, do not
     duplicate the parser. Title comes from frontmatter `title` key; body is
     the markdown body.
   - Notes (`memory/notes/YYYY-MM-DD.md`) may lack `title`: fall back to the
     filename stem.

### Behavior rules (tests must cover each)

- Sync on fresh home creates empty index, no error, creates dirs.
- Add file → Sync → Search finds it; Search before Sync finds nothing new.
- Edit file (content change; to be safe force mtime change via os.Chtimes)
  → Sync → new content findable, old content NOT.
- Delete file → Sync → path gone from both docs and manifest.
- Corrupt frontmatter file (e.g. `---` opened, never closed) → Sync skips
  it (returns reindexed count excluding it, error is nil; log warning
  through a `func(string)` warn hook field on Index, default no-op).
- Query with FTS5 operators injected (`" OR ", AND, NOT, NEAR, *, "`, etc.)
  is treated as literal text and returns zero matches, not a syntax error.
- Two files, one matching: correct ranking path returned with correct title.
- Re-open (Close + Open without changes) → Sync reindexes 0 files (manifest
  works).

### Acceptance gate (paste real output before finishing)

```
go vet ./internal/memindex/ && go test ./internal/memindex/ -count=1 -v
```
All tests green. gofmt clean. Do not modify go.mod beyond what `go mod tidy`
would do for modernc.org/sqlite.
