package custos_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// M22: custos/vault.go ChangePassphrase: extend the M9 discipline — check write
// and sync errors at EVERY step (new vault write, surrogates re-encrypt, renames,
// dir fsyncs); on any mid-window failure before renames complete, restore/abort per
// the M9 Change law (CUSTOS-SPEC §4.2), and make the crash-recovery quarantine
// cover the truncated-file window introduced by disk-full.
// Test: a failing writer after surrogates re-encrypted -> vault remains openable
// with the OLD passphrase (registry quarantine path).
func TestM22_ChangePassphraseFailingWriter(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	m9AddCred(t, v, pass, "m22-cred")
	if _, err := v.AddSurrogate(pass, "m22-cred", "api.openai.com", 443, "", false, "test"); err != nil {
		t.Fatal(err)
	}

	// Inject a failing writer after surrogates are re-encrypted
	custos.SetPayloadWriterHookForTest(func(w io.Writer, p []byte) (int, error) {
		return 0, errors.New("simulated disk-full error on envelope write")
	})
	defer custos.SetPayloadWriterHookForTest(nil)

	err := v.ChangePassphrase(pass, "new-pass-rotation")
	if err == nil {
		t.Fatal("ChangePassphrase must fail when envelope writer fails")
	}

	custos.SetPayloadWriterHookForTest(nil)

	// Precondition: vault.age still on old envelope, surrogates.age re-encrypted under new passphrase.
	// Unlock under OLD passphrase must recover via registry quarantine path:
	// quarantine remnant, vault readable, registry restarts empty.
	vCheck := custos.NewVault(stateDir, 5*time.Second)
	if err := vCheck.Unlock(pass, false); err != nil {
		t.Fatalf("unlock old passphrase after failed writer: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(stateDir, "surrogates.age.crashed-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one quarantined surrogates remnant, got %v (err %v)", matches, err)
	}

	surs, err := vCheck.ListSurrogates()
	if err != nil {
		t.Fatalf("ListSurrogates: %v", err)
	}
	if len(surs) != 0 {
		t.Errorf("registry must restart empty after quarantine, got %+v", surs)
	}

	// Credentials survive
	if _, ok := vCheck.GetCredential("m22-cred"); !ok {
		t.Error("credential m22-cred lost through interrupted change")
	}
}

// TestM22_TruncatedSurrogatesQuarantinedOnDiskFull verifies that a zero-length or
// truncated surrogates.age left by disk-full during an interrupted change_passphrase
// is quarantined rather than bricking unlock.
func TestM22_TruncatedSurrogatesQuarantinedOnDiskFull(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	m9AddCred(t, v, pass, "m22-trunc-cred")

	// Plant a dangling change_passphrase intent and a 0-byte truncated surrogates.age
	audit := custos.NewAuditLogger(stateDir)
	if err := audit.Append(custos.AuditRecord{
		Kind:   custos.AuditKindVaultMutationIntent,
		Nonce:  "test-nonce-truncated",
		Gen:    2,
		Actor:  "cli",
		Reason: "change_passphrase",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "surrogates.age"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	vCheck := custos.NewVault(stateDir, 5*time.Second)
	if err := vCheck.Unlock(pass, false); err != nil {
		t.Fatalf("unlock old passphrase with truncated surrogates.age: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(stateDir, "surrogates.age.crashed-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one quarantined surrogates remnant, got %v (err %v)", matches, err)
	}

	if _, ok := vCheck.GetCredential("m22-trunc-cred"); !ok {
		t.Error("credential m22-trunc-cred lost through interrupted change")
	}
}
