package memindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Index wraps an SQLite FTS5 search index over the memory/ tree.
type Index struct {
	db   *sql.DB
	home string
	warn func(string)
}

// Open opens or creates the index database at <home>/memory/index.db.
func Open(home string) (*Index, error) {
	dir := filepath.Join(home, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("memindex: mkdir %s: %w", dir, err)
	}

	dbPath := filepath.Join(dir, "index.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("memindex: open %s: %w", dbPath, err)
	}

	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS manifest (
			path TEXT PRIMARY KEY, mtime_ns INTEGER NOT NULL, size INTEGER NOT NULL
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("memindex: create manifest: %w", err)
	}

	_, err = db.ExecContext(context.Background(), `
		CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
			path UNINDEXED, title, body, tokenize='porter unicode61'
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("memindex: create docs: %w", err)
	}

	ix := &Index{
		db:   db,
		home: home,
		warn: func(string) {},
	}

	return ix, nil
}

// Warn sets a warning callback for skipped files. Default is no-op.
func (ix *Index) Warn(warn func(string)) {
	ix.warn = warn
}

// Sync walks the memory/ tree, reindexes changed files, and updates the manifest.
// Returns the number of files that were (re)indexed.
func (ix *Index) Sync(ctx context.Context) (int, error) {
	memoryDir := filepath.Join(ix.home, "memory")

	// Collect all .md files under memory/{people,projects,notes}.
	var files []string
	for _, sub := range []string{"people", "projects", "notes"} {
		root := filepath.Join(memoryDir, sub)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(p, ".md") {
				files = append(files, p)
			}
			return nil
		})
	}

	// Build current manifest from DB.
	type entry struct {
		mtimeNs int64
		size    int64
	}
	oldManifest := make(map[string]entry)
	rows, err := ix.db.QueryContext(ctx, "SELECT path, mtime_ns, size FROM manifest")
	if err != nil {
		return 0, fmt.Errorf("memindex: query manifest: %w", err)
	}
	for rows.Next() {
		var path string
		var mtimeNs, size int64
		if err := rows.Scan(&path, &mtimeNs, &size); err != nil {
			rows.Close()
			return 0, err
		}
		oldManifest[path] = entry{mtimeNs: mtimeNs, size: size}
	}
	rows.Close()

	// Build new manifest from filesystem.
	newManifest := make(map[string]entry)
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(memoryDir, f)
		newManifest[rel] = entry{mtimeNs: info.ModTime().UnixNano(), size: info.Size()}
	}

	// Determine changed/new files and vanished files.
	changed := make(map[string]bool)
	for rel, ne := range newManifest {
		oe, exists := oldManifest[rel]
		if !exists || oe.mtimeNs != ne.mtimeNs || oe.size != ne.size {
			changed[rel] = true
		}
	}
	for rel := range oldManifest {
		if _, exists := newManifest[rel]; !exists {
			changed[rel] = true
		}
	}

	// Begin transaction.
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("memindex: begin: %w", err)
	}

	reindexed := 0

	// Process changed/new files.
	for rel := range newManifest {
		if !changed[rel] {
			continue
		}
		fullPath := filepath.Join(memoryDir, rel)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			ix.warn(fmt.Sprintf("memindex: read %s: %v", rel, err))
			continue
		}

		title, body, err := parseFile(fullPath, data)
		if err != nil {
			ix.warn(fmt.Sprintf("memindex: parse %s: %v", rel, err))
			continue
		}

		// Delete old row if exists.
		_, _ = tx.ExecContext(ctx, "DELETE FROM docs WHERE path = ?", rel)
		_, _ = tx.ExecContext(ctx, "DELETE FROM manifest WHERE path = ?", rel)

		// Insert new doc.
		_, err = tx.ExecContext(ctx, "INSERT INTO docs(path, title, body) VALUES (?, ?, ?)", rel, title, body)
		if err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("memindex: insert doc %s: %w", rel, err)
		}

		// Insert new manifest entry.
		ne := newManifest[rel]
		_, err = tx.ExecContext(ctx, "INSERT INTO manifest(path, mtime_ns, size) VALUES (?, ?, ?)", rel, ne.mtimeNs, ne.size)
		if err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("memindex: insert manifest %s: %w", rel, err)
		}

		reindexed++
	}

	// Delete vanished files.
	for rel := range oldManifest {
		if _, exists := newManifest[rel]; !exists {
			_, _ = tx.ExecContext(ctx, "DELETE FROM docs WHERE path = ?", rel)
			_, _ = tx.ExecContext(ctx, "DELETE FROM manifest WHERE path = ?", rel)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("memindex: commit: %w", err)
	}

	return reindexed, nil
}

// Close closes the index database.
func (ix *Index) Close() error {
	return ix.db.Close()
}
