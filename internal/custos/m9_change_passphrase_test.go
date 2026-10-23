package custos_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// M9: ChangePassphrase must never leave vault.age and surrogates.age under
// different passphrases. The re-encryption of surrogates precedes every
// rename and fails loudly (zero-mutation abort), and a crash inside the
// change window is closed by first-unlock: the uncommitted
// change_passphrase intent explains a surrogates.age the old passphrase
// cannot open, so the remnant is quarantined instead of bricking unlock.

func m9AddCred(t *testing.T, v *custos.Vault, pass, name string) {
	t.Helper()
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials[name] = custos.Credential{Kind: "api_key", Secret: "sekret-" + name}
		return []string{name}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate add %s: %v", name, err)
	}
}

// (a) Happy path: surrogates land under the new passphrase and are
// unreadable under the old one.
func TestM9_ChangePassphraseReencryptsSurrogates(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	m9AddCred(t, v, pass, "m9-cred")
	if _, err := v.AddSurrogate(pass, "m9-cred", "api.openai.com", 443, "", false, "test"); err != nil {
		t.Fatal(err)
	}

	newPass := "rotated-passphrase-5678"
	if err := v.ChangePassphrase(pass, newPass); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}

	surData, err := os.ReadFile(filepath.Join(stateDir, "surrogates.age"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := custos.DecryptSurrogates(surData, newPass); err != nil {
		t.Errorf("surrogates must open under new passphrase: %v", err)
	}
	if _, err := custos.DecryptSurrogates(surData, pass); err == nil {
		t.Error("surrogates must NOT open under old passphrase after rotation")
	}

	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(newPass, false); err != nil {
		t.Fatalf("unlock under new passphrase: %v", err)
	}
	surs, err := v2.ListSurrogates()
	if err != nil {
		t.Fatal(err)
	}
	if len(surs) != 1 || surs[0].Credential != "m9-cred" {
		t.Errorf("surrogates after rotation = %+v, want one bound to m9-cred", surs)
	}
}

// (b) Crash window kill (after_surrogates_reencrypt): vault.age still old,
// surrogates.age already new, dangling intent open. Unlock under the OLD
// passphrase must recover: quarantine the remnant, vault readable, registry
// restarts empty — never a lockout.
func TestM9_CrashBetweenSurrogatesAndEnvelope(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	m9AddCred(t, v, pass, "m9-pre")

	runCPSubprocessWithKill(t, stateDir, pass, "m9-new-pass", "after_surrogates_reencrypt")
	// Precondition for the window: vault.age is still the old envelope.
	vCheck := custos.NewVault(stateDir, 5*time.Second)
	if err := vCheck.Unlock(pass, false); err != nil {
		t.Fatalf("unlock old passphrase after mid-change crash: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(stateDir, "surrogates.age.crashed-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one quarantined surrogates remnant, got %v (err %v)", matches, err)
	}

	surs, err := vCheck.ListSurrogates()
	if err != nil {
		t.Fatalf("ListSurrogates after quarantine: %v", err)
	}
	if len(surs) != 0 {
		t.Errorf("registry must restart empty after quarantine, got %+v", surs)
	}

	// The intent must be resolved (no dangling change_passphrase left).
	audit := custos.NewAuditLogger(stateDir)
	dangling, err := audit.FindDanglingIntents()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dangling {
		if d.Reason == "change_passphrase" {
			t.Errorf("dangling change_passphrase intent left open: %+v", d)
		}
	}

	// Credentials survive every window of the change.
	if _, ok := vCheck.GetCredential("m9-pre"); !ok {
		t.Error("credential m9-pre lost through interrupted change")
	}
}

// (c) A surrogates.age that the old passphrase cannot open aborts the
// change with zero mutation of vault.age (no rename ever ran).
func TestM9_ChangeAbortsOnUndecryptableSurrogates(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	// Simulate a corrupt/foreign registry under a foreign key.
	foreign := "not-the-vault-passphrase"
	doc := &custos.SurrogateDoc{Version: 1, Surrogates: map[string]custos.SurrogateRecord{}}
	ct, err := custos.EncryptSurrogates(doc, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "surrogates.age"), ct, 0o600); err != nil {
		t.Fatal(err)
	}

	vaultBefore, err := os.ReadFile(filepath.Join(stateDir, "vault.age"))
	if err != nil {
		t.Fatal(err)
	}

	if err := v.ChangePassphrase(pass, "unreachable-new-pass"); err == nil {
		t.Fatal("ChangePassphrase must refuse when surrogates cannot be re-encrypted")
	}

	vaultAfter, err := os.ReadFile(filepath.Join(stateDir, "vault.age"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vaultBefore, vaultAfter) {
		t.Error("vault.age mutated despite aborted change")
	}
	if _, err := os.ReadFile(filepath.Join(stateDir, "surrogates.age")); err != nil {
		t.Errorf("surrogates.age must be untouched on abort: %v", err)
	}

	// Posture note: an undecryptable surrogates.age locks every unlock
	// per CUSTOS §6.6 (frozen S9 — remedy is restore), which is exactly
	// why the change must refuse instead of silently carrying the stale
	// registry forward. The abort leaves that state untouched.
	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(pass, false); err == nil {
		t.Error("§6.6: corrupt surrogates must keep the vault locked")
	}
	// The refusal did not rewrite anything further.
	vaultFinal, err := os.ReadFile(filepath.Join(stateDir, "vault.age"))
	if err != nil || !bytes.Equal(vaultBefore, vaultFinal) {
		t.Error("vault.age mutated after abort + locked unlock")
	}
}
