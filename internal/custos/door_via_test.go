package custos

import "testing"

// CUSTOS-SPEC §6.4b amendment v14 widens the verdict channel enum to
// {web, telegram, device}. The enum stays CLOSED: v9's reasoning that a
// silent default is a spoof path for attribution is unchanged, so an
// unknown or empty value must still be refused rather than defaulted.
func TestValidDoorVia(t *testing.T) {
	tests := []struct {
		via  string
		want bool
	}{
		{"web", true},
		{"telegram", true},
		{"device", true},
		{"", false},
		{"cli", false},
		{"ctl", false},
		{"Device", false},
		{"device ", false},
		{"hub", false},
		{"listener_closed", false},
	}
	for _, tc := range tests {
		if got := validDoorVia(tc.via); got != tc.want {
			t.Errorf("validDoorVia(%q) = %v, want %v", tc.via, got, tc.want)
		}
	}
}
