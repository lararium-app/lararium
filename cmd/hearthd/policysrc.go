package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// policySourceAdapter adapts *custos.Vault to surface.PolicySource and PolicyDecider.
type policySourceAdapter struct {
	vault *custos.Vault
}

func newPolicySourceAdapter(vault *custos.Vault) surface.PolicySource {
	return &policySourceAdapter{vault: vault}
}

var (
	_ surface.PolicySource = (*policySourceAdapter)(nil)
	_ PolicyDecider        = (*policySourceAdapter)(nil)
)

// wireCustosPolicy attaches a policySourceAdapter to srv if the custos vault exists on disk.
func wireCustosPolicy(srv *surface.Server, home string, cfg CustosConfig) PolicyDecider {
	if srv == nil {
		return nil
	}
	v := openCustosVault(home, cfg)
	if v == nil {
		return nil
	}
	adapter := &policySourceAdapter{vault: v}
	srv.SetPolicySource(adapter)
	return adapter
}

// Needles exposes the vault's redaction needles (CREDHANDLE-SPEC R3) to the
// loop's Secrets seam; nil while the vault is locked.
func (a *policySourceAdapter) Needles() []string {
	if a == nil || a.vault == nil {
		return nil
	}
	return a.vault.Needles()
}

// openCustosVault opens the on-disk custody vault (nil when absent) and
// unlocks it from the configured keyfile when one is set.
func openCustosVault(home string, cfg CustosConfig) *custos.Vault {
	stateDir := ""
	if cfg.DoorsSock != "" {
		stateDir = filepath.Dir(cfg.DoorsSock)
	} else if home != "" {
		stateDir = filepath.Join(home, "custos")
	}
	if stateDir == "" {
		return nil
	}
	v := custos.NewVault(stateDir, 10*time.Second)
	if !v.Exists() {
		return nil
	}
	if cfg.UnlockKeyfile != "" {
		if keyBytes, err := os.ReadFile(cfg.UnlockKeyfile); err == nil {
			passphrase := strings.TrimSpace(string(keyBytes))
			if passphrase != "" {
				_ = v.Unlock(passphrase, true)
			}
		}
	}
	return v
}

// wrapPolicyErr joins surface.ErrPolicyCorrupt with custos.ErrPolicyCorrupt so errors.Is matches either sentinel across the package boundary.
func wrapPolicyErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, custos.ErrPolicyCorrupt) {
		return fmt.Errorf("%w: %w", surface.ErrPolicyCorrupt, err)
	}
	return err
}

func (a *policySourceAdapter) ListRules() ([]surface.PolicyRule, error) {
	if a.vault == nil || !a.vault.IsUnlocked() {
		return nil, surface.ErrPolicyLocked
	}
	pe := a.vault.Policy()
	if pe == nil {
		return nil, surface.ErrPolicyLocked
	}
	if pe.IsCorrupt() {
		return nil, wrapPolicyErr(custos.ErrPolicyCorrupt)
	}
	rows := pe.ListRules()
	rules := make([]surface.PolicyRule, len(rows))
	for i, r := range rows {
		rules[i] = surface.PolicyRule{
			Lane:    r.Lane,
			Pattern: r.Pattern,
			Verdict: r.Verdict,
			Always:  r.Always,
			Source:  r.Source,
		}
	}
	return rules, nil
}

func (a *policySourceAdapter) EgressStrict() bool {
	if a.vault == nil || !a.vault.IsUnlocked() {
		return false
	}
	pe := a.vault.Policy()
	if pe == nil {
		return false
	}
	return pe.EgressStrict()
}

// AddRule mirrors ctl POLICY-ADD exactly: lane "egress" -> AddEgressRule,
// anything else -> AddCredentialRule. Patterns are passed to the engine
// unmodified — canonical-form enforcement (§6.3) is the engine's job.
func (a *policySourceAdapter) AddRule(lane, pattern, verdict string) ([]string, error) {
	if a.vault == nil || !a.vault.IsUnlocked() {
		return nil, surface.ErrPolicyLocked
	}
	pe := a.vault.Policy()
	if pe == nil {
		return nil, surface.ErrPolicyLocked
	}
	var notes []string
	var err error
	if lane == "egress" {
		notes, err = pe.AddEgressRule(pattern, verdict, false, "web", "web")
	} else {
		notes, err = pe.AddCredentialRule(pattern, verdict, false, "web", "web")
	}
	if err != nil {
		return nil, wrapPolicyErr(err)
	}
	return notes, nil
}

func (a *policySourceAdapter) RemoveRule(lane, pattern string) (bool, error) {
	if a.vault == nil || !a.vault.IsUnlocked() {
		return false, surface.ErrPolicyLocked
	}
	pe := a.vault.Policy()
	if pe == nil {
		return false, surface.ErrPolicyLocked
	}
	removed, err := pe.RemoveRule(lane, pattern, "web")
	if err != nil {
		return false, wrapPolicyErr(err)
	}
	return removed, nil
}

// Decide evaluates the policy verdict for an MCP tool call (issue #129).
// Worker lane semantics: credential=connector, tool=tool.
func (a *policySourceAdapter) Decide(connector, tool string) string {
	if a == nil || a.vault == nil {
		return custos.VerdictAsk
	}
	pe := a.vault.Policy()
	if pe == nil {
		return custos.VerdictAsk
	}
	verdict, _ := pe.Decide(custos.DecideInput{
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

// AddAlwaysRule mints a card-minted always-rule (issue #132;
// CUSTOS-SPEC §6.5): credential-lane exact pattern "<connector>/<tool>",
// verdict auto, always=true, source "always" — mirroring the door path's
// AddCredentialRule(pat, "auto", true, "always", actor). Once present,
// Decide returns auto for that call and the approval card never
// re-raises; the rule is visible via GET /v1/policy/rules (always:true)
// and removable only through policy controls, never page-settable
// (SURFACE-SPEC §4 CA-7 posture preserved).
func (a *policySourceAdapter) AddAlwaysRule(pattern string) error {
	if a == nil || a.vault == nil || !a.vault.IsUnlocked() {
		return surface.ErrPolicyLocked
	}
	pe := a.vault.Policy()
	if pe == nil {
		return surface.ErrPolicyLocked
	}
	_, err := pe.AddCredentialRule(pattern, "auto", true, "always", "custos")
	return wrapPolicyErr(err)
}
