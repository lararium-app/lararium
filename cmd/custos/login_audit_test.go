package main

import (
	"errors"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestEmitLoginDenied asserts the CUSTOS-SPEC V13 wiring doctrine:
// OAuth login refusals (forged state, missing state, consent denied, no code,
// timeout) must be refused AND audited with login_denied and the closed reason.
func TestEmitLoginDenied(t *testing.T) {
	v := custos.NewVault(t.TempDir(), time.Second)
	if err := v.Init("test-passphrase-1234"); err != nil {
		t.Fatalf("v.Init: %v", err)
	}

	tests := []struct {
		name       string
		err        error
		wantKind   string
		wantReason string
		wantAudit  bool
	}{
		{
			name:       "forged_state_mismatch",
			err:        custos.ErrLoginStateMismatch,
			wantKind:   custos.AuditKindLoginDenied,
			wantReason: "state_mismatch",
			wantAudit:  true,
		},
		{
			name:       "missing_state",
			err:        custos.ErrLoginNoState,
			wantKind:   custos.AuditKindLoginDenied,
			wantReason: "missing_state",
			wantAudit:  true,
		},
		{
			name:       "consent_denied",
			err:        custos.ErrLoginDenied,
			wantKind:   custos.AuditKindLoginDenied,
			wantReason: "consent_denied",
			wantAudit:  true,
		},
		{
			name:       "no_code",
			err:        custos.ErrLoginNoCode,
			wantKind:   custos.AuditKindLoginDenied,
			wantReason: "no_code",
			wantAudit:  true,
		},
		{
			name:       "timeout",
			err:        custos.ErrLoginTimeout,
			wantKind:   custos.AuditKindLoginDenied,
			wantReason: "timeout",
			wantAudit:  true,
		},
		{
			name:      "unrelated_error_no_audit",
			err:       errors.New("dial tcp connection refused"),
			wantAudit: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recsBefore, _ := v.Audit().ReadTailRecords(0)
			before := len(recsBefore)
			emitLoginDenied(v, tc.err)
			records, _ := v.Audit().ReadTailRecords(0)
			if !tc.wantAudit {
				if len(records) != before {
					t.Fatalf("expected no audit record added, got %d (before: %d)", len(records), before)
				}
				return
			}
			if len(records) != before+1 {
				t.Fatalf("expected 1 audit record added, got %d (before: %d)", len(records), before)
			}
			last := records[len(records)-1]
			if last.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", last.Kind, tc.wantKind)
			}
			if last.Actor != "cli" {
				t.Errorf("Actor = %q, want 'cli'", last.Actor)
			}
			if last.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", last.Reason, tc.wantReason)
			}
		})
	}

	// Safety: nil vault or nil audit must be a no-op, never a panic.
	emitLoginDenied(nil, custos.ErrLoginStateMismatch)
}
