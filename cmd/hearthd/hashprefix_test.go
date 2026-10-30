package main

import "testing"

// Revoke target shape: every-hex target (any case, any length > 0) is
// treated as a stored-hash prefix — the store then validates length and
// uniqueness. Anything else goes down the label path, which keeps the
// V5b spec contract `token revoke <label>` working for real labels.
func TestIsHexTarget(t *testing.T) {
	good := []string{"a3394c10", "abaf6cdc00ff11ee", "00000000", "deadbeef", "A3394C10", "1234567", "cafe"}
	bad := []string{"", "revoke-me", "a3394c1g", "desk", "a3394c10 ", "0xdead", "phone"}
	for _, s := range good {
		if !isHexTarget(s) {
			t.Errorf("isHexTarget(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isHexTarget(s) {
			t.Errorf("isHexTarget(%q) = true, want false", s)
		}
	}
}
