package surface

import (
	"testing"
	"time"
)

// A Hub built from a raw (un-normalized) config must still carry the
// spec defaults: a zero TurnTimeout armed an already-expired context on
// every API turn, so every message died instantly.
func TestNewHubNormalizesZeroTimeouts(t *testing.T) {
	h := NewHub(ServeConfig{}, t.TempDir(), 512, 80, nil, nil, nil)
	if h.cfg.TurnTimeout != 10*time.Minute {
		t.Errorf("TurnTimeout = %v, want 10m", h.cfg.TurnTimeout)
	}
	if h.cfg.ApprovalTimeout != 5*time.Minute {
		t.Errorf("ApprovalTimeout = %v, want 5m", h.cfg.ApprovalTimeout)
	}
	if h.cfg.Listen != "127.0.0.1:7717" {
		t.Errorf("Listen = %q, want 127.0.0.1:7717", h.cfg.Listen)
	}
}
