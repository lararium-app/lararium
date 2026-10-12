package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

func TestPolicySourceAdapter_Locked(t *testing.T) {
	// Nil vault
	nilAdapter := newPolicySourceAdapter(nil)
	if _, err := nilAdapter.ListRules(); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("ListRules(nil vault) = %v, want ErrPolicyLocked", err)
	}
	if nilAdapter.EgressStrict() {
		t.Errorf("EgressStrict(nil vault) = true, want false")
	}
	if _, err := nilAdapter.AddRule("credential", "demo/echo", "ask"); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("AddRule(nil vault) = %v, want ErrPolicyLocked", err)
	}
	if _, err := nilAdapter.RemoveRule("credential", "demo/echo"); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("RemoveRule(nil vault) = %v, want ErrPolicyLocked", err)
	}

	// Locked vault
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := custos.NewVault(stateDir, 5*time.Second)
	pass := "secret-test-passphrase"
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}

	adapter := newPolicySourceAdapter(v)
	if _, err := adapter.ListRules(); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("ListRules(locked) = %v, want ErrPolicyLocked", err)
	}
	if adapter.EgressStrict() {
		t.Errorf("EgressStrict(locked) = true, want false")
	}
	if _, err := adapter.AddRule("credential", "demo/echo", "ask"); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("AddRule(locked) = %v, want ErrPolicyLocked", err)
	}
	if _, err := adapter.RemoveRule("credential", "demo/echo"); !errors.Is(err, surface.ErrPolicyLocked) {
		t.Errorf("RemoveRule(locked) = %v, want ErrPolicyLocked", err)
	}
}

func TestPolicySourceAdapter_Unlocked(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := custos.NewVault(stateDir, 5*time.Second)
	pass := "secret-test-passphrase"
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("v.Unlock: %v", err)
	}

	adapter := newPolicySourceAdapter(v)

	// 1. Fresh engine
	rules, err := adapter.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("fresh rules len = %d, want 0", len(rules))
	}
	if adapter.EgressStrict() {
		t.Errorf("fresh EgressStrict = true, want false")
	}

	// 2. Add credential rule
	notes, err := adapter.AddRule("credential", "demo/echo", "ask")
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	_ = notes

	rules, err = adapter.ListRules()
	if err != nil {
		t.Fatalf("ListRules after add: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules len = %d, want 1", len(rules))
	}
	if rules[0].Lane != "credential" || rules[0].Pattern != "demo/echo" || rules[0].Verdict != "ask" || rules[0].Source != "web" || rules[0].Always {
		t.Errorf("rule mismatch: %+v", rules[0])
	}

	// 3. Add egress rule in canonical suffix form (engine rejects *. prefix
	// per §6.3 — the surface passes patterns through unmodified, ctl parity)
	_, err = adapter.AddRule("egress", "proxy.test/*", "auto")
	if err != nil {
		t.Fatalf("AddRule egress: %v", err)
	}
	if _, errBad := adapter.AddRule("egress", "*.proxy.test", "auto"); errBad == nil {
		t.Errorf("AddRule(*.proxy.test) = nil error, want engine §6.3 rejection")
	}

	rules, err = adapter.ListRules()
	if err != nil {
		t.Fatalf("ListRules after add egress: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules len = %d, want 2", len(rules))
	}

	// 4. Remove rule
	removed, err := adapter.RemoveRule("credential", "demo/echo")
	if err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	if !removed {
		t.Errorf("removed = false, want true")
	}

	// 5. Remove non-existent rule
	removed2, err := adapter.RemoveRule("credential", "demo/echo")
	if err != nil {
		t.Fatalf("RemoveRule second: %v", err)
	}
	if removed2 {
		t.Errorf("removed2 = true, want false")
	}
}

func TestPolicySourceAdapter_Corrupt(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := custos.NewVault(stateDir, 5*time.Second)
	pass := "secret-test-passphrase"
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}

	// Corrupt policy.json on disk
	policyPath := filepath.Join(stateDir, "policy.json")
	if err := os.WriteFile(policyPath, []byte("invalid-json{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("v.Unlock: %v", err)
	}

	adapter := newPolicySourceAdapter(v)

	// ListRules -> ErrPolicyCorrupt
	if _, err := adapter.ListRules(); !errors.Is(err, surface.ErrPolicyCorrupt) {
		t.Fatalf("ListRules corrupt err = %v, want ErrPolicyCorrupt", err)
	}

	// AddRule -> ErrPolicyCorrupt
	if _, err := adapter.AddRule("credential", "demo/echo", "ask"); !errors.Is(err, surface.ErrPolicyCorrupt) {
		t.Fatalf("AddRule corrupt err = %v, want ErrPolicyCorrupt", err)
	}

	// RemoveRule -> ErrPolicyCorrupt
	if _, err := adapter.RemoveRule("credential", "demo/echo"); !errors.Is(err, surface.ErrPolicyCorrupt) {
		t.Fatalf("RemoveRule corrupt err = %v, want ErrPolicyCorrupt", err)
	}
}
