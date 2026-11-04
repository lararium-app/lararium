package fasti_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
)

// TestNoiseState_DedupMapRoundTripAndBounded: dedup map survives reopen and is bounded at 64 per ID (spec D8).
func TestNoiseState_DedupMapRoundTripAndBounded(t *testing.T) {
	home := t.TempDir()
	statePath := fasti.NoiseStateFilePath(home)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

	s, err := fasti.LoadNoiseState(statePath, now)
	if err != nil {
		t.Fatalf("LoadNoiseState initial: %v", err)
	}

	targetID := "j_01HNTESTTARGET00000000000"

	// 1. Record 70 hashes for targetID
	for i := 1; i <= 70; i++ {
		hash := fmt.Sprintf("hash-%03d", i)
		s.RecordEditDedup(targetID, hash)
	}

	// In memory: must be capped at 64
	if s.CheckEditDedup(targetID, "hash-001") {
		t.Errorf("hash-001 should have been pruned from bounding at 64")
	}
	if s.CheckEditDedup(targetID, "hash-006") {
		t.Errorf("hash-006 should have been pruned from bounding at 64")
	}
	if !s.CheckEditDedup(targetID, "hash-007") {
		t.Errorf("hash-007 should be retained in the last 64")
	}
	if !s.CheckEditDedup(targetID, "hash-070") {
		t.Errorf("hash-070 should be retained")
	}

	// 2. Persist to state.json
	if err := fasti.SaveNoiseState(statePath, s); err != nil {
		t.Fatalf("SaveNoiseState: %v", err)
	}

	// 3. Reopen / restart simulation
	reopened, err := fasti.LoadNoiseState(statePath, now)
	if err != nil {
		t.Fatalf("LoadNoiseState reopen: %v", err)
	}

	// Check dedup map survived
	if reopened.CheckEditDedup(targetID, "hash-001") {
		t.Errorf("hash-001 should not be present in reopened state")
	}
	if !reopened.CheckEditDedup(targetID, "hash-007") {
		t.Errorf("hash-007 should be present after reopen")
	}
	if !reopened.CheckEditDedup(targetID, "hash-070") {
		t.Errorf("hash-070 should be present after reopen")
	}

	// Unseen hash returns false
	if reopened.CheckEditDedup(targetID, "hash-unseen") {
		t.Errorf("unseen hash should return false")
	}

	// Different ID returns false
	if reopened.CheckEditDedup("other_id", "hash-070") {
		t.Errorf("hash for other_id should return false")
	}
}
