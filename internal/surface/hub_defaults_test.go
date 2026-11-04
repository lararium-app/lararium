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

func TestHubSecretNeedlesRoundTrip(t *testing.T) {
	var nilHub *Hub
	if nilHub.SecretNeedles() != nil {
		t.Fatal("nil Hub SecretNeedles() must be nil")
	}

	h := NewHub(ServeConfig{}, t.TempDir(), 512, 80, nil, nil, nil)
	fn := h.SecretNeedles()
	if fn == nil {
		t.Fatal("SecretNeedles() returned nil closure on non-nil Hub")
	}
	if got := fn(); got != nil {
		t.Fatalf("initial SecretNeedles()() = %v, want nil", got)
	}

	// SetSecrets after obtaining the closure must resolve lazily on next call.
	h.SetSecrets(func() []string {
		return []string{"secret-alpha-1234", "secret-beta-5678"}
	})
	got := fn()
	if len(got) != 2 || got[0] != "secret-alpha-1234" || got[1] != "secret-beta-5678" {
		t.Fatalf("after SetSecrets, SecretNeedles()() = %v, want [secret-alpha-1234 secret-beta-5678]", got)
	}

	// SetSecrets(nil) disables again.
	h.SetSecrets(nil)
	if got := fn(); got != nil {
		t.Fatalf("after SetSecrets(nil), SecretNeedles()() = %v, want nil", got)
	}
}
