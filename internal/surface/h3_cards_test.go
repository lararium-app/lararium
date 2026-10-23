package surface

import (
	"sync"
	"testing"
	"time"
)

// H3 (audit 2026-10): custody approval cards must carry the destination
// as its own addressable field AND deliver the caller-built review text
// (recipient, subject, params) to every surface — previously the review
// string was dropped and only the destination reached the fan-out, so a
// human approved "api.example.com" blind.

type captureFan struct {
	mu   sync.Mutex
	pend [][4]string // id, sessionID, name, argsSummary
	term [][5]string
}

func (f *captureFan) ApprovalPending(id, sessionID, name, argsSummary string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pend = append(f.pend, [4]string{id, sessionID, name, argsSummary})
}

func (f *captureFan) ApprovalTerminal(id, sessionID, state, reason, source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.term = append(f.term, [5]string{id, sessionID, state, reason, source})
}

func TestH3_CustosCard_DestSeparableReviewComposed(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	fan := &captureFan{}
	hub.SetFanout(fan)

	id, _ := hub.RegisterCustos("c_w", "smtp", "smtp.corp.example:465",
		"to: alice@partner.io subject: Q4 numbers", 30*time.Second, nil)

	// Fan-out (door CARD frame source) carries dest + review composed.
	fan.mu.Lock()
	if len(fan.pend) != 1 {
		fan.mu.Unlock()
		t.Fatalf("fan pending = %d, want 1", len(fan.pend))
	}
	got := fan.pend[0]
	fan.mu.Unlock()
	if got[0] != id || got[1] != "c_w" || got[2] != "smtp" {
		t.Errorf("fan tuple = %v, want id=%s c_w smtp", got, id)
	}
	wantReview := "smtp.corp.example:465 | to: alice@partner.io subject: Q4 numbers"
	if got[3] != wantReview {
		t.Errorf("fan review = %q, want %q", got[3], wantReview)
	}

	// Dest stays separately addressable for IP-literal authority checks.
	var found bool
	for _, c := range hub.Pending() {
		if c.ID == id {
			found = true
			if c.Dest != "smtp.corp.example:465" {
				t.Errorf("card.Dest = %q, want smtp.corp.example:465", c.Dest)
			}
			if c.Cred != "smtp" || c.Cell != "c_w" {
				t.Errorf("card identity = %v, want cred smtp cell c_w", c)
			}
		}
	}
	if !found {
		t.Fatal("custody card missing from hub Pending()")
	}
}

func TestH3_CustosCard_DestOnlyAndReviewOnly(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	fan := &captureFan{}
	hub.SetFanout(fan)

	// No review text: dest alone (pre-H3 behavior preserved).
	_, _ = hub.RegisterCustos("c_a", "svc", "api.example.com:443", "", 30*time.Second, nil)
	// No dest (worker-lane custody card): review alone, Dest empty.
	_, _ = hub.RegisterCustos("c_b", "gmail", "", "to: x@y.z", 30*time.Second, nil)

	fan.mu.Lock()
	defer fan.mu.Unlock()
	if len(fan.pend) != 2 {
		t.Fatalf("fan pending = %d, want 2", len(fan.pend))
	}
	if fan.pend[0][3] != "api.example.com:443" {
		t.Errorf("dest-only review = %q, want dest verbatim", fan.pend[0][3])
	}
	if fan.pend[1][3] != "to: x@y.z" {
		t.Errorf("review-only fan text = %q, want review verbatim", fan.pend[1][3])
	}

	var destA, destB string
	for _, c := range hub.Pending() {
		switch c.Cell {
		case "c_a":
			destA = c.Dest
		case "c_b":
			destB = c.Dest
		}
	}
	if destA != "api.example.com:443" || destB != "" {
		t.Errorf("dests = %q / %q, want api.example.com:443 / empty", destA, destB)
	}
}
