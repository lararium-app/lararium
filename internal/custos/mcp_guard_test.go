// H2 regression: MCP-CALL carries the worker-lane enforcement gate
// (vault lock, worker policy verdict, approval hold, audit) on every
// socket, so a door.token or ctl.token holder can never execute a rail
// tool with a locked vault or an ask/deny verdict set.

package custos_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// guardFanout captures the approval IDs the hub fans out so a test can
// resolve a custos-source card (observers only see turn cards).
type guardFanout struct{ ids chan string }

func (g guardFanout) ApprovalPending(id, sessionID, name, argsSummary string) {
	if sessionID == "guardtest" {
		select {
		case g.ids <- id:
		default:
		}
	}
}

func (g guardFanout) ApprovalTerminal(id, sessionID, state, reason, source string) {}

func newGuardVault(t *testing.T) *custos.Vault {
	t.Helper()
	v := custos.NewVault(t.TempDir(), 5*time.Second)
	if err := v.Init("test-passphrase-9f3k"); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if err := v.Unlock("test-passphrase-9f3k", false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}
	return v
}

func dispatchCall(t *testing.T, reg *custos.RailRegistry, tool string, g custos.MCPCallGuard) string {
	t.Helper()
	var got string
	if !custos.DispatchMCPCall(nil, reg, "alpha", tool, `{"message":"hi"}`, captureWrite(t, &got), g) {
		t.Fatal("DispatchMCPCall did not handle the call")
	}
	return got
}

func TestMCPCallGuard_LockedVaultRefused(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	if err := v.Lock(); err != nil {
		t.Fatalf("lock: %v", err)
	}
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{Vault: v, Actor: "guardtest"})
	if got != "ERR locked\n" {
		t.Fatalf("locked vault frame = %q, want ERR locked", got)
	}
}

func TestMCPCallGuard_DenyVerdictRefused(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	if _, err := v.Policy().AddCredentialRule("alpha/ping", "deny", false, "test", "guardtest"); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{Vault: v, Actor: "guardtest"})
	if got != "ERR denied\n" {
		t.Fatalf("deny frame = %q, want ERR denied", got)
	}
}

func TestMCPCallGuard_AskWithoutHubFailsClosed(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{Vault: v, Actor: "guardtest"})
	if got != "ERR approval_required\n" {
		t.Fatalf("ask-no-hub frame = %q, want ERR approval_required", got)
	}
}

// ask + card denied → ERR denied, and the tool never executed (an
// executed echo would answer OK …pong).
func TestMCPCallGuard_AskCardDenied(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	hub := surface.NewApprovalHub(30 * time.Second)
	ids := make(chan string, 4)
	hub.SetFanout(guardFanout{ids: ids})

	guard := custos.MCPCallGuard{Vault: v, Hub: hub, Actor: "guardtest", Hold: 20 * time.Second}
	done := make(chan string, 1)
	go func() { done <- dispatchCall(t, reg, "ping", guard) }()

	select {
	case id := <-ids:
		if code := hub.ResolveFrom("guardtest", id, false, "test"); code != 200 {
			t.Fatalf("resolve deny code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no approval card raised for ask verdict")
	}

	select {
	case got := <-done:
		if got != "ERR denied\n" {
			t.Fatalf("card-denied frame = %q, want ERR denied", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch did not return after card denial")
	}
}

func TestMCPCallGuard_AskCardApproved(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	hub := surface.NewApprovalHub(30 * time.Second)
	ids := make(chan string, 4)
	hub.SetFanout(guardFanout{ids: ids})

	guard := custos.MCPCallGuard{Vault: v, Hub: hub, Actor: "guardtest", Hold: 20 * time.Second}
	done := make(chan string, 1)
	go func() { done <- dispatchCall(t, reg, "echo", guard) }()

	select {
	case id := <-ids:
		if code := hub.ResolveFrom("guardtest", id, true, "test"); code != 200 {
			t.Fatalf("resolve approve code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no approval card raised for ask verdict")
	}

	select {
	case got := <-done:
		if !strings.HasPrefix(got, "OK ") || !strings.Contains(got, "echo:hi") {
			t.Fatalf("card-approved frame = %q, want OK echo result", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("dispatch did not return after card approval")
	}
}
