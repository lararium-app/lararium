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

// policySourceAdapter adapts *custos.Vault to surface.PolicySource.
type policySourceAdapter struct {
	vault *custos.Vault
}

func newPolicySourceAdapter(vault *custos.Vault) surface.PolicySource {
	return &policySourceAdapter{vault: vault}
}

var _ surface.PolicySource = (*policySourceAdapter)(nil)

// wireCustosPolicy attaches a policySourceAdapter to srv if the custos vault exists on disk.
func wireCustosPolicy(srv *surface.Server, home string, cfg CustosConfig) {
	if srv == nil {
		return
	}
	stateDir := ""
	if cfg.DoorsSock != "" {
		stateDir = filepath.Dir(cfg.DoorsSock)
	} else if home != "" {
		stateDir = filepath.Join(home, "custos")
	}
	if stateDir == "" {
		return
	}
	v := custos.NewVault(stateDir, 10*time.Second)
	if !v.Exists() {
		return
	}
	if cfg.UnlockKeyfile != "" {
		if keyBytes, err := os.ReadFile(cfg.UnlockKeyfile); err == nil {
			passphrase := strings.TrimSpace(string(keyBytes))
			if passphrase != "" {
				_ = v.Unlock(passphrase, true)
			}
		}
	}
	srv.SetPolicySource(newPolicySourceAdapter(v))
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
