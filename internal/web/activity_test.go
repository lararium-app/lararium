package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestActivity_RotateKeepsOne(t *testing.T) {
	home := t.TempDir()
	logger := NewActivityLogger(home)
	// Lower threshold to 300 bytes for rotation test
	logger.SetRotateBytes(300)

	// Write enough records to trigger rotation multiple times
	for i := range 20 {
		rec := ActivityRecord{
			TS:      time.Now().UTC().Format(time.RFC3339Nano),
			Origin:  "example.com",
			URL:     fmt.Sprintf("https://example.com/item/%d", i),
			Kind:    KindAct,
			Verdict: VerdictOK,
			Intent:  fmt.Sprintf("click button %d", i),
		}
		if err := logger.Append(rec); err != nil {
			t.Fatalf("append failed on iteration %d: %v", i, err)
		}
	}

	primaryPath := filepath.Join(home, "sessions", "activity.jsonl")
	gen1Path := filepath.Join(home, "sessions", "activity.jsonl.1")
	gen2Path := filepath.Join(home, "sessions", "activity.jsonl.2")

	if _, err := os.Stat(primaryPath); err != nil {
		t.Fatalf("expected primary activity file to exist: %v", err)
	}
	if _, err := os.Stat(gen1Path); err != nil {
		t.Fatalf("expected gen 1 activity file to exist: %v", err)
	}
	if _, err := os.Stat(gen2Path); !os.IsNotExist(err) {
		t.Fatalf("expected gen 2 activity file NOT to exist, got err: %v", err)
	}
}

func TestActivity_QueryNewestFirst(t *testing.T) {
	home := t.TempDir()
	logger := NewActivityLogger(home)
	logger.SetRotateBytes(400) // allow rotation to test across gen boundary

	baseTime := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for i := range 6 {
		rec := ActivityRecord{
			TS:      baseTime.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			Origin:  "ups.com",
			URL:     fmt.Sprintf("https://ups.com/track/%d", i),
			Kind:    KindRead,
			Verdict: VerdictOK,
			Intent:  fmt.Sprintf("intent-%d", i),
		}
		if err := logger.Append(rec); err != nil {
			t.Fatalf("append failed: %v", err)
		}
	}

	// Query capped at 3
	res, err := logger.Query(3, "")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("expected 3 records, got %d", len(res))
	}

	// Verify newest first: intent-5, intent-4, intent-3
	if res[0].Intent != "intent-5" || res[1].Intent != "intent-4" || res[2].Intent != "intent-3" {
		t.Fatalf("expected newest-first [intent-5, intent-4, intent-3], got: [%s, %s, %s]",
			res[0].Intent, res[1].Intent, res[2].Intent)
	}

	// Query with origin filter
	upsRes, err := logger.Query(10, "ups.com")
	if err != nil {
		t.Fatalf("query with filter failed: %v", err)
	}
	if len(upsRes) != 6 {
		t.Fatalf("expected 6 ups.com records, got %d", len(upsRes))
	}

	otherRes, err := logger.Query(10, "fedex.com")
	if err != nil {
		t.Fatalf("query other filter failed: %v", err)
	}
	if len(otherRes) != 0 {
		t.Fatalf("expected 0 fedex records, got %d", len(otherRes))
	}
}

func TestActivity_NoPageTextInRecords(t *testing.T) {
	// Verify struct schema has no DOM text, page content, or cookie fields
	rt := reflect.TypeOf(ActivityRecord{})
	numFields := rt.NumField()
	for i := range numFields {
		name := rt.Field(i).Name
		jsonTag := rt.Field(i).Tag.Get("json")
		if name == "PageText" || name == "DOM" || name == "Body" || name == "Content" || name == "Cookies" {
			t.Fatalf("forbidden field %s in ActivityRecord", name)
		}
		if jsonTag == "text" || jsonTag == "body" || jsonTag == "content" || jsonTag == "cookies" {
			t.Fatalf("forbidden json tag %s in ActivityRecord", jsonTag)
		}
	}

	// Verify serialized output contains only permitted keys
	rec := ActivityRecord{
		TS:            "2026-11-05T01:30:00Z",
		Origin:        "example.com",
		URL:           "https://example.com/checkout",
		Kind:          KindAct,
		Verdict:       VerdictOK,
		Intent:        "submit order",
		ScreenshotRef: "web/shots/1.png",
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	allowedKeys := map[string]bool{
		"ts":             true,
		"origin":         true,
		"url":            true,
		"kind":           true,
		"verdict":        true,
		"intent":         true,
		"screenshot_ref": true,
	}

	for k := range m {
		if !allowedKeys[k] {
			t.Errorf("unexpected key %q found in activity record payload", k)
		}
	}
}
