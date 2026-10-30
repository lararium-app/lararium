package custos_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// doorFxValue is the fake secret under test (9876-style tail for leak greps).
const doorFxValue = "sk-liv-DOOR...9876"

type doorCall struct {
	args json.RawMessage
	cred custos.Credential
}

// doorConnector records what Execute saw so tests can assert auth state and
// the args actually forwarded upstream.
type doorConnector struct {
	mu    sync.Mutex
	calls []doorCall
}

func (*doorConnector) Name() string             { return "fx" }
func (*doorConnector) Credential() string       { return "fx" }
func (*doorConnector) Host() string             { return "fx.test" }
func (*doorConnector) HasTool(tool string) bool { return tool == "a" || tool == "b" }
func (*doorConnector) ValidateArgs(_ string, args json.RawMessage) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) == nil {
		if _, bad := m["credential"]; bad {
			var s string
			if json.Unmarshal(m["credential"], &s) == nil && strings.HasPrefix(s, "hdl_") {
				return context.Canceled // handle must be stripped before validation
			}
		}
	}
	return nil
}
func (*doorConnector) ReviewFields(string, json.RawMessage) string { return "fx" }
func (c *doorConnector) Execute(_ context.Context, _ string, args json.RawMessage, cred custos.Credential) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, doorCall{args: args, cred: cred})
	return json.RawMessage(`{"done":true}`), nil
}

func (c *doorConnector) last(t *testing.T) doorCall {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		t.Fatal("connector never executed")
	}
	return c.calls[len(c.calls)-1]
}

func setupDoor(t *testing.T) (*workerHarness, *doorConnector, string) {
	t.Helper()
	h := setupWorkerHarness(t, &fakeOAuth{})
	conn := &doorConnector{}
	if err := h.ws.RegisterConnector(conn); err != nil {
		t.Fatal(err)
	}
	if err := h.v.Mutate("", func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["fx"] = custos.Credential{Kind: "api_key", Secret: doorFxValue}
		return []string{"fx"}, nil
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.v.Policy().AddCredentialRule("fx/*", "auto", false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	return h, conn, h.v.StateDir()
}

func (h *workerHarness) doorCall(t *testing.T, tool, args string) custos.WorkerResponse {
	t.Helper()
	return h.send(t, custos.WorkerRequest{Op: "call", Connector: "fx", Tool: tool, Args: json.RawMessage(args), CellToken: v13CellToken})
}

func TestDoorB1_ValidHandle(t *testing.T) {
	h, conn, stateDir := setupDoor(t)
	rec, err := h.v.CreateHandle("", "fx", "fx", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	resp := h.doorCall(t, "a", `{"q":1,"credential":"`+rec.Handle+`"}`)
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
	got := conn.last(t)
	if got.cred.Secret != doorFxValue {
		t.Fatalf("not authenticated: %+v", got.cred)
	}
	if strings.Contains(string(got.args), "credential") || strings.Contains(string(got.args), rec.Handle) {
		t.Fatalf("args leaked handle: %s", got.args)
	}
	if string(got.args) != `{"q":1}` {
		t.Fatalf("args = %s", got.args)
	}
	if used := auditByKind(t, stateDir, custos.AuditKindHandleUsed); len(used) != 1 || used[0].Sur != rec.Fingerprint() {
		t.Fatalf("handle_used = %+v", used)
	}
	if strings.Contains(strings.Join(readAuditLines(t, stateDir), "\n"), doorFxValue) {
		t.Fatal("secret in audit")
	}
}

func TestDoorDenials(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *workerHarness) (handle, tool string)
		kind  string
	}{
		{"B2_unknown", func(*workerHarness) (string, string) { return "hdl_doesnotexist0000000000", "a" }, custos.AuditKindHandleDeniedScope},
		{"B3_scope", func(h *workerHarness) (string, string) {
			rec, _ := h.v.CreateHandle("", "fx", "fx/a", 0, nil, "cli")
			return rec.Handle, "b"
		}, custos.AuditKindHandleDeniedScope},
		{"B4_rate", func(h *workerHarness) (string, string) {
			rec, _ := h.v.CreateHandle("", "fx", "fx", 0, &custos.HandleRate{N: 1, Window: 3600}, "cli")
			if g := h.v.ResolveHandle(rec.Handle, "a", "fx.test", 0); g.Deny != "" {
				t.Fatalf("priming resolve denied: %+v", g)
			}
			return rec.Handle, "a"
		}, custos.AuditKindHandleDeniedRate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, conn, stateDir := setupDoor(t)
			handle, tool := tc.setup(h)
			resp := h.doorCall(t, tool, `{"credential":"`+handle+`"}`)
			if !resp.OK {
				t.Fatalf("denial must proceed, got %+v", resp)
			}
			got := conn.last(t)
			if got.cred != (custos.Credential{}) {
				t.Fatalf("expected unauthenticated, got %+v", got.cred)
			}
			if strings.Contains(string(got.args), "credential") {
				t.Fatalf("handle forwarded: %s", got.args)
			}
			if n := len(auditByKind(t, stateDir, tc.kind)); n != 1 {
				t.Fatalf("%s audits = %d, want exactly 1", tc.kind, n)
			}
			if strings.Contains(strings.Join(readAuditLines(t, stateDir), "\n")+resp.Detail+string(resp.Result), doorFxValue) {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestDoorB5_NonHandleValuePassthrough(t *testing.T) {
	h, conn, _ := setupDoor(t)
	const args = `{"credential":"plain-value","q":2}`
	if resp := h.doorCall(t, "a", args); !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
	got := conn.last(t)
	if string(got.args) != args {
		t.Fatalf("args changed: %s", got.args)
	}
	if got.cred.Secret != doorFxValue {
		t.Fatalf("connector credential not used: %+v", got.cred)
	}
}

func TestDoorB6_OneShot(t *testing.T) {
	h, conn, _ := setupDoor(t)
	rec, err := h.v.CreateHandle("", "fx", "fx", 0, &custos.HandleRate{N: 1, Window: 3600}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	args := `{"credential":"` + rec.Handle + `"}`
	if resp := h.doorCall(t, "a", args); !resp.OK || conn.last(t).cred.Secret != doorFxValue {
		t.Fatalf("first call not authenticated: %+v", resp)
	}
	if resp := h.doorCall(t, "a", args); !resp.OK || conn.last(t).cred != (custos.Credential{}) {
		t.Fatalf("second call must be unauthenticated: %+v", resp)
	}
}

func TestDoorB10_VaultNeedles(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	tok, err := v.AddSurrogate(pass, "svc", "api.example.com", 443, "", false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	ns := v.Needles()
	var gotSecret, gotSur bool
	for _, n := range ns {
		gotSecret = gotSecret || n == handleTestValue
		gotSur = gotSur || n == tok
	}
	if !gotSecret || !gotSur {
		t.Fatalf("needles = %v", ns)
	}
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}
	if v.Needles() != nil {
		t.Fatal("needles while locked")
	}
}
