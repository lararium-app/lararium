package custos_test

// Drift audit 2026-10-19 (amendment v13): the audit-reason vocabulary
// emitted by the worker lane is closed by CUSTOS-SPEC §8.1. These tests
// pin the three gaps the audit found unobserved: the pre-hold
// policy-deny reason (now the declared `verdict`), the 429 carve-out's
// audit reason (`rate_limited`, declared by v13), and the gmail review
// fields the ask card actually shows (§6.4: `to` and `subject`).

import (
	"encoding/json"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// v13: a pre-hold policy deny must audit the frozen `verdict` reason,
// never the unbounded `policy:<pattern>` value the audit found.
func TestV13PolicyDenyAuditsVerdict(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)
	if _, err := h.v.Policy().AddCredentialRule("gmail/send", custos.VerdictDeny, false, "cli", "cli"); err != nil {
		t.Fatalf("policy add: %v", err)
	}
	resp := h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "denied" || resp.Detail != "denied: policy" {
		t.Fatalf("policy deny: want denied/'denied: policy', got %+v", resp)
	}
	recs, _ := h.v.Audit().ReadTailRecords(8)
	found := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Tool == "send" {
			if r.Reason != "verdict" {
				t.Fatalf("policy denial audited undeclared reason %q, want \"verdict\"", r.Reason)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("policy denial left no worker_call_denied audit line")
	}
}

// v13: the 429 carve-out is spec-frozen (status class rate_limited /
// "upstream rate limited") AND its audit line must carry the now-
// declared `rate_limited` reason (auditDenied records we.code).
func TestV13RateLimitedAuditsReason(t *testing.T) {
	fake := &fakeOAuth{refreshFail: 429}
	h := setupWorkerHarness(t, fake)
	if _, err := h.v.Policy().AddCredentialRule("gmail/send", "auto", false, "cli", "cli"); err != nil {
		t.Fatalf("policy add: %v", err)
	}
	s4StoreExpiringGmail(t, h, h.fakeURL+"/token-refresh")
	resp := h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "rate_limited" || resp.Detail != "upstream rate limited" {
		t.Fatalf("429: want rate_limited/'upstream rate limited', got %+v", resp)
	}
	recs, _ := h.v.Audit().ReadTailRecords(8)
	found := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Tool == "send" && r.Reason == "rate_limited" {
			found = true
		}
	}
	if !found {
		t.Fatalf("429 denial left no worker_call_denied(rate_limited) audit line")
	}
}

// v13: §6.4 review fields — gmail/send declares `to` and `subject`,
// gmail/read declares `id`; malformed args fall back to placeholders.
func TestV13GmailReviewFields(t *testing.T) {
	g := custos.NewGmailConnector(nil)
	send := g.ReviewFields("send", json.RawMessage(`{"to":["a@x.io","b@y.io"],"subject":"hi there"}`))
	if send != "to=a@x.io,b@y.io, subject=hi there" {
		t.Fatalf("send review fields = %q", send)
	}
	read := g.ReviewFields("read", json.RawMessage(`{"id":"msg-1"}`))
	if read != "id=msg-1" {
		t.Fatalf("read review fields = %q", read)
	}
	if got := g.ReviewFields("send", json.RawMessage(`{`)); got != "to=?, subject=?" {
		t.Fatalf("malformed send args review fields = %q", got)
	}
	if got := g.ReviewFields("nope", json.RawMessage(`{}`)); got != "?" {
		t.Fatalf("unknown tool review fields = %q", got)
	}
}
