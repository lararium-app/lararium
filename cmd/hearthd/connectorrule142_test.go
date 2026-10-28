package main

// Issue #142: table tests for resolve-with-scope:
// - absent scope = today's behavior;
// - allow+scope mints exactly the same rule shape custos mcp trust writes
//   (assert byte-identical rule string "<name>/*");
// - a follow-up tool call of that connector resolves auto without a card
//   (reuse alwaysrule132_test.go patterns);
// - mint-failure surfaces error (and approval remains pending);
// - deny+scope rejected (HTTP 400, approval remains pending).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

func TestIssue142_ResolveWithScopeTable(t *testing.T) {
	tests := []struct {
		name           string
		decision       string
		scope          string
		lockPolicy     bool
		badTool        bool
		wantCode       int
		wantRule       bool
		wantAuto       bool
		wantPendingEnd bool
	}{
		{
			name:           "absent scope preserves existing behavior (allow, no rule minted)",
			decision:       "allow",
			scope:          "",
			wantCode:       http.StatusOK,
			wantRule:       false,
			wantAuto:       false,
			wantPendingEnd: false, // settled
		},
		{
			name:           "deny with scope connector is invalid and rejected",
			decision:       "deny",
			scope:          "connector",
			wantCode:       http.StatusBadRequest,
			wantRule:       false,
			wantAuto:       false,
			wantPendingEnd: true, // not settled, still pending
		},
		{
			name:           "invalid scope string is rejected",
			decision:       "allow",
			scope:          "invalid_scope",
			wantCode:       http.StatusBadRequest,
			wantRule:       false,
			wantAuto:       false,
			wantPendingEnd: true, // not settled
		},
		{
			name:           "mint failure due to locked policy surfaces error",
			decision:       "allow",
			scope:          "connector",
			lockPolicy:     true,
			wantCode:       http.StatusInternalServerError,
			wantRule:       false,
			wantAuto:       false,
			wantPendingEnd: true, // not settled, remains pending
		},
		{
			name:           "mint failure due to non-rail tool surfaces error",
			decision:       "allow",
			scope:          "connector",
			badTool:        true,
			wantCode:       http.StatusInternalServerError,
			wantRule:       false,
			wantAuto:       false,
			wantPendingEnd: true, // not settled, remains pending
		},
		{
			name:           "allow with scope connector mints auto rule and enables silent execute",
			decision:       "allow",
			scope:          "connector",
			wantCode:       http.StatusOK,
			wantRule:       true,
			wantAuto:       true,
			wantPendingEnd: false, // settled
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newUnlockedAdapter(t)
			srv := &surface.Server{}
			srv.SetPolicySource(adapter)

			lister := fakeRailLister{tools: []custosdoor.ToolInfo{
				{Connector: "github", Name: "create_issue"},
				{Connector: "github", Name: "delete_repo"},
				{Connector: "gmail", Name: "send"},
			}}

			ap := surface.NewApprovalHub(10 * time.Second)
			hub := surface.NewHub(surface.ServeConfig{}, t.TempDir(), 1000, 80, nil, nil, ap)
			srv.SetApprovalHub(ap)

			// Wire the connector trust minter hook
			minter := turnConnectorTrustMinter(srv, lister)
			ap.SetConnectorTrustHook(minter)

			if tc.lockPolicy {
				// Locked adapter returns ErrPolicyLocked on mutation
				lockedVault := custos.NewVault(t.TempDir(), 5*time.Second)
				lockedAdapter := &policySourceAdapter{vault: lockedVault}
				srv.SetPolicySource(lockedAdapter)
			}

			// Pre-condition: Decide returns ask for all tools
			if got := adapter.Decide("github", "create_issue"); got != custos.VerdictAsk {
				t.Fatalf("initial Decide = %q, want %q", got, custos.VerdictAsk)
			}
			if got := adapter.Decide("github", "delete_repo"); got != custos.VerdictAsk {
				t.Fatalf("initial sibling Decide = %q, want %q", got, custos.VerdictAsk)
			}
			if got := adapter.Decide("gmail", "send"); got != custos.VerdictAsk {
				t.Fatalf("initial other connector Decide = %q, want %q", got, custos.VerdictAsk)
			}

			toolToRegister := "mcp__github__create_issue"
			if tc.badTool {
				toolToRegister = "unknown_tool_without_rail"
			}

			approvalID, ch := ap.Register("sess_1", toolToRegister, `{"title":"bug"}`, func(id string) {})

			bodyMap := map[string]string{
				"decision": tc.decision,
			}
			if tc.scope != "" {
				bodyMap["scope"] = tc.scope
			}
			reqBody, _ := json.Marshal(bodyMap)

			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess_1/approvals/"+approvalID, strings.NewReader(string(reqBody)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			hub.HandleApproval(w, req, "sess_1", approvalID)

			if w.Code != tc.wantCode {
				t.Fatalf("HandleApproval status = %d, want %d (body: %s)", w.Code, tc.wantCode, w.Body.String())
			}

			if tc.wantPendingEnd {
				// Approval must remain pending (not settled)
				select {
				case <-ch:
					t.Fatal("decision channel received value, want approval still pending")
				default:
					// Expected
				}
			} else {
				// Approval should be settled
				select {
				case dec := <-ch:
					wantAllow := tc.decision == "allow"
					if dec != wantAllow {
						t.Fatalf("decision received = %v, want %v", dec, wantAllow)
					}
				case <-time.After(200 * time.Millisecond):
					t.Fatal("decision not received on channel within timeout")
				}
			}

			// Verify rule in policy
			rules, err := adapter.ListRules()
			if err != nil && !tc.lockPolicy {
				t.Fatalf("ListRules: %v", err)
			}

			expectedPattern := "github/*" // exactly the rule string custos mcp trust writes
			var foundRule *surface.PolicyRule
			for _, r := range rules {
				if r.Pattern == expectedPattern {
					rCopy := r
					foundRule = &rCopy
					break
				}
			}

			if tc.wantRule {
				if foundRule == nil {
					t.Fatalf("expected rule %q not found in policy rules: %+v", expectedPattern, rules)
				}
				// Assert byte-identical rule shape matching custos mcp trust
				if foundRule.Pattern != "github/*" {
					t.Errorf("rule pattern = %q, want %q", foundRule.Pattern, "github/*")
				}
				if foundRule.Lane != "credential" {
					t.Errorf("rule lane = %q, want %q", foundRule.Lane, "credential")
				}
				if foundRule.Verdict != "auto" {
					t.Errorf("rule verdict = %q, want %q", foundRule.Verdict, "auto")
				}
				if foundRule.Always {
					t.Errorf("rule always = true, want false (custos mcp trust writes non-always rule)")
				}
			} else if foundRule != nil {
				t.Fatalf("unexpected rule %q found in policy rules: %+v", expectedPattern, rules)
			}

			// Verify follow-up Decide behavior
			if tc.wantAuto {
				// Follow-up tool call of that connector resolves auto without a card
				if got := adapter.Decide("github", "create_issue"); got != custos.VerdictAuto {
					t.Errorf("follow-up Decide(target) = %q, want auto", got)
				}
				// Sibling tool of same connector also resolves auto
				if got := adapter.Decide("github", "delete_repo"); got != custos.VerdictAuto {
					t.Errorf("follow-up Decide(sibling) = %q, want auto", got)
				}
				// Other connector still asks
				if got := adapter.Decide("gmail", "send"); got != custos.VerdictAsk {
					t.Errorf("other connector Decide = %q, want ask", got)
				}
			}
		})
	}
}

func TestIssue142_QueryParamScopeSupported(t *testing.T) {
	adapter := newUnlockedAdapter(t)
	srv := &surface.Server{}
	srv.SetPolicySource(adapter)

	lister := fakeRailLister{tools: []custosdoor.ToolInfo{
		{Connector: "searxng", Name: "search"},
	}}

	ap := surface.NewApprovalHub(10 * time.Second)
	hub := surface.NewHub(surface.ServeConfig{}, t.TempDir(), 1000, 80, nil, nil, ap)
	srv.SetApprovalHub(ap)
	ap.SetConnectorTrustHook(turnConnectorTrustMinter(srv, lister))

	approvalID, ch := ap.Register("s_q", "mcp__searxng__search", `{"q":"test"}`, func(id string) {})

	// Send scope via URL query param (?scope=connector) instead of body
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/s_q/approvals/"+approvalID+"?scope=connector", strings.NewReader(`{"decision":"allow"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	hub.HandleApproval(w, req, "s_q", approvalID)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleApproval with query param code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	select {
	case dec := <-ch:
		if !dec {
			t.Fatal("expected allow decision")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("decision timeout")
	}

	// Verify rule was minted
	if got := adapter.Decide("searxng", "search"); got != custos.VerdictAuto {
		t.Errorf("Decide after query param mint = %q, want auto", got)
	}
}

func TestIssue142_WireDepsPreservesSignatures(t *testing.T) {
	// Verify that WireDeps.ResolveApproval accepts scope optionally
	// without breaking 3-argument callers.
	ap := surface.NewApprovalHub(10 * time.Second)
	hub := surface.NewHub(surface.ServeConfig{}, t.TempDir(), 1000, 80, nil, nil, ap)
	wire := nuntiusWire(hub, ap, "test/model")

	id, ch := ap.Register("sess_nuntius", "echo", "{}", func(id string) {})

	// Calling with 3 arguments (existing callers)
	status := wire.ResolveApproval("sess_nuntius", id, true)
	if status != 200 {
		t.Fatalf("ResolveApproval 3-arg status = %d, want 200", status)
	}
	select {
	case dec := <-ch:
		if !dec {
			t.Fatal("expected allow")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}

	// Calling with 4 arguments (optional scope parameter)
	id2, _ := ap.Register("sess_nuntius", "echo", "{}", func(id string) {})
	status2 := wire.ResolveApproval("sess_nuntius", id2, false, "connector")
	// deny+scope is rejected as 400
	if status2 != 400 {
		t.Fatalf("ResolveApproval deny+scope status = %d, want 400", status2)
	}
}
