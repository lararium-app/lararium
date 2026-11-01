package devices

import (
	"strings"
	"testing"
	"time"
)

func TestGraceWindowTransitions(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Use a 25ms grace period for fast, sleep-free testing
	mgr.SetGracePeriod(25 * time.Millisecond)

	devID := "phone-grace"

	// 1. Initial state: offline
	if mgr.IsOnline(devID) {
		t.Fatalf("initially expected offline")
	}

	// 2. Connect: becomes immediately online
	mgr.SetOnline(devID, true)
	if !mgr.IsOnline(devID) {
		t.Fatalf("expected online immediately after SetOnline(true)")
	}

	// 3. Drop signal: enters 30s grace window (25ms in test)
	mgr.SetOnline(devID, false)
	// During grace window, device MUST still be considered online!
	if !mgr.IsOnline(devID) {
		t.Fatalf("expected device to remain online during grace window")
	}

	// Wait for grace timer to fire (35ms < 100ms)
	time.Sleep(35 * time.Millisecond)
	if mgr.IsOnline(devID) {
		t.Fatalf("expected device to transition to offline after grace window expired")
	}

	// 4. Reconnect: becomes online again
	mgr.SetOnline(devID, true)
	if !mgr.IsOnline(devID) {
		t.Fatalf("expected online on reconnect")
	}

	// 5. Drop signal, but reconnect before grace window expires
	mgr.SetOnline(devID, false)
	if !mgr.IsOnline(devID) {
		t.Fatalf("expected online during second grace window")
	}

	// Reconnect after 5ms (well before 25ms expiration)
	time.Sleep(5 * time.Millisecond)
	mgr.SetOnline(devID, true)

	// Wait 35ms — grace timer was cancelled, so device must STAY online!
	time.Sleep(35 * time.Millisecond)
	if !mgr.IsOnline(devID) {
		t.Fatalf("expected device to remain online because reconnect cancelled the timer")
	}
}

func TestStalenessMarkerExactFormat(t *testing.T) {
	fixedTime := time.Date(2026, 11, 1, 14, 30, 0, 0, time.UTC)
	lag := 45 * time.Minute

	marker := StalenessMarker(fixedTime, lag)
	expected := "[stale: phone offline since 2026-11-01T14:30:00Z, lag 45m0s]"
	if marker != expected {
		t.Fatalf("StalenessMarker = %q, want %q", marker, expected)
	}

	// Test with 0 lag
	markerZero := StalenessMarker(fixedTime, 0)
	expectedZero := "[stale: phone offline since 2026-11-01T14:30:00Z, lag 0s]"
	if markerZero != expectedZero {
		t.Fatalf("StalenessMarker zero lag = %q, want %q", markerZero, expectedZero)
	}

	// Test with hours and seconds
	markerComplex := StalenessMarker(fixedTime, 1*time.Hour+15*time.Minute+30*time.Second)
	expectedComplex := "[stale: phone offline since 2026-11-01T14:30:00Z, lag 1h15m30s]"
	if markerComplex != expectedComplex {
		t.Fatalf("StalenessMarker complex lag = %q, want %q", markerComplex, expectedComplex)
	}

	// Verify exact prefix pattern
	if !strings.HasPrefix(expected, "[stale: phone offline since ") {
		t.Fatalf("missing exact prefix")
	}
}
