package custos_test

import (
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// H4 (audit 2026-10): bearer-lane "always" verdicts mint *scoped*
// credential rules <name>@host[:port] instead of bare global names.
// Covers the grammar gate (§6.4b: IP-literal bindings are ask-only),
// lane isolation (host-scoped rules never match the worker lane), and
// the comparator ladder (host:port > host > bare).

func TestH4CredScopeGrammar(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)
	pe := v.Policy()

	cases := []struct {
		pattern string
		verdict string
		wantErr bool
	}{
		// Valid scoped forms
		{"svc@api.example.com", custos.VerdictAuto, false},
		{"svc@api.example.com:8443", custos.VerdictAuto, false},
		{"svc@198.51.100.7", custos.VerdictAsk, false}, // IP literal: ask-only OK
		{"svc@198.51.100.7:8443", custos.VerdictAsk, false},
		{"svc@[2001:db8::1]", custos.VerdictAsk, false}, // bracketed IPv6 OK
		{"svc@[2001:db8::1]:8443", custos.VerdictAsk, false},
		// IP literals must never leave ask (§6.4b)
		{"svc@198.51.100.7", custos.VerdictAuto, true},
		{"svc@198.51.100.7:8443", custos.VerdictDeny, true},
		{"svc@[2001:db8::1]", custos.VerdictAuto, true},
		// Bare IPv6 without brackets
		{"svc@2001:db8::1", custos.VerdictAsk, true},
		// Grammar refusals
		{"svc@", custos.VerdictAuto, true},
		{"@api.example.com", custos.VerdictAuto, true},
		{"svc@api.example.com:0", custos.VerdictAuto, true},
		{"svc@api.example.com:65536", custos.VerdictAuto, true},
		{"svc@api.example.com:0443", custos.VerdictAuto, true},
		{"svc@API.example.com", custos.VerdictAuto, true}, // uppercase host
		{"Svc@api.example.com", custos.VerdictAuto, true}, // uppercase name
		{"svc@ex..ample.com", custos.VerdictAuto, true},
		{"svc@.api.example.com", custos.VerdictAuto, true},
		{"svc@api.example.com/", custos.VerdictAuto, true}, // no paths in authorities
		{"svc@ex ample.com", custos.VerdictAuto, true},
	}
	for _, tc := range cases {
		_, err := pe.AddCredentialRule(tc.pattern, tc.verdict, false, "cli", "cli")
		if tc.wantErr && err == nil {
			t.Errorf("AddCredentialRule(%q, %s) accepted, want ErrInvalidPattern", tc.pattern, tc.verdict)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("AddCredentialRule(%q, %s) rejected: %v", tc.pattern, tc.verdict, err)
		}
	}
}

func TestH4CredScopeBearerDecide(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)
	pe := v.Policy()

	if _, err := pe.AddCredentialRule("svc@api.example.com", custos.VerdictAuto, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.AddCredentialRule("svc@vault.example.com:8443", custos.VerdictAuto, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}

	// Scoped match: same credential, matching authority → auto
	if vd, pat := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "api.example.com", Port: 443}); vd != custos.VerdictAuto || pat != "svc@api.example.com" {
		t.Errorf("bearer svc@api.example.com:443 = %s (%s), want auto (svc@api.example.com)", vd, pat)
	}
	// Host is case-insensitive
	if vd, _ := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "API.Example.COM", Port: 443}); vd != custos.VerdictAuto {
		t.Errorf("bearer svc@API.Example.COM = %s, want auto", vd)
	}
	// Different host: scoped rule must NOT fire (this is the H4 fix —
	// bare-name always-rules used to authorize every destination)
	if vd, _ := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "evil.example.net", Port: 443}); vd != custos.VerdictAsk {
		t.Errorf("bearer svc@evil.example.net = %s, want ask", vd)
	}
	// Subdomain must NOT fire a scoped apex rule (scoped rules are exact hosts)
	if vd, _ := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "sub.api.example.com", Port: 443}); vd != custos.VerdictAsk {
		t.Errorf("bearer svc@sub.api.example.com = %s, want ask", vd)
	}
	// Different credential never borrows the scope
	if vd, _ := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "other", Host: "api.example.com", Port: 443}); vd != custos.VerdictAsk {
		t.Errorf("bearer other@api.example.com = %s, want ask", vd)
	}
	// Port-scoped: matching port auto, other port ask
	if vd, pat := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "vault.example.com", Port: 8443}); vd != custos.VerdictAuto || pat != "svc@vault.example.com:8443" {
		t.Errorf("bearer svc@vault:8443 = %s (%s), want auto (svc@vault.example.com:8443)", vd, pat)
	}
	if vd, _ := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "vault.example.com", Port: 9999}); vd != custos.VerdictAsk {
		t.Errorf("bearer svc@vault:9999 = %s, want ask", vd)
	}
}

func TestH4CredScopeComparatorOverBare(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)
	pe := v.Policy()

	// Bare rule auto, scoped deny: scoped (more specific) wins on its host
	if _, err := pe.AddCredentialRule("gmail", custos.VerdictAuto, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.AddCredentialRule("gmail@smtp.corp.example", custos.VerdictDeny, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if vd, pat := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "gmail", Host: "smtp.corp.example", Port: 465}); vd != custos.VerdictDeny || pat != "gmail@smtp.corp.example" {
		t.Errorf("scoped deny over bare auto: got %s (%s), want deny (gmail@smtp.corp.example)", vd, pat)
	}
	// Off-scope host falls back to the bare rule
	if vd, pat := pe.Decide(custos.DecideInput{Lane: "bearer", Credential: "gmail", Host: "gmail.com", Port: 443}); vd != custos.VerdictAuto || pat != "gmail" {
		t.Errorf("bare fallback: got %s (%s), want auto (gmail)", vd, pat)
	}
	// host:port scope outranks host scope
	pe2, _, pass2 := setupTestVault(t)
	_ = pe2.Init(pass2)
	_ = pe2.Unlock(pass2, false)
	if _, err := pe2.Policy().AddCredentialRule("svc@api.example.com", custos.VerdictDeny, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := pe2.Policy().AddCredentialRule("svc@api.example.com:8443", custos.VerdictAuto, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if vd, pat := pe2.Policy().Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "api.example.com", Port: 8443}); vd != custos.VerdictAuto || pat != "svc@api.example.com:8443" {
		t.Errorf("host:port over host scope: got %s (%s), want auto (svc@api.example.com:8443)", vd, pat)
	}
	if vd, _ := pe2.Policy().Decide(custos.DecideInput{Lane: "bearer", Credential: "svc", Host: "api.example.com", Port: 443}); vd != custos.VerdictDeny {
		t.Errorf("host scope still applies off-port: got %s, want deny", vd)
	}
}

func TestH4CredScopeNeverWorkerLane(t *testing.T) {
	v, _, pass := setupTestVault(t)
	_ = v.Init(pass)
	_ = v.Unlock(pass, false)
	pe := v.Policy()

	// A scoped auto rule must grant ZERO worker-lane authority: the
	// worker lane has no destination to check the scope against, so
	// only bare/wildcard/qualified rules may match there.
	if _, err := pe.AddCredentialRule("deploy@api.example.com", custos.VerdictAuto, false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if vd, pat := pe.Decide(custos.DecideInput{Lane: "worker", Credential: "deploy", Tool: "rollout"}); vd != custos.VerdictAsk || pat != "" {
		t.Errorf("worker lane with only a scoped rule: got %s (%s), want ask (no pattern)", vd, pat)
	}
	// And the scoped pattern alone must not auto-approve the same
	// credential on the worker lane with an empty tool either.
	if vd, _ := pe.Decide(custos.DecideInput{Lane: "worker", Credential: "deploy"}); vd != custos.VerdictAsk {
		t.Errorf("worker lane empty tool with scoped-only rules: got %s, want ask", vd)
	}
}
