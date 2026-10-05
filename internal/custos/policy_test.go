package custos_test

import (
	"errors"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// V6: Writer grammar gate & comparator ordering law:
// - Every illegal pattern class refused at construction:
//   - uppercase rejected (no uppercase)
//   - bare * rejected
//   - single-label suffix com/* rejected
//   - userinfo @ rejected
//   - mid-path * rejected
//   - port :0 rejected
//   - out-of-range port rejected
//   - non-canonical IP spellings rejected (leading zeros, mixed ::ffff)
//   - IP-literal with auto verdict rejected (ip bindings are ask-only)
//   - Always rule with suffix /* rejected
//   - Always rule with floor IP rejected (unsatisfiable binding)
//
// - Written rules re-validate, file re-reads strict
// - Comparator ordering cases from §6.1 / §10 V24:
//   - suffix dominance (cdn.tracker.com/* auto vs tracker.com/* deny)
//   - credential lane qualification (gmail/send deny vs gmail ask)
//   - apex Always over wildcard (example.com auto vs example.com/* deny)
//   - domination warning note: overridden by <p2> (more-specific match wins)
//
// All per CUSTOS-SPEC §6.1, §6.2, §6.3, §6.5, §10 V6, V24.
func TestV6_WriterGrammarGate(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}

	pe := v.Policy()

	// 1. Illegal pattern classes refused at construction:
	illegalEgress := []struct {
		pattern string
		verdict string
		always  bool
		wantErr error
	}{
		// Uppercase
		{"API.EXAMPLE.COM", custos.VerdictAuto, false, custos.ErrInvalidPattern},
		{"Example.com/*", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		// Bare *
		{"*", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		// Single-label suffix
		{"com/*", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"org/*", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		// Userinfo @
		{"user@example.com", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"admin@api.example.com:8080", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		// Mid-path *
		{"api*.example.com", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"example.com/*/v1", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"*.example.com", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		// Port :0
		{"example.com:0", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"192.0.2.1:0", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		// Out of range port
		{"example.com:65536", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"example.com:99999", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		{"example.com:-1", custos.VerdictDeny, false, custos.ErrInvalidPattern},
		// Non-canonical IPv4 spellings (leading zeros)
		{"203.0.113.007", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		{"010.0.0.1", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		// Non-canonical IPv6 spellings (uppercase, mixed-form, uncompressed)
		{"2001:DB8::1", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		{"::ffff:1.2.3.4", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		{"2001:0db8::0001", custos.VerdictAsk, false, custos.ErrInvalidPattern},
		// IP-literal with auto verdict (ip bindings are ask-only per §4.3, §6.5)
		{"203.0.113.7", custos.VerdictAuto, false, custos.ErrIPBindingsAskOnly},
		{"[2001:db8::1]:8080", custos.VerdictAuto, false, custos.ErrIPBindingsAskOnly},
		{"203.0.113.7", custos.VerdictAuto, true, custos.ErrIPBindingsAskOnly},
		// Suffix on Always rule
		{"example.com/*", custos.VerdictAuto, true, custos.ErrInvalidPattern},
		// Floor IP on Always rule (unsatisfiable binding per §6.5)
		{"127.0.0.1", custos.VerdictAsk, true, custos.ErrUnsatisfiableBinding},
		{"169.254.169.254", custos.VerdictAsk, true, custos.ErrUnsatisfiableBinding},
		{"100.64.0.1", custos.VerdictAsk, true, custos.ErrUnsatisfiableBinding},
	}

	for _, tc := range illegalEgress {
		_, err := pe.AddEgressRule(tc.pattern, tc.verdict, tc.always, "cli", "cli")
		if err == nil {
			t.Errorf("AddEgressRule(%q, %s, always=%t) expected error %v, got nil", tc.pattern, tc.verdict, tc.always, tc.wantErr)
			continue
		}
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("AddEgressRule(%q) error = %v, want %v", tc.pattern, err, tc.wantErr)
		}
	}

	// Illegal credential patterns:
	illegalCred := []struct {
		pattern string
		always  bool
	}{
		{"OPENAI", false},
		{"gmail/SEND", false},
		{"gmail/*", true}, // suffix on Always
		{"invalid/tool/deep", false},
		{"", false},
		{"with spaces", false},
	}
	for _, tc := range illegalCred {
		_, err := pe.AddCredentialRule(tc.pattern, custos.VerdictAsk, tc.always, "cli", "cli")
		if err == nil {
			t.Errorf("AddCredentialRule(%q) expected error, got nil", tc.pattern)
		}
	}

	// 2. Written rules re-validate and policy.json re-reads strictly:
	legalRules := []struct {
		lane    string
		pattern string
		verdict string
	}{
		{"egress", "api.example.com", custos.VerdictAuto},
		{"egress", "api.example.com:8080", custos.VerdictDeny},
		{"egress", "example.org/*", custos.VerdictAsk},
		{"egress", "203.0.113.7", custos.VerdictAsk},
		{"egress", "2001:db8::1", custos.VerdictDeny},
		{"credential", "openai", custos.VerdictAsk},
		{"credential", "gmail/send", custos.VerdictAuto},
		{"credential", "gmail/*", custos.VerdictAsk},
	}

	for _, r := range legalRules {
		var err error
		if r.lane == "egress" {
			_, err = pe.AddEgressRule(r.pattern, r.verdict, false, "cli", "cli")
		} else {
			_, err = pe.AddCredentialRule(r.pattern, r.verdict, false, "cli", "cli")
		}
		if err != nil {
			t.Fatalf("add legal rule %s %s: %v", r.lane, r.pattern, err)
		}
	}

	// Re-read file strictly
	if err := pe.LoadStrict(); err != nil {
		t.Fatalf("LoadStrict re-reading written rules failed: %v", err)
	}

	// 3. Comparator ordering cases from §6.1 / §10 V24:
	// Case A: Suffix dominance warning
	// Existing: cdn.tracker.com/* auto
	// Adding: tracker.com/* deny
	// New pattern tracker.com/* is strictly dominated by existing cdn.tracker.com/*
	// Stored with full frozen note: note: overridden by cdn.tracker.com/* (more-specific match wins)
	vA, _, passA := setupTestVault(t)
	_ = vA.Init(passA)
	_ = vA.Unlock(passA, false)

	_, err := vA.Policy().AddEgressRule("cdn.tracker.com/*", custos.VerdictAuto, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}

	notes, err := vA.Policy().AddEgressRule("tracker.com/*", custos.VerdictDeny, false, "cli", "cli")
	if err != nil {
		t.Fatalf("AddEgressRule tracker.com/* should succeed, got %v", err)
	}
	expectedNote := "note: overridden by cdn.tracker.com/* (more-specific match wins)"
	foundNote := false
	for _, n := range notes {
		if n == expectedNote {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("notes = %v, want %q", notes, expectedNote)
	}

	// Request to cdn.tracker.com resolves auto (tier-2 longer match wins)
	verdict, matched := vA.Policy().Decide(custos.DecideInput{
		Host: "cdn.tracker.com",
		Port: 80,
	})
	if verdict != custos.VerdictAuto || matched != "cdn.tracker.com/*" {
		t.Errorf("cdn.tracker.com decided %s (%s), want auto (cdn.tracker.com/*)", verdict, matched)
	}

	// Request to www.tracker.com resolves deny
	verdict, matched = vA.Policy().Decide(custos.DecideInput{
		Host: "www.tracker.com",
		Port: 80,
	})
	if verdict != custos.VerdictDeny || matched != "tracker.com/*" {
		t.Errorf("www.tracker.com decided %s (%s), want deny (tracker.com/*)", verdict, matched)
	}

	// Same-verdict overlap stores silently
	silentNotes, err := vA.Policy().AddEgressRule("sub.cdn.tracker.com/*", custos.VerdictAuto, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(silentNotes) > 0 {
		t.Errorf("same-verdict overlap should store silently, got notes: %v", silentNotes)
	}

	// Case B: Credential lane qualification
	// With gmail/send deny stored, adding gmail ask warns:
	// note: overridden by gmail/send (more-specific match wins)
	vB, _, passB := setupTestVault(t)
	_ = vB.Init(passB)
	_ = vB.Unlock(passB, false)

	_, err = vB.Policy().AddCredentialRule("gmail/send", custos.VerdictDeny, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}
	notes, err = vB.Policy().AddCredentialRule("gmail", custos.VerdictAsk, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}
	expectedCredNote := "note: overridden by gmail/send (more-specific match wins)"
	foundCredNote := false
	for _, n := range notes {
		if n == expectedCredNote {
			foundCredNote = true
		}
	}
	if !foundCredNote {
		t.Errorf("cred notes = %v, want %q", notes, expectedCredNote)
	}

	// Worker call gmail/send resolves deny (tool-qualified exact outranks bare exact)
	verdict, matched = vB.Policy().Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "gmail",
		Tool:       "send",
	})
	if verdict != custos.VerdictDeny || matched != "gmail/send" {
		t.Errorf("worker gmail/send decided %s (%s), want deny (gmail/send)", verdict, matched)
	}

	// Worker call gmail/list resolves ask (bare gmail matches)
	verdict, matched = vB.Policy().Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "gmail",
		Tool:       "list",
	})
	if verdict != custos.VerdictAsk || matched != "gmail" {
		t.Errorf("worker gmail/list decided %s (%s), want ask (gmail)", verdict, matched)
	}

	// Bearer swap for gmail resolves ask
	verdict, matched = vB.Policy().Decide(custos.DecideInput{
		Lane:       "bearer",
		Credential: "gmail",
	})
	if verdict != custos.VerdictAsk || matched != "gmail" {
		t.Errorf("bearer gmail decided %s (%s), want ask (gmail)", verdict, matched)
	}

	// Case C: Tier 1 Apex Always over Apex Wildcard
	// With deny example.com/* stored, an Always on apex example.com stores example.com auto
	vC, _, passC := setupTestVault(t)
	_ = vC.Init(passC)
	_ = vC.Unlock(passC, false)

	_, err = vC.Policy().AddEgressRule("example.com/*", custos.VerdictDeny, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}
	_, err = vC.Policy().AddEgressRule("example.com", custos.VerdictAuto, true, "approval", "cli")
	if err != nil {
		t.Fatal(err)
	}

	// Apex request resolves auto (Tier 1 exact outranks Tier 2 suffix)
	verdict, matched = vC.Policy().Decide(custos.DecideInput{
		Host: "example.com",
		Port: 80,
	})
	if verdict != custos.VerdictAuto || matched != "example.com" {
		t.Errorf("apex example.com decided %s (%s), want auto (example.com)", verdict, matched)
	}

	// Subdomain sub.example.com resolves deny (matches example.com/*)
	verdict, matched = vC.Policy().Decide(custos.DecideInput{
		Host: "sub.example.com",
		Port: 80,
	})
	if verdict != custos.VerdictDeny || matched != "example.com/*" {
		t.Errorf("sub.example.com decided %s (%s), want deny (example.com/*)", verdict, matched)
	}
}

// Egress strict toggle flips only the no-match default to ask per CUSTOS-SPEC §6.1:
// - strict off: credential-less egress defaults to auto
// - strict on: credential-less egress defaults to ask
// - rules still apply in both modes
// - floor and credential actions unaffected
func TestEgressStrictToggle(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)

	pe := v.Policy()

	// 1. Strict off by default: unlisted host resolves auto
	verdict, _ := pe.Decide(custos.DecideInput{
		Host: "unlisted.example.com",
	})
	if verdict != custos.VerdictAuto {
		t.Errorf("default strict-off Decide() = %s, want auto", verdict)
	}

	// Add an explicit deny rule
	_, _ = pe.AddEgressRule("denyhost.com", custos.VerdictDeny, false, "cli", "cli")

	verdict, _ = pe.Decide(custos.DecideInput{
		Host: "denyhost.com",
	})
	if verdict != custos.VerdictDeny {
		t.Errorf("denyhost.com Decide() = %s, want deny", verdict)
	}

	// 2. Flip egress strict on
	if err := pe.SetEgressStrict(true, "cli"); err != nil {
		t.Fatal(err)
	}
	if !pe.EgressStrict() {
		t.Fatal("expected EgressStrict to be true")
	}

	// Unlisted host now resolves ask
	verdict, _ = pe.Decide(custos.DecideInput{
		Host: "unlisted.example.com",
	})
	if verdict != custos.VerdictAsk {
		t.Errorf("strict-on unlisted host Decide() = %s, want ask", verdict)
	}

	// Explicit deny rule still resolves deny
	verdict, _ = pe.Decide(custos.DecideInput{
		Host: "denyhost.com",
	})
	if verdict != custos.VerdictDeny {
		t.Errorf("strict-on denyhost.com Decide() = %s, want deny", verdict)
	}

	// 3. Flip egress strict off
	if err := pe.SetEgressStrict(false, "cli"); err != nil {
		t.Fatal(err)
	}
	verdict, _ = pe.Decide(custos.DecideInput{
		Host: "unlisted.example.com",
	})
	if verdict != custos.VerdictAuto {
		t.Errorf("strict-off unlisted host Decide() = %s, want auto", verdict)
	}
}

// Two-tier comparator totality & IP-literal separation per CUSTOS §6.1, §6.3:
// - Host rules never match IP targets
// - IP rules never match Host targets
// - Exact host:port outranks exact host
// - Longest suffix wins among suffix rules
func TestTwoTierComparatorTotality(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)

	pe := v.Policy()

	// Add IP-form rule: 203.0.113.7 deny
	_, err := pe.AddEgressRule("203.0.113.7", custos.VerdictDeny, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}

	// Add host rule with wildcard covering everything: com/* auto
	// Wait, com/* is illegal; use example.com/* auto
	_, err = pe.AddEgressRule("example.com/*", custos.VerdictAuto, false, "cli", "cli")
	if err != nil {
		t.Fatal(err)
	}

	// 1. IP target matches IP-form rule exactly
	verdict, matched := pe.Decide(custos.DecideInput{
		Host: "203.0.113.7",
		Port: 80,
	})
	if verdict != custos.VerdictDeny || matched != "203.0.113.7" {
		t.Errorf("IP target decided %s (%s), want deny (203.0.113.7)", verdict, matched)
	}

	// 2. Unlisted IP target does NOT match host rules; follows credential-less default (auto)
	verdict, _ = pe.Decide(custos.DecideInput{
		Host: "198.51.100.1",
		Port: 80,
	})
	if verdict != custos.VerdictAuto {
		t.Errorf("unlisted IP target decided %s, want auto (host rules never match IP)", verdict)
	}

	// 3. Port qualification order: exact host:8080 outranks exact host
	_, _ = pe.AddEgressRule("api.service.com", custos.VerdictAsk, false, "cli", "cli")
	_, _ = pe.AddEgressRule("api.service.com:8080", custos.VerdictAuto, false, "cli", "cli")

	verdict, matched = pe.Decide(custos.DecideInput{
		Host: "api.service.com",
		Port: 8080,
	})
	if verdict != custos.VerdictAuto || matched != "api.service.com:8080" {
		t.Errorf("host:8080 decided %s (%s), want auto (api.service.com:8080)", verdict, matched)
	}

	verdict, matched = pe.Decide(custos.DecideInput{
		Host: "api.service.com",
		Port: 9000,
	})
	if verdict != custos.VerdictAsk || matched != "api.service.com" {
		t.Errorf("host:9000 decided %s (%s), want ask (api.service.com)", verdict, matched)
	}
}

// Worker vs bearer lane qualification per CUSTOS §6.2:
// - /tool-suffixed patterns match worker-lane calls ONLY
// - bearer-lane matches bare <name> only
func TestWorkerVsBearerLaneQualification(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)

	pe := v.Policy()

	_, _ = pe.AddCredentialRule("gmail/send", custos.VerdictAuto, false, "cli", "cli")
	_, _ = pe.AddCredentialRule("gmail/*", custos.VerdictAsk, false, "cli", "cli")

	// Worker lane with tool send matches gmail/send auto
	verdict, matched := pe.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "gmail",
		Tool:       "send",
	})
	if verdict != custos.VerdictAuto || matched != "gmail/send" {
		t.Errorf("worker send decided %s (%s), want auto (gmail/send)", verdict, matched)
	}

	// Worker lane with tool read matches gmail/* ask
	verdict, matched = pe.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "gmail",
		Tool:       "read",
	})
	if verdict != custos.VerdictAsk || matched != "gmail/*" {
		t.Errorf("worker read decided %s (%s), want ask (gmail/*)", verdict, matched)
	}

	// Bearer lane structurally ignores gmail/send and gmail/*
	// Has no bare gmail rule => returns credential default ask
	verdict, matched = pe.Decide(custos.DecideInput{
		Lane:       "bearer",
		Credential: "gmail",
	})
	if verdict != custos.VerdictAsk || matched != "" {
		t.Errorf("bearer lane decided %s (%s), want default ask (\"\")", verdict, matched)
	}

	// Add bare gmail deny rule
	_, _ = pe.AddCredentialRule("gmail", custos.VerdictDeny, false, "cli", "cli")

	// Now bearer lane matches bare gmail deny
	verdict, matched = pe.Decide(custos.DecideInput{
		Lane:       "bearer",
		Credential: "gmail",
	})
	if verdict != custos.VerdictDeny || matched != "gmail" {
		t.Errorf("bearer lane decided %s (%s), want deny (gmail)", verdict, matched)
	}

	// Worker lane gmail/send STILL resolves auto (gmail/send outranks bare gmail)
	verdict, matched = pe.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "gmail",
		Tool:       "send",
	})
	if verdict != custos.VerdictAuto || matched != "gmail/send" {
		t.Errorf("worker send decided %s (%s), want auto (gmail/send)", verdict, matched)
	}
}
