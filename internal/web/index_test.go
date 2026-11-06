package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalletIndex_MarkStaleAndStampReRaise_JSONRoundTrip(t *testing.T) {
	home := t.TempDir()
	idx := NewWalletIndex(home)
	origin := "https://example.com"

	// 1. Initial touch should omit stale and re_raise_date in JSON
	if err := idx.Touch(origin, "initial goal"); err != nil {
		t.Fatalf("Touch failed: %v", err)
	}

	indexPath := filepath.Join(home, "sessions", "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "stale") {
		t.Errorf("expected 'stale' to be omitted initially, got: %s", content)
	}
	if strings.Contains(content, "re_raise_date") {
		t.Errorf("expected 're_raise_date' to be omitted initially, got: %s", content)
	}

	entry, ok := idx.Get(origin)
	if !ok {
		t.Fatalf("expected origin entry in index")
	}
	if entry.Stale {
		t.Errorf("expected Stale=false initially, got true")
	}
	if entry.ReRaiseDate != "" {
		t.Errorf("expected empty ReRaiseDate initially, got: %q", entry.ReRaiseDate)
	}

	// 2. MarkStale should set stale=true and serialize "stale": true while omitting re_raise_date
	if err := idx.MarkStale(origin); err != nil {
		t.Fatalf("MarkStale failed: %v", err)
	}

	entry, ok = idx.Get(origin)
	if !ok || !entry.Stale {
		t.Fatalf("expected Stale=true after MarkStale")
	}

	data, err = os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	content = string(data)
	if !strings.Contains(content, `"stale": true`) && !strings.Contains(content, `"stale":true`) {
		t.Errorf("expected 'stale: true' in serialized JSON, got: %s", content)
	}
	if strings.Contains(content, "re_raise_date") {
		t.Errorf("expected 're_raise_date' to remain omitted, got: %s", content)
	}

	// 3. StampReRaise should set re_raise_date and serialize it to JSON
	testDate := "2026-11-05"
	if err := idx.StampReRaise(origin, testDate); err != nil {
		t.Fatalf("StampReRaise failed: %v", err)
	}

	entry, ok = idx.Get(origin)
	if !ok || entry.ReRaiseDate != testDate {
		t.Fatalf("expected ReRaiseDate=%q, got: %q", testDate, entry.ReRaiseDate)
	}

	data, err = os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	content = string(data)
	if !strings.Contains(content, `"re_raise_date": "2026-11-05"`) && !strings.Contains(content, `"re_raise_date":"2026-11-05"`) {
		t.Errorf("expected 're_raise_date' in serialized JSON, got: %s", content)
	}

	// 4. Round-trip through fresh WalletIndex
	idx2 := NewWalletIndex(home)
	entry2, ok := idx2.Get(origin)
	if !ok {
		t.Fatalf("expected entry in fresh index instance")
	}
	if !entry2.Stale {
		t.Errorf("round-trip Stale=false, want true")
	}
	if entry2.ReRaiseDate != testDate {
		t.Errorf("round-trip ReRaiseDate=%q, want %q", entry2.ReRaiseDate, testDate)
	}
}

func TestWalletIndex_MarkStale_NewOrigin(t *testing.T) {
	home := t.TempDir()
	idx := NewWalletIndex(home)
	origin := "api.example.com"

	if err := idx.MarkStale(origin); err != nil {
		t.Fatalf("MarkStale failed: %v", err)
	}

	entry, ok := idx.Get(origin)
	if !ok {
		t.Fatalf("expected new entry created by MarkStale")
	}
	if !entry.Stale {
		t.Errorf("expected Stale=true")
	}
	if entry.Policy != PolicyAlwaysAsk {
		t.Errorf("expected default policy %s, got %s", PolicyAlwaysAsk, entry.Policy)
	}
}

func TestWalletIndex_StampReRaise_NewOrigin(t *testing.T) {
	home := t.TempDir()
	idx := NewWalletIndex(home)
	origin := "auth.example.com"
	date := "2026-11-05"

	if err := idx.StampReRaise(origin, date); err != nil {
		t.Fatalf("StampReRaise failed: %v", err)
	}

	entry, ok := idx.Get(origin)
	if !ok {
		t.Fatalf("expected new entry created by StampReRaise")
	}
	if entry.ReRaiseDate != date {
		t.Errorf("expected ReRaiseDate=%q, got %q", date, entry.ReRaiseDate)
	}
}

func TestWalletIndex_MarkStale_InvalidOrigin(t *testing.T) {
	home := t.TempDir()
	idx := NewWalletIndex(home)

	if err := idx.MarkStale(""); err == nil {
		t.Errorf("expected error for empty origin")
	}
	if err := idx.StampReRaise("", "2026-11-05"); err == nil {
		t.Errorf("expected error for empty origin")
	}
}
