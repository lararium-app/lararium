package web

// #320 review round 2: the web-side denial mirror must tell the truth
// about cancellations, and the hub-backed denial audit must thread
// through context.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCancelledGuidanceDoesNotBlameUser(t *testing.T) {
	g := denialGuidance("cancelled")
	if !strings.Contains(g, "was cancelled before a user verdict was recorded") {
		t.Fatalf("cancelled guidance = %q", g)
	}
	if strings.Contains(g, "the user denied") {
		t.Fatalf("cancelled guidance blames the user: %q", g)
	}
	// The denied-verdict guidance keeps blaming the verdict (correctly).
	if d := denialGuidance("denied"); !strings.Contains(d, "the user denied") {
		t.Fatalf("denied guidance = %q", d)
	}
}

func TestCustosLockedGuidanceParity(t *testing.T) {
	g := denialGuidance("custos_locked")
	if !strings.Contains(g, "custody is locked or the credential was revoked") {
		t.Fatalf("custos_locked guidance = %q", g)
	}
	if strings.Contains(g, "the user denied") {
		t.Fatalf("custos_locked guidance blames the user: %q", g)
	}
}

func TestDenialAuditContextRoundTrip(t *testing.T) {
	if DenialAuditFromContext(context.Background()) != nil {
		t.Fatal("expected no DenialAudit on a bare context")
	}
	var fn DenialAudit = func(string) (json.RawMessage, bool) {
		return json.RawMessage(`{"decision":"denied"}`), true
	}
	ctx := ContextWithDenialAudit(context.Background(), fn)
	if DenialAuditFromContext(ctx) == nil {
		t.Fatal("DenialAudit lost through context")
	}
}
