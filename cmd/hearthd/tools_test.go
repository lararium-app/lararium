package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
)

func TestSaveMemoryRoundTrip(t *testing.T) {
	home := t.TempDir()

	doc := penatus.Doc{
		Meta: map[string]string{
			"name":        "MEMORY",
			"description": "long-term memory",
			"metadata":    `{ "node_type": "memory" }`,
		},
		Body: "- [high] 2026-10-04 | User prefers concise answers",
	}

	if err := saveMemory(home, doc); err != nil {
		t.Fatalf("saveMemory: %v", err)
	}

	loaded, err := loadMemory(home)
	if err != nil {
		t.Fatalf("loadMemory: %v", err)
	}

	if loaded.Body != doc.Body {
		t.Errorf("loaded body = %q, want %q", loaded.Body, doc.Body)
	}
	if loaded.Meta["name"] != doc.Meta["name"] {
		t.Errorf("loaded meta name = %q, want %q", loaded.Meta["name"], doc.Meta["name"])
	}

	// Assert file mode 0600
	info, err := os.Stat(memoryPath(home))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}

	// Assert no *.tmp left behind
	matches, err := filepath.Glob(filepath.Join(home, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("found leftover tmp files: %v", matches)
	}
}

func TestAppendAndRemoveMemoryLines(t *testing.T) {
	home := t.TempDir()

	if err := appendMemoryLine(home, "User prefers dark mode", "high"); err != nil {
		t.Fatalf("appendMemoryLine: %v", err)
	}

	loaded, err := loadMemory(home)
	if err != nil {
		t.Fatalf("loadMemory: %v", err)
	}
	lines := penatus.ParseMemoryBody(loaded.Body)
	if len(lines) != 1 || lines[0].Text != "User prefers dark mode" {
		t.Fatalf("unexpected lines: %+v", lines)
	}

	n, err := removeMemoryLines(home, "dark mode")
	if err != nil {
		t.Fatalf("removeMemoryLines: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed %d lines, want 1", n)
	}

	loadedAfter, err := loadMemory(home)
	if err != nil {
		t.Fatalf("loadMemory after remove: %v", err)
	}
	linesAfter := penatus.ParseMemoryBody(loadedAfter.Body)
	if len(linesAfter) != 0 {
		t.Fatalf("expected 0 lines after removal, got %d", len(linesAfter))
	}

	matches, err := filepath.Glob(filepath.Join(home, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("found leftover tmp files: %v", matches)
	}
}
