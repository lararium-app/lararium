package surface

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
)

func TestPenatusSource(t *testing.T) {
	tmpDir := t.TempDir()

	// Create main session
	mainDir := filepath.Join(tmpDir, "sessions", "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mainHeader := penatus.SessionHeader{
		Penatus: 1,
		ID:      "main",
		Created: "2024-01-01T00:00:00Z",
		Kind:    "main",
	}
	data, err := json.MarshalIndent(mainHeader, "", "  ")
	if err != nil {
		t.Fatalf("marshal main header: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "session.json"), data, 0o644); err != nil {
		t.Fatalf("write main session.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "events.jsonl"), nil, 0o644); err != nil {
		t.Fatalf("write main events.jsonl: %v", err)
	}

	// Create a side session
	sideID := "s_SIDESESSIONID1234567890123"
	sideDir := filepath.Join(tmpDir, "sessions", sideID)
	if err := os.MkdirAll(sideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sideHeader := penatus.SessionHeader{
		Penatus: 1,
		ID:      sideID,
		Created: "2024-01-02T00:00:00Z",
		Kind:    "side",
		Title:   strPtr("Test Side"),
		Parent:  strPtr("main"),
	}
	data, err = json.MarshalIndent(sideHeader, "", "  ")
	if err != nil {
		t.Fatalf("marshal side header: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sideDir, "session.json"), data, 0o644); err != nil {
		t.Fatalf("write side session.json: %v", err)
	}
	eventsData := "{\"seq\":1,\"t\":\"user\",\"text\":\"hello\"}\n{\"seq\":2,\"t\":\"assistant\",\"text\":\"hi there\"}\n"
	if err := os.WriteFile(filepath.Join(sideDir, "events.jsonl"), []byte(eventsData), 0o644); err != nil {
		t.Fatalf("write side events.jsonl: %v", err)
	}

	hub := &fakeHub{inFlight: map[string]bool{sideID: true}}
	source := NewPenatusSource(tmpDir, hub)

	// Test List
	sessions, err := source.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("List returned %d sessions, want 2", len(sessions))
	}
	// Newest first
	if sessions[0].ID != sideID {
		t.Errorf("first session ID = %q, want %q", sessions[0].ID, sideID)
	}
	if sessions[1].ID != "main" {
		t.Errorf("second session ID = %q, want main", sessions[1].ID)
	}

	// Test Events
	events, inFlight, err := source.Events(sideID, 0, 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("Events returned %d events, want 2", len(events))
	}
	if !inFlight {
		t.Errorf("inFlight = false, want true")
	}

	// Test Events with after_seq
	events, _, err = source.Events(sideID, 1, 10)
	if err != nil {
		t.Fatalf("Events after_seq: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("Events after_seq=1 returned %d events, want 1", len(events))
	}

	// Test Events with limit clamp
	events, _, err = source.Events(sideID, 0, 5000)
	if err != nil {
		t.Fatalf("Events limit: %v", err)
	}
	if len(events) > 1000 {
		t.Errorf("Events limit=5000 returned %d events, want <=1000", len(events))
	}

	// Test Create
	newSession, err := source.Create("New Title", "model-x")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if newSession.ID == "" {
		t.Errorf("Create returned empty ID")
	}
	if newSession.Kind != "side" {
		t.Errorf("Create kind = %q, want side", newSession.Kind)
	}
	if newSession.Parent == nil || *newSession.Parent != "main" {
		t.Errorf("Create parent = %v, want main", newSession.Parent)
	}
	if newSession.Title == nil || *newSession.Title != "New Title" {
		t.Errorf("Create title = %v, want New Title", newSession.Title)
	}
	if newSession.ModelPin == nil || *newSession.ModelPin != "model-x" {
		t.Errorf("Create model_pin = %v, want model-x", newSession.ModelPin)
	}
}

func TestPenatusSourceInvalidID(t *testing.T) {
	tmpDir := t.TempDir()
	source := NewPenatusSource(tmpDir, &fakeHub{})

	_, _, err := source.Events("invalid", 0, 10)
	if err == nil {
		t.Errorf("Events with invalid ID should error")
	}
}
