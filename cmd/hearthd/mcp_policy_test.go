package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/surface"
)

type mockConnectorCaller struct {
	mu    sync.Mutex
	calls []mockCall
	resp  json.RawMessage
	err   error
}

type mockCall struct {
	connector string
	tool      string
	args      json.RawMessage
}

func (m *mockConnectorCaller) Call(_ context.Context, connector, tool string, args json.RawMessage) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, mockCall{connector: connector, tool: tool, args: args})
	if m.err != nil {
		return nil, m.err
	}
	if m.resp != nil {
		return m.resp, nil
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"result ok"}]}`), nil
}

func (m *mockConnectorCaller) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

type mockToolClient struct {
	mockConnectorCaller

	tools []custosdoor.ToolInfo
}

func (m *mockToolClient) ListTools() []custosdoor.ToolInfo {
	return m.tools
}

// fakePolicyStore provides a test policy engine backed by custos.PolicyEngine.
type fakePolicyStore struct {
	pe *custos.PolicyEngine
}

func newFakePolicyStore(t *testing.T) *fakePolicyStore {
	t.Helper()
	dir := shortTmp(t)
	v := custos.NewVault(dir, 5*time.Second)
	return &fakePolicyStore{pe: v.Policy()}
}

func (f *fakePolicyStore) Decide(connector, tool string) string {
	verdict, _ := f.pe.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: connector,
		Tool:       tool,
		Host:       connector,
	})
	if verdict == "" {
		return custos.VerdictAsk
	}
	return verdict
}

func (f *fakePolicyStore) AddRule(t *testing.T, pattern, verdict string) {
	t.Helper()
	_, err := f.pe.AddCredentialRule(pattern, verdict, false, "test", "test")
	if err != nil {
		t.Fatalf("AddCredentialRule(%q, %q): %v", pattern, verdict, err)
	}
}

// TestMCPPolicy_GateVerdicts tests the policy gate for MCP tools (issue #129):
// (a) no rule -> ask (untrusted: prompts human approver, runs when approved)
// (b) auto -> silent execute (trusted: runs without card).
// (c) deny -> error result without card (trusted: Run returns blocked by policy error, connector uncalled).
// (d) pattern `connector/*` matches tool calls.
func TestMCPPolicy_GateVerdicts(t *testing.T) {
	tests := []struct {
		name         string
		pattern      string
		verdict      string // empty if no rule
		toolName     string
		wantTrusted  bool
		wantErrSub   string
		wantExecuted bool
	}{
		{
			name:         "(a) no rule -> ask (untrusted fail-closed default)",
			pattern:      "",
			verdict:      "",
			toolName:     "searxng_search",
			wantTrusted:  false,
			wantExecuted: true,
		},
		{
			name:         "(b) auto exact rule -> silent execute",
			pattern:      "searxng/searxng_search",
			verdict:      custos.VerdictAuto,
			toolName:     "searxng_search",
			wantTrusted:  true,
			wantExecuted: true,
		},
		{
			name:         "(c) deny exact rule -> error result without card",
			pattern:      "searxng/searxng_search",
			verdict:      custos.VerdictDeny,
			toolName:     "searxng_search",
			wantTrusted:  true,
			wantErrSub:   "blocked by policy",
			wantExecuted: false,
		},
		{
			name:         "(d) pattern connector/* auto matches tool call",
			pattern:      "searxng/*",
			verdict:      custos.VerdictAuto,
			toolName:     "searxng_search",
			wantTrusted:  true,
			wantExecuted: true,
		},
		{
			name:         "(d) pattern connector/* deny matches tool call",
			pattern:      "searxng/*",
			verdict:      custos.VerdictDeny,
			toolName:     "searxng_search",
			wantTrusted:  true,
			wantErrSub:   "blocked by policy",
			wantExecuted: false,
		},
		{
			name:         "pattern bare connector auto matches tool call",
			pattern:      "searxng",
			verdict:      custos.VerdictAuto,
			toolName:     "searxng_search",
			wantTrusted:  true,
			wantExecuted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePolicyStore(t)
			if tc.pattern != "" && tc.verdict != "" {
				store.AddRule(t, tc.pattern, tc.verdict)
			}

			client := &mockToolClient{
				tools: []custosdoor.ToolInfo{
					{
						Connector:   "searxng",
						Name:        tc.toolName,
						Title:       "Search tool",
						InputSchema: json.RawMessage(`{"type":"object"}`),
					},
				},
			}

			toolSource := mcpToolSource(client, store)
			tools := toolSource()
			if len(tools) != 1 {
				t.Fatalf("toolSource returned %d tools, want 1", len(tools))
			}
			tool := tools[0]

			// Verify Trusted matches expected card behavior
			if tool.Trusted != tc.wantTrusted {
				t.Errorf("tool.Trusted = %v, want %v", tool.Trusted, tc.wantTrusted)
			}

			// Execute tool Run
			ctx := context.Background()
			res, err := tool.Run(ctx, `{"query":"test"}`)

			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("tool.Run() err = %v, want error containing %q", err, tc.wantErrSub)
				}
			} else if err != nil {
				t.Fatalf("tool.Run() unexpected error: %v", err)
			}

			// Verify whether connector was actually called
			if tc.wantExecuted {
				if client.CallCount() != 1 {
					t.Errorf("client.CallCount = %d, want 1", client.CallCount())
				}
				if !strings.Contains(res, "result") {
					t.Errorf("tool.Run() result = %q, want json with 'result'", res)
				}
			} else if client.CallCount() != 0 {
				t.Errorf("client.CallCount = %d, want 0 (blocked calls must not reach connector)", client.CallCount())
			}
		})
	}
}

// TestMCPPolicy_WildcardComparatorOrder tests that exact tool rules override
// wildcard rules according to normative comparator law (<name>/<tool> > bare <name> > <name>/*).
func TestMCPPolicy_WildcardComparatorOrder(t *testing.T) {
	store := newFakePolicyStore(t)
	// Base wildcard is ask
	store.AddRule(t, "github/*", custos.VerdictAsk)
	// Specific tool is auto
	store.AddRule(t, "github/read_repo", custos.VerdictAuto)
	// Another specific tool is deny
	store.AddRule(t, "github/delete_repo", custos.VerdictDeny)

	client := &mockToolClient{
		tools: []custosdoor.ToolInfo{
			{Connector: "github", Name: "read_repo", Title: "Read repo"},
			{Connector: "github", Name: "delete_repo", Title: "Delete repo"},
			{Connector: "github", Name: "other_action", Title: "Other action"},
		},
	}

	toolSource := mcpToolSource(client, store)
	tools := toolSource()
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(tools))
	}

	byName := make(map[string]loop.Tool)
	for _, tool := range tools {
		byName[tool.Spec.Name] = tool
	}

	// 1. read_repo -> exact auto overrides wildcard ask
	readTool := byName["mcp__github__read_repo"]
	if !readTool.Trusted {
		t.Errorf("read_repo Trusted = false, want true (auto rule)")
	}
	if _, err := readTool.Run(context.Background(), `{}`); err != nil {
		t.Errorf("read_repo Run() err = %v, want nil", err)
	}

	// 2. delete_repo -> exact deny overrides wildcard ask
	delTool := byName["mcp__github__delete_repo"]
	if !delTool.Trusted {
		t.Errorf("delete_repo Trusted = false, want true (deny skips card to error directly)")
	}
	if _, err := delTool.Run(context.Background(), `{}`); err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Errorf("delete_repo Run() err = %v, want 'blocked by policy'", err)
	}

	// 3. other_action -> falls back to wildcard ask
	otherTool := byName["mcp__github__other_action"]
	if otherTool.Trusted {
		t.Errorf("other_action Trusted = true, want false (ask rule)")
	}

	// 4. Client should have only executed read_repo
	if client.CallCount() != 1 {
		t.Errorf("client.CallCount = %d, want 1", client.CallCount())
	}
}

// TestMCPPolicy_MidSessionHotReload tests (e):
// Rule added mid-session changes next-call verdict (hot reload consistency with proxy lane).
func TestMCPPolicy_MidSessionHotReload(t *testing.T) {
	store := newFakePolicyStore(t)
	client := &mockToolClient{
		tools: []custosdoor.ToolInfo{
			{Connector: "searxng", Name: "web_search", Title: "Web search"},
		},
	}

	toolSource := mcpToolSource(client, store)

	// Step 1: No rule configured -> ask (untrusted)
	t1 := toolSource()[0]
	if t1.Trusted {
		t.Fatalf("initial turn: Trusted = true, want false (no rule -> ask)")
	}

	// Step 2: Add auto rule mid-session -> next call pulls fresh tools, becomes trusted
	store.AddRule(t, "searxng/*", custos.VerdictAuto)
	t2 := toolSource()[0]
	if !t2.Trusted {
		t.Fatalf("after adding auto rule: Trusted = false, want true")
	}
	if _, err := t2.Run(context.Background(), `{}`); err != nil {
		t.Fatalf("t2.Run() err = %v, want nil", err)
	}
	if client.CallCount() != 1 {
		t.Fatalf("client.CallCount = %d, want 1", client.CallCount())
	}

	// Step 3: Flip rule to deny mid-session -> next call pulls fresh tools, becomes deny (no card, blocked)
	store.AddRule(t, "searxng/*", custos.VerdictDeny)
	t3 := toolSource()[0]
	if !t3.Trusted {
		t.Fatalf("after flipping to deny: Trusted = false, want true (skips approval card)")
	}
	if _, err := t3.Run(context.Background(), `{}`); err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Fatalf("t3.Run() err = %v, want blocked by policy error", err)
	}
	if client.CallCount() != 1 {
		t.Fatalf("client.CallCount = %d, want 1 (blocked call did not run)", client.CallCount())
	}
}

// TestMCPPolicy_FlipDuringPendingApproval verifies that if a tool was gated as ask,
// but policy flips to deny before the approved Run executes, Run re-checks policy
// and aborts before executing the upstream connector call (§6.2 doctrine).
func TestMCPPolicy_FlipDuringPendingApproval(t *testing.T) {
	store := newFakePolicyStore(t)
	client := &mockToolClient{
		tools: []custosdoor.ToolInfo{
			{Connector: "searxng", Name: "web_search", Title: "Web search"},
		},
	}

	toolSource := mcpToolSource(client, store)

	// Created with no rule -> ask (approval card popped)
	tool := toolSource()[0]
	if tool.Trusted {
		t.Fatalf("Trusted = true, want false (ask)")
	}

	// While approval is pending, admin flips policy to deny
	store.AddRule(t, "searxng/*", custos.VerdictDeny)

	// Approver approves and calls tool.Run()
	_, err := tool.Run(context.Background(), `{}`)
	if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Fatalf("Run() err = %v, want blocked by policy error", err)
	}

	// Upstream connector must not have been called
	if client.CallCount() != 0 {
		t.Fatalf("client.CallCount = %d, want 0 (flip during hold must block upstream call)", client.CallCount())
	}
}

func TestMCPPolicy_AutoToAskFlipMidTurnFailsClosed(t *testing.T) {
	store := newFakePolicyStore(t)
	store.AddRule(t, "searxng/*", custos.VerdictAuto)
	client := &mockToolClient{
		tools: []custosdoor.ToolInfo{
			{Connector: "searxng", Name: "web_search", Title: "Web search"},
		},
	}

	// Turn started while auto -> tool built trusted (no card).
	tool := mcpToolSource(client, store)()[0]
	if !tool.Trusted {
		t.Fatalf("Trusted = false, want true (auto at turn start)")
	}

	// Admin revokes auto before the tool executes. No card was ever
	// raised for the new state, so executing silently would be a
	// fail-open window — the run must be refused instead.
	store.AddRule(t, "searxng/*", custos.VerdictAsk)

	if _, err := tool.Run(context.Background(), `{}`); err == nil ||
		!strings.Contains(err.Error(), "policy changed to ask") {
		t.Fatalf("Run() err = %v, want 'policy changed to ask' refusal", err)
	}
	if client.CallCount() != 0 {
		t.Fatalf("client.CallCount = %d, want 0 (auto->ask flip must not execute)", client.CallCount())
	}
}

// TestMCPPolicy_ServerDeciderIntegration tests the full wiring through
// surface.Server and policySourceAdapter.
func TestMCPPolicy_ServerDeciderIntegration(t *testing.T) {
	dir := shortTmp(t)
	v := custos.NewVault(filepath.Join(dir, "custos"), 5*time.Second)
	if err := v.Init("test-passphrase"); err != nil {
		t.Fatalf("v.Init: %v", err)
	}

	keyPath := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(keyPath, []byte("test-passphrase\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &surface.Server{}
	wireCustosPolicy(srv, dir, CustosConfig{
		DoorsSock:     filepath.Join(dir, "custos", "doors.sock"),
		UnlockKeyfile: keyPath,
	})

	decider := &serverPolicyDecider{srv: srv}

	// 1. Initial state: no rule -> ask
	if v := decider.Decide("searxng", "search"); v != custos.VerdictAsk {
		t.Fatalf("Decide() = %q, want %q", v, custos.VerdictAsk)
	}

	// 2. Add rule via surface.PolicySource
	ps := srv.PolicySource()
	if ps == nil {
		t.Fatal("srv.PolicySource() returned nil")
	}
	if _, err := ps.AddRule("worker", "searxng/*", custos.VerdictAuto); err != nil {
		t.Fatalf("ps.AddRule: %v", err)
	}

	// 3. Immediately visible to decider
	if v := decider.Decide("searxng", "search"); v != custos.VerdictAuto {
		t.Fatalf("Decide() after AddRule = %q, want %q", v, custos.VerdictAuto)
	}
}

// TestMCPPolicy_NilDeciderFallback verifies fail-closed behavior when decider is nil.
func TestMCPPolicy_NilDeciderFallback(t *testing.T) {
	client := &mockToolClient{
		tools: []custosdoor.ToolInfo{
			{Connector: "searxng", Name: "search", Title: "Search"},
		},
	}

	toolSource := mcpToolSource(client) // no decider passed
	tools := toolSource()
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool := tools[0]

	// Fail-closed default: untrusted
	if tool.Trusted {
		t.Errorf("tool.Trusted = true, want false (fail-closed nil decider default)")
	}

	// When approved and run, succeeds
	if _, err := tool.Run(context.Background(), `{}`); err != nil {
		t.Errorf("Run() unexpected error: %v", err)
	}
	if client.CallCount() != 1 {
		t.Errorf("client.CallCount = %d, want 1", client.CallCount())
	}
}

// TestMCPPolicy_ConnectorError verifies upstream errors from connector are passed through.
func TestMCPPolicy_ConnectorError(t *testing.T) {
	client := &mockToolClient{
		mockConnectorCaller: mockConnectorCaller{
			err: errors.New("upstream connection reset"),
		},
		tools: []custosdoor.ToolInfo{
			{Connector: "searxng", Name: "search", Title: "Search"},
		},
	}

	store := newFakePolicyStore(t)
	store.AddRule(t, "searxng/*", custos.VerdictAuto)

	tool := mcpToolSource(client, store)()[0]
	_, err := tool.Run(context.Background(), `{}`)
	if err == nil || !strings.Contains(err.Error(), "upstream connection reset") {
		t.Fatalf("Run() err = %v, want upstream error", err)
	}
}
