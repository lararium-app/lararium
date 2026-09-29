package memindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tmpHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memindex-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func writeMD(t *testing.T, dir, rel string, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(full), 0755)
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func mustOpen(t *testing.T, home string) *Index {
	t.Helper()
	ix, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func mustSync(t *testing.T, ix *Index) int {
	t.Helper()
	n, err := ix.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestFreshSync creates dirs and syncs with no files.
func TestFreshSync(t *testing.T) {
	home := tmpHome(t)
	ix := mustOpen(t, home)
	n, err := ix.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync on empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 reindexed, got %d", n)
	}
}

// TestAddAndSearch: add file, sync, search finds it.
func TestAddAndSearch(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	content := "---\ntitle: Hello World\ntype: note\n---\nThis is a hello world note."
	writeMD(t, memoryDir, "notes/hello.md", content)

	ix := mustOpen(t, home)

	// Before sync, nothing.
	hits, err := ix.Search(context.Background(), "hello", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("before sync: expected 0 hits, got %d", len(hits))
	}

	n := mustSync(t, ix)
	if n != 1 {
		t.Fatalf("expected 1 reindexed, got %d", n)
	}

	hits, err = ix.Search(context.Background(), "hello", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Title != "Hello World" {
		t.Fatalf("expected title 'Hello World', got %q", hits[0].Title)
	}
}

// TestEditFile: edit content, sync picks up change.
func TestEditFile(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	content1 := "---\ntitle: Foo\ntype: note\n---\nOriginal content about apples."
	writeMD(t, memoryDir, "notes/foo.md", content1)

	ix := mustOpen(t, home)
	mustSync(t, ix)

	// Verify original content.
	hits, err := ix.Search(context.Background(), "apples", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for 'apples', got %d", len(hits))
	}

	// Edit and force mtime change.
	content2 := "---\ntitle: Foo\ntype: note\n---\nUpdated content about oranges."
	fullPath := filepath.Join(memoryDir, "notes/foo.md")
	if err := os.WriteFile(fullPath, []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.Chtimes(fullPath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	n := mustSync(t, ix)
	if n != 1 {
		t.Fatalf("expected 1 reindexed after edit, got %d", n)
	}

	// New content should be findable.
	hits, err = ix.Search(context.Background(), "oranges", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for 'oranges', got %d", len(hits))
	}

	// Old content should NOT be findable.
	hits, err = ix.Search(context.Background(), "apples", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits for 'apples' after edit, got %d", len(hits))
	}
}

// TestDeleteFile: remove file, sync removes from index.
func TestDeleteFile(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	content := "---\ntitle: Gone\ntype: note\n---\nThis file will be deleted."
	writeMD(t, memoryDir, "notes/gone.md", content)

	ix := mustOpen(t, home)
	mustSync(t, ix)

	// Verify it exists.
	hits, err := ix.Search(context.Background(), "deleted", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit before delete, got %d", len(hits))
	}

	// Delete file.
	fullPath := filepath.Join(memoryDir, "notes/gone.md")
	if err := os.Remove(fullPath); err != nil {
		t.Fatal(err)
	}

	mustSync(t, ix)

	// Should not be findable.
	hits, err = ix.Search(context.Background(), "deleted", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits after delete, got %d", len(hits))
	}

	// Verify manifest is also clean.
	var count int
	err = ix.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM manifest WHERE path = 'notes/gone.md'").Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("manifest still has deleted file: count=%d", count)
	}
}

// TestCorruptFrontmatter: bad file is skipped.
func TestCorruptFrontmatter(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	// Good file.
	writeMD(t, memoryDir, "notes/good.md", "---\ntitle: Good\ntype: note\n---\nGood content.")
	// Corrupt file: --- opened but never closed.
	writeMD(t, memoryDir, "notes/bad.md", "---\ntitle: Bad\ntype: note\nNo closing dashes.")

	var warnings []string
	ix := mustOpen(t, home)
	ix.Warn(func(msg string) { warnings = append(warnings, msg) })

	n := mustSync(t, ix)
	if n != 1 {
		t.Fatalf("expected 1 reindexed (good file only), got %d", n)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning for corrupt file")
	}

	// Good file should be findable.
	hits, err := ix.Search(context.Background(), "good", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for good file, got %d", len(hits))
	}
}

// TestQueryInjection: FTS5 operators are escaped.
func TestQueryInjection(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	writeMD(t, memoryDir, "notes/a.md", "---\ntitle: A\ntype: note\n---\nSome literal text here.")
	writeMD(t, memoryDir, "notes/b.md", "---\ntitle: B\ntype: note\n---\nAnother note with or keyword.")

	ix := mustOpen(t, home)
	mustSync(t, ix)

	// Injected operators should be treated as literal text.
	injections := []string{
		`" OR *`,
		`NOT`,
		`*`,
		`NEAR`,
		`" OR "`,
	}

	for _, q := range injections {
		hits, err := ix.Search(context.Background(), q, 10)
		if err != nil {
			t.Fatalf("query %q caused error: %v", q, err)
		}
		// Should not panic or return syntax error; zero matches is fine.
		_ = hits
	}
}

// TestRanking: two files, correct ranking.
func TestRanking(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	writeMD(t, memoryDir, "notes/a.md", "---\ntitle: Alpha\ntype: note\n---\nThis document is about searching and ranking results.")
	writeMD(t, memoryDir, "notes/b.md", "---\ntitle: Beta\ntype: note\n---\nSearching is great for finding things quickly.")

	ix := mustOpen(t, home)
	mustSync(t, ix)

	hits, err := ix.Search(context.Background(), "searching", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	// Both should have negative scores.
	for _, h := range hits {
		if h.Score >= 0 {
			t.Fatalf("expected negative score, got %f for %s", h.Score, h.Path)
		}
	}

	// First hit should have a lower (more negative or closer to zero depending on bm25) score.
	// bm25 in FTS5 returns negative values; ASC order means best first.
	if hits[0].Score > hits[1].Score {
		t.Fatalf("expected hits[0].score <= hits[1].score, got %f > %f", hits[0].Score, hits[1].Score)
	}
}

// TestEmptyQuery: empty query returns ErrEmptyQuery.
func TestEmptyQuery(t *testing.T) {
	home := tmpHome(t)
	ix := mustOpen(t, home)

	_, err := ix.Search(context.Background(), "", 10)
	if err != ErrEmptyQuery {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}

	_, err = ix.Search(context.Background(), "   ", 10)
	if err != ErrEmptyQuery {
		t.Fatalf("expected ErrEmptyQuery for whitespace, got %v", err)
	}
}

// TestNoMatches: query with no matches returns empty slice.
func TestNoMatches(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	writeMD(t, memoryDir, "notes/a.md", "---\ntitle: A\ntype: note\n---\nSome content.")
	ix := mustOpen(t, home)
	mustSync(t, ix)

	hits, err := ix.Search(context.Background(), "zzzznonexistent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %d", len(hits))
	}
}

// TestReopenNoResync: close + open + sync = 0 reindexed.
func TestReopenNoResync(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	writeMD(t, memoryDir, "notes/a.md", "---\ntitle: A\ntype: note\n---\nSome content.")

	ix := mustOpen(t, home)
	n := mustSync(t, ix)
	if n != 1 {
		t.Fatalf("first sync: expected 1, got %d", n)
	}

	// Close and reopen.
	ix.Close()
	ix2, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()

	n = mustSync(t, ix2)
	if n != 0 {
		t.Fatalf("re-sync after reopen: expected 0, got %d", n)
	}
}

// TestNoteFallbackTitle: note without title uses filename stem.
func TestNoteFallbackTitle(t *testing.T) {
	home := tmpHome(t)
	memoryDir := filepath.Join(home, "memory")

	// Note without title key.
	writeMD(t, memoryDir, "notes/2024-01-15.md", "---\ntype: note\n---\nDaily note content.")

	ix := mustOpen(t, home)
	mustSync(t, ix)

	hits, err := ix.Search(context.Background(), "daily", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Title != "2024-01-15" {
		t.Fatalf("expected title '2024-01-15', got %q", hits[0].Title)
	}
}

// TestEscapeQuery: verify the escapeQuery function directly.
func TestEscapeQuery(t *testing.T) {
	tests := []struct {
		in     string
		expect string
	}{
		{"hello", `"hello"`},
		{"hello world", `"hello" "world"`},
		{`say "hi"`, `"say" """hi"""`},
		{`" OR *`, `"""" "OR" "*"`},
		{"NOT", `"NOT"`},
		{"NEAR", `"NEAR"`},
		{"a  b  c", `"a" "b" "c"`},
	}

	for _, tc := range tests {
		got := escapeQuery(tc.in)
		if got != tc.expect {
			t.Errorf("escapeQuery(%q) = %q, want %q", tc.in, got, tc.expect)
		}
	}
}

// Regression: a query containing an apostrophe (SQL string-literal breaker)
// must go through as data, not syntax. Written by the architect after review;
// the original TestQueryInjection used only single-token payloads that
// survived the accidental fmt.Sprintf embedding.
func TestApostropheQueryFindsMatch(t *testing.T) {
	home := tmpHome(t)
	writeMD(t, home+"/memory", "notes/case.md",
		"---\ntitle: Case File\ntype: note\n---\nthe o'connor inheritance case")
	ix := mustOpen(t, home)
	mustSync(t, ix)

	hits, err := ix.Search(context.Background(), "o'connor", 10)
	if err != nil {
		t.Fatalf("apostrophe query returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != filepath.Join("notes", "case.md") {
		t.Fatalf("expected exactly notes/case.md, got %+v", hits)
	}
}
