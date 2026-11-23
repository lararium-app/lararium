package loop

import (
	"encoding/json"
	"testing"
)

// A hub-initiated close is not a user verdict: the model-facing gate must
// not blame the user for it (#320 review round 3).
func TestDenialGateHubCloseReason(t *testing.T) {
	audit := func(string) (json.RawMessage, bool) {
		return json.RawMessage(`{"decision":"denied","reason":"shutdown","source":"shutdown"}`), true
	}
	if got := denialGate("denied:a_1", audit); got != gateCancelled {
		t.Fatalf("denialGate = %q, want %q", got, gateCancelled)
	}
}
