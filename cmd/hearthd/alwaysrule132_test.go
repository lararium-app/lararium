package main

// Issue #132 end-to-end at the wiring layer: settling a turn-approval
// card with "always" must land a worker-lane always-rule that Decide
// honors on the NEXT turn (silent auto), scoped to the exact
// connector/tool pair, and visible through the policy surface.

import (
	"errors"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

type fakeRailLister struct{ tools []custosdoor.ToolInfo }

func (f fakeRailLister) ListTools() []custosdoor.ToolInfo { return f.tools }

func newUnlockedAdapter(t *testing.T) *policySourceAdapter {
	t.Helper()
	v := custos.NewVault(t.TempDir(), 5*time.Second)
	pass := "test-passphrase-132"
	if err := v.Init(pass); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if !v.IsUnlocked() {
		if err := v.Unlock(pass, false); err != nil {
			t.Fatalf("vault unlock: %v", err)
		}
	}
	if v.Policy() == nil {
		t.Fatal("policy engine nil after init/unlock")
	}
	return &policySourceAdapter{vault: v}
}

func TestIssue132_AlwaysRuleEndToEnd(t *testing.T) {
	a := newUnlockedAdapter(t)
	srv := &surface.Server{}
	srv.SetPolicySource(a)

	lister := fakeRailLister{tools: []custosdoor.ToolInfo{
		{Connector: "github", Name: "create_issue"},
		{Connector: "gmail", Name: "send"},
	}}
	hook := turnAlwaysRuleMinter(srv, lister)

	// Pre-conditions: everything asks.
	if got := a.Decide("github", "create_issue"); got != custos.VerdictAsk {
		t.Fatalf("initial Decide = %q, want ask", got)
	}

	// The card settles with always → mint via model-facing name.
	if err := hook("mcp__github__create_issue"); err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Next turn: exact pair auto; siblings and other tools still ask.
	if got := a.Decide("github", "create_issue"); got != custos.VerdictAuto {
		t.Errorf("after mint Decide(target) = %q, want auto", got)
	}
	if got := a.Decide("github", "delete_repo"); got != custos.VerdictAsk {
		t.Errorf("sibling tool Decide = %q, want ask (rule leaked too wide)", got)
	}
	if got := a.Decide("gmail", "send"); got != custos.VerdictAsk {
		t.Errorf("other connector Decide = %q, want ask", got)
	}

	// The rule is visible with the always flag and card source.
	rules, err := a.ListRules()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rules {
		if r.Pattern == "github/create_issue" {
			found = true
			if r.Verdict != "auto" || !r.Always || r.Source != "always" {
				t.Errorf("rule row = %+v, want auto/always/always", r)
			}
		}
	}
	if !found {
		t.Errorf("minted rule missing from ListRules: %+v", rules)
	}
}

func TestIssue132_MintErrorPaths(t *testing.T) {
	a := newUnlockedAdapter(t)
	srv := &surface.Server{}
	srv.SetPolicySource(a)
	lister := fakeRailLister{tools: []custosdoor.ToolInfo{{Connector: "github", Name: "create_issue"}}}
	hook := turnAlwaysRuleMinter(srv, lister)

	if err := hook("memory_delete"); err == nil {
		t.Error("non-rail tool minted; want error (stale card / local tool)")
	}
	if err := hook(""); err == nil {
		t.Error("empty tool name minted; want error")
	}

	// No policy source wired → honest error, no panic.
	bare := &surface.Server{}
	if err := turnAlwaysRuleMinter(bare, lister)("mcp__github__create_issue"); err == nil {
		t.Error("mint without policy source succeeded; want error")
	}

	// Grammar-illegal tool name (dots are not in the policy credential
	// grammar): honest error rather than a rule that Decide would never
	// match.
	tricky := fakeRailLister{tools: []custosdoor.ToolInfo{{Connector: "github", Name: "a.b"}}}
	if err := turnAlwaysRuleMinter(srv, tricky)("mcp__github__a_b"); err == nil {
		t.Error("grammar-illegal tool minted; want error")
	}
}

func TestIssue132_AddAlwaysRuleLocked(t *testing.T) {
	var nilAd *policySourceAdapter
	if err := nilAd.AddAlwaysRule("github/create_issue"); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("nil adapter err = %v, want ErrPolicyLocked", err)
	}
	v := custos.NewVault(t.TempDir(), 5*time.Second)
	a := &policySourceAdapter{vault: v}
	if err := a.AddAlwaysRule("github/create_issue"); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("locked adapter err = %v, want ErrPolicyLocked", err)
	}
}
