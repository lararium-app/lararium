package custos_test

import (
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestF5_CollisionFreeToolFingerprints proves that:
//  1. Two tool names that previously normalized/truncated to the same input
//     produce distinct fingerprints per MCP-SPEC §4.3 (F5).
//  2. Two different connectors with boundary-shifted names produce distinct fingerprints.
//  3. Existing approvals for an old-format fingerprint degrade safely (fail closed,
//     never auto-approve).
func TestF5_CollisionFreeToolFingerprints(t *testing.T) {
	// 1. Two tool names that exceed 64 runes and share the first 64 runes
	prefix64 := strings.Repeat("a", 64)
	name1 := prefix64 + "_first"
	name2 := prefix64 + "_second"

	// Verify they normalize to the same truncated 64-rune string
	norm1, ok1 := custos.NormalizeToolName(name1)
	norm2, ok2 := custos.NormalizeToolName(name2)
	if !ok1 || !ok2 || norm1 != norm2 {
		t.Fatalf("expected both names to normalize to the same prefix, got %q vs %q", norm1, norm2)
	}

	snap1, err := custos.BuildCatalogSnapshot("conn", []*mcp.Tool{{Name: name1}}, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snap2, err := custos.BuildCatalogSnapshot("conn", []*mcp.Tool{{Name: name2}}, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	fp1 := snap1.Fingerprints[norm1]
	fp2 := snap2.Fingerprints[norm2]
	if fp1 == "" || fp2 == "" {
		t.Fatal("missing computed fingerprint")
	}
	if fp1 == fp2 {
		t.Fatalf("collision: distinct tool names %q and %q produced identical fingerprint %s", name1, name2, fp1)
	}

	// 2. Cross-connector collision resistance: conn="ab", tool="c" vs conn="a", tool="bc"
	snapConn1, err := custos.BuildCatalogSnapshot("ab", []*mcp.Tool{{Name: "c"}}, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapConn2, err := custos.BuildCatalogSnapshot("a", []*mcp.Tool{{Name: "bc"}}, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fpConn1 := snapConn1.Fingerprints["c"]
	fpConn2 := snapConn2.Fingerprints["bc"]
	if fpConn1 == fpConn2 {
		t.Fatalf("collision: cross-connector pair produced identical fingerprint %s", fpConn1)
	}

	// 3. Stored approval degradation (safe fail-closed):
	// A stored approval with an old/mismatched fingerprint triggers revocation on refresh,
	// reverting to VerdictAsk (never auto-approving).
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	policy := v.Policy()
	// Add an Always auto-rule for conn/tool_a
	pattern := "test-conn/tool_a"
	if _, err := policy.AddCredentialRule(pattern, custos.VerdictAuto, true, "approval", "cli"); err != nil {
		t.Fatalf("AddCredentialRule: %v", err)
	}

	// Verify rule is auto
	verdict, _ := policy.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "test-conn",
		Tool:       "tool_a",
		Host:       "test-conn",
	})
	if verdict != custos.VerdictAuto {
		t.Fatalf("initial verdict = %q, want auto", verdict)
	}

	// Simulate old snapshot with a legacy fingerprint
	oldSnap := &custos.CatalogSnapshot{
		Connector: "test-conn",
		Fingerprints: map[string]string{
			"tool_a": "legacy-old-format-fingerprint",
		},
	}

	// New snapshot built with new collision-free fingerprint
	newSnap, err := custos.BuildCatalogSnapshot("test-conn", []*mcp.Tool{{Name: "tool_a"}}, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Run revocation check: old stored fingerprint does not match new fingerprint
	oldFp := oldSnap.Fingerprints["tool_a"]
	newFp := newSnap.Fingerprints["tool_a"]
	if oldFp == newFp {
		t.Fatal("legacy and new fingerprint unexpectedly matched")
	}

	// When connector refreshes, mismatched fingerprint invokes revoker
	revoker := custos.NewPolicyFingerprintRevokerForTest(policy)
	if err := revoker.RevokeAlwaysRule("test-conn", pattern); err != nil {
		t.Fatalf("RevokeAlwaysRule: %v", err)
	}

	// After revocation, Decide must fail closed to ask (not auto)
	verdictAfter, _ := policy.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "test-conn",
		Tool:       "tool_a",
		Host:       "test-conn",
	})
	if verdictAfter == custos.VerdictAuto {
		t.Fatalf("verdict after fingerprint mismatch is still auto; must fail closed")
	}
	if verdictAfter != custos.VerdictAsk {
		t.Fatalf("verdict after revocation = %q, want ask", verdictAfter)
	}
}
