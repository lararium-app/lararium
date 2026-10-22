package custos_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

type dedupeConnector struct{}

func (dedupeConnector) Name() string             { return "dd" }
func (dedupeConnector) Credential() string       { return "dd" }
func (dedupeConnector) Host() string             { return "dd.test" }
func (dedupeConnector) HasTool(tool string) bool { return tool == "a" || tool == "b" }
func (dedupeConnector) ValidateArgs(string, json.RawMessage) error {
	return nil
}
func (dedupeConnector) ReviewFields(tool string, _ json.RawMessage) string { return "dd:" + tool }
func (dedupeConnector) Execute(context.Context, string, json.RawMessage, custos.Credential) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

func setupDedupe(t *testing.T) *workerHarness {
	t.Helper()
	h := setupWorkerHarness(t, &fakeOAuth{})
	if err := h.ws.RegisterConnector(dedupeConnector{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := h.v.StoreOAuthGrant("", "dd", custos.Credential{Kind: "oauth2", RefreshToken: "r"}, "cli"); err != nil {
		t.Fatalf("store cred: %v", err)
	}
	return h
}

func (h *workerHarness) callAsync(t *testing.T, tool string) <-chan custos.WorkerResponse {
	t.Helper()
	ch := make(chan custos.WorkerResponse, 1)
	go func() {
		ch <- h.send(t, custos.WorkerRequest{Op: "call", Connector: "dd", Tool: tool, CellToken: v13CellToken})
	}()
	return ch
}

func (h *workerHarness) waitPending(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.hub.PendingCount("cell-1") == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending cards = %d, want %d", h.hub.PendingCount("cell-1"), n)
}

func awaitResp(t *testing.T, ch <-chan custos.WorkerResponse) custos.WorkerResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("flow did not resume within 5s")
		return custos.WorkerResponse{}
	}
}

func TestParkDedupe_ConcurrentApproveResumesBoth(t *testing.T) {
	h := setupDedupe(t)
	first := h.callAsync(t, "a")
	h.waitPending(t, 1)
	second := h.callAsync(t, "a")
	time.Sleep(300 * time.Millisecond)
	cards := h.hub.Pending()
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want exactly 1", len(cards))
	}
	if st := h.hub.Resolve("cell-1", cards[0].ID, true); st != 200 {
		t.Fatalf("resolve = %d", st)
	}
	for i, ch := range []<-chan custos.WorkerResponse{first, second} {
		if r := awaitResp(t, ch); !r.OK {
			t.Fatalf("flow %d not approved: %+v", i, r)
		}
	}
	if n := h.hub.PendingCount("cell-1"); n != 0 {
		t.Fatalf("pending after resolve = %d", n)
	}
}

func TestParkDedupe_DenyResumesBothWithDeny(t *testing.T) {
	h := setupDedupe(t)
	first := h.callAsync(t, "a")
	h.waitPending(t, 1)
	second := h.callAsync(t, "a")
	time.Sleep(300 * time.Millisecond)
	cards := h.hub.Pending()
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want exactly 1", len(cards))
	}
	if st := h.hub.Resolve("cell-1", cards[0].ID, false); st != 200 {
		t.Fatalf("resolve = %d", st)
	}
	for i, ch := range []<-chan custos.WorkerResponse{first, second} {
		if r := awaitResp(t, ch); r.OK || r.ErrorCode != "denied" {
			t.Fatalf("flow %d not denied: %+v", i, r)
		}
	}
	recs, err := h.v.Audit().ReadTailRecords(100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	denied := 0
	for _, r := range recs {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Tool == "a" && r.Reason == "denied_by_user" {
			denied++
		}
	}
	if denied != 2 {
		t.Fatalf("denied audit records = %d, want 2", denied)
	}
}

func TestParkDedupe_ExpiryResumesBothNoOrphans(t *testing.T) {
	// The harness holds cards for 5s; neither waiter may be orphaned when
	// the leader's hold timer fires, and the pending key must be released
	// so a later call mints a fresh card.
	h := setupDedupe(t)
	first := h.callAsync(t, "a")
	h.waitPending(t, 1)
	second := h.callAsync(t, "a")
	time.Sleep(300 * time.Millisecond)
	if n := h.hub.PendingCount("cell-1"); n != 1 {
		t.Fatalf("cards = %d, want exactly 1", n)
	}
	awaitLong := func(ch <-chan custos.WorkerResponse) custos.WorkerResponse {
		select {
		case r := <-ch:
			return r
		case <-time.After(9 * time.Second):
			t.Fatal("waiter orphaned: no resume after hold expiry")
			return custos.WorkerResponse{}
		}
	}
	for i, ch := range []<-chan custos.WorkerResponse{first, second} {
		if r := awaitLong(ch); r.OK {
			t.Fatalf("flow %d resumed OK after expiry, want deny: %+v", i, r)
		}
	}
	if n := h.hub.PendingCount("cell-1"); n != 0 {
		t.Fatalf("pending after expiry = %d", n)
	}
	third := h.callAsync(t, "a")
	h.waitPending(t, 1) // fresh card => pending key was released
	cards := h.hub.Pending()
	if len(cards) == 0 {
		t.Fatal("no fresh card after expiry")
	}
	h.hub.Resolve("cell-1", cards[0].ID, true)
	if r := awaitResp(t, third); !r.OK {
		t.Fatalf("post-expiry call: %+v", r)
	}
}

func TestParkDedupe_DifferentToolsNotMerged(t *testing.T) {
	h := setupDedupe(t)
	a := h.callAsync(t, "a")
	h.waitPending(t, 1)
	b := h.callAsync(t, "b")
	h.waitPending(t, 2)
	var wg sync.WaitGroup
	for _, c := range h.hub.Pending() {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			h.hub.Resolve("cell-1", id, true)
		}(c.ID)
	}
	wg.Wait()
	for _, ch := range []<-chan custos.WorkerResponse{a, b} {
		if r := awaitResp(t, ch); !r.OK {
			t.Fatalf("not approved: %+v", r)
		}
	}
}

func TestMCPTrust_RuleLifecycle(t *testing.T) {
	h := setupDedupe(t)
	p := h.v.Policy()
	decide := func() string {
		v, _ := p.Decide(custos.DecideInput{Lane: "worker", Credential: "dd", Tool: "anytool"})
		return v
	}
	if got := decide(); got != custos.VerdictAsk {
		t.Fatalf("baseline = %q", got)
	}
	if _, err := p.AddCredentialRule("dd/*", "auto", false, "cli", "cli"); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if got := decide(); got != custos.VerdictAuto {
		t.Fatalf("trusted = %q", got)
	}
	if ok, err := p.RemoveRule("credential", "dd/*", "cli"); err != nil || !ok {
		t.Fatalf("distrust: %v %v", ok, err)
	}
	if got := decide(); got != custos.VerdictAsk {
		t.Fatalf("distrusted = %q", got)
	}
}
