package custos_test

import (
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// TestL16_LoopbackAgreement verifies that both call sites (surface loopback helper
// and custos proxy floor logic) agree on the unified L-16 classifier law:
// parses IP via netip, checks IsLoopback (covering 127/8 and ::1), and strictly rejects zone IDs.
func TestL16_LoopbackAgreement(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"127.0.0.53", true},
		{"0177.0.0.1", false},
		{"[::1]", true},
		{"::1%eth0", false},
		{"8.8.8.8", false},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			surfaceGot := surface.IsLoopbackLiteral(tc.input)
			if surfaceGot != tc.want {
				t.Errorf("surface.IsLoopbackLiteral(%q) = %v, want %v", tc.input, surfaceGot, tc.want)
			}

			custosGot := custos.IsFloorHost(tc.input)
			if custosGot != tc.want {
				t.Errorf("custos.IsFloorHost(%q) = %v, want %v", tc.input, custosGot, tc.want)
			}

			if surfaceGot != custosGot {
				t.Errorf("call sites disagree on %q: surface=%v, custos=%v", tc.input, surfaceGot, custosGot)
			}
		})
	}
}
