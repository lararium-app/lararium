package custos

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DanglingIntent represents an unresolved vault_mutation_intent.
type DanglingIntent struct {
	File   string
	Line   int
	Nonce  string
	Gen    int64
	Names  []string
	Reason string
}

// FindDanglingIntents scans audit logs and returns all uncommitted mutation intents.
func (a *AuditLogger) FindDanglingIntents() ([]DanglingIntent, error) {
	files, err := a.ListLogFiles()
	if err != nil {
		return nil, err
	}

	intents := make(map[string]DanglingIntent)
	var orderedNonces []string

	for _, fpath := range files {
		lines, err := readNonEmptyLines(fpath)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(fpath)
		for _, li := range lines {
			var rec AuditRecord
			if err := json.Unmarshal(li.raw, &rec); err != nil {
				continue
			}
			if rec.Nonce != "" {
				switch rec.Kind {
				case AuditKindVaultMutationIntent:
					if _, exists := intents[rec.Nonce]; !exists {
						orderedNonces = append(orderedNonces, rec.Nonce)
					}
					intents[rec.Nonce] = DanglingIntent{
						File:   base,
						Line:   li.num,
						Nonce:  rec.Nonce,
						Gen:    rec.Gen,
						Names:  rec.Names,
						Reason: rec.Reason,
					}
				case AuditKindVaultMutation, AuditKindVaultMutationRecovered,
					AuditKindVaultMutationAborted, AuditKindVaultMutationSuperseded:
					delete(intents, rec.Nonce)
				}
			}
		}
	}

	var result []DanglingIntent
	for _, nonce := range orderedNonces {
		if d, ok := intents[nonce]; ok {
			result = append(result, d)
		}
	}
	return result, nil
}

// quarantineInterruptedSurrogates handles the interrupted-change_passphrase
// crash window (CUSTOS-SPEC §11 Change law): surrogates.age was rewritten
// under the new passphrase but vault.age never swapped, so the passphrase
// that opens the vault cannot open it. The remnant is moved aside — the
// surrogate registry restarts empty (doors re-pair) instead of bricking
// every unlock under §6.6. Anything else keeps the corruption posture.
func quarantineInterruptedSurrogates(stateDir, passphrase string, intent DanglingIntent, audit *AuditLogger) {
	surPath := filepath.Join(stateDir, "surrogates.age")
	surData, err := os.ReadFile(surPath)
	if err != nil || len(surData) == 0 {
		return
	}
	if _, err := DecryptSurrogates(surData, passphrase); err == nil {
		return // opens cleanly: nothing interrupted
	}
	quarantined := filepath.Join(stateDir, fmt.Sprintf("surrogates.age.crashed-%d", intent.Gen))
	if err := os.Rename(surPath, quarantined); err != nil {
		return
	}
	_ = audit.Append(AuditRecord{
		Kind:   AuditKindVaultMutationRecovered,
		Nonce:  intent.Nonce,
		Gen:    intent.Gen,
		Actor:  "cli",
		Reason: "surrogates_quarantined",
	})
}

// RemoveStaleTemps unlinks any *.tmp files in stateDir per CUSTOS §4.2.
func RemoveStaleTemps(stateDir string) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
			// #nosec G703 -- e.Name() comes from os.ReadDir of stateDir:
			// a bare entry name, never path separators, cannot traverse.
			_ = os.Remove(filepath.Join(stateDir, e.Name()))
		}
	}
}

// ReemitEnvelopeMAC re-emits vault.age.mac from surviving vault.age envelope bytes per CUSTOS §4.2.
func ReemitEnvelopeMAC(stateDir string, instanceKey []byte) error {
	vaultAgePath := filepath.Join(stateDir, "vault.age")
	macPath := filepath.Join(stateDir, "vault.age.mac")

	envelopeBytes, err := os.ReadFile(vaultAgePath)
	if err != nil {
		return fmt.Errorf("read vault.age for mac reemit: %w", err)
	}

	newMAC := ComputeEnvelopeMAC(instanceKey, envelopeBytes)

	// Atomic write mac
	tmp, err := os.CreateTemp(stateDir, "vaultmac-reemit-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(newMAC + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, macPath)
}

// FirstUnlockRecovery performs landedness recovery under flock per CUSTOS-SPEC §4.2.
// Caller holds custos.lock. Decrypts under passphrase specifically to inspect generation.
func FirstUnlockRecovery(stateDir string, instanceKey []byte, passphrase string, audit *AuditLogger) (*VaultDoc, error) {
	dangling, err := audit.FindDanglingIntents()
	if err != nil {
		return nil, fmt.Errorf("find dangling intents: %w", err)
	}

	vaultAgePath := filepath.Join(stateDir, "vault.age")
	envelopeBytes, err := os.ReadFile(vaultAgePath)
	if err != nil {
		return nil, fmt.Errorf("read vault.age: %w", err)
	}

	if len(dangling) == 0 {
		// Strict unlock check per CUSTOS §4.1: MAC verify runs after first-unlock recovery
		macBytes, err := os.ReadFile(filepath.Join(stateDir, "vault.age.mac"))
		if err != nil {
			return nil, ErrVaultEnvelopeCorrupt
		}
		expectedMAC := strings.TrimSpace(string(macBytes))
		if !VerifyEnvelopeMAC(instanceKey, envelopeBytes, expectedMAC) {
			return nil, ErrVaultEnvelopeCorrupt
		}

		// Decrypt vault
		docBytes, err := DecryptAge(envelopeBytes, passphrase)
		if err != nil {
			return nil, err
		}
		var doc VaultDoc
		if err := json.Unmarshal(docBytes, &doc); err != nil {
			return nil, fmt.Errorf("unmarshal vault doc: %w", err)
		}
		return &doc, nil
	}

	// CUSTOS-SPEC §4.2 landedness doctrine:
	// Decrypt specifically to inspect the generation
	docBytes, err := DecryptAge(envelopeBytes, passphrase)
	if err != nil {
		// CUSTOS-SPEC §4.1: If envelope is tampered (MAC mismatch), report ErrVaultEnvelopeCorrupt
		macBytes, macErr := os.ReadFile(filepath.Join(stateDir, "vault.age.mac"))
		if macErr == nil {
			expectedMAC := strings.TrimSpace(string(macBytes))
			if !VerifyEnvelopeMAC(instanceKey, envelopeBytes, expectedMAC) {
				return nil, ErrVaultEnvelopeCorrupt
			}
		}
		// Wrong passphrase returns directly without modifying state
		return nil, err
	}
	var doc VaultDoc
	if err := json.Unmarshal(docBytes, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal vault doc: %w", err)
	}

	// CUSTOS-SPEC §11 (Change law): an uncommitted change_passphrase
	// intent explains a surrogates.age that the current passphrase
	// cannot open — the crash sat between the surrogates rewrite and
	// the envelope swap. Quarantine the remnant so unlock proceeds with
	// an empty registry (doors re-pair, secrets survive); genuine
	// corruption keeps the §6.6 abort posture instead.
	for _, intent := range dangling {
		if intent.Reason == "change_passphrase" {
			quarantineInterruptedSurrogates(stateDir, passphrase, intent, audit)
		}
	}

	resolvedAny := false

	// CUSTOS §4.2: for each dangling intent decide landed vs not
	for _, intent := range dangling {
		switch {
		case doc.Generation == intent.Gen:
			// Landed: append vault_mutation_recovered (nonce-bearing) and re-emit vault.age.mac if it lags
			_ = audit.Append(AuditRecord{
				Kind:  AuditKindVaultMutationRecovered,
				Nonce: intent.Nonce,
				Gen:   intent.Gen,
				Names: intent.Names,
				Actor: "cli",
			})
			_ = ReemitEnvelopeMAC(stateDir, instanceKey)
			resolvedAny = true

		case doc.Generation < intent.Gen:
			// Not landed: append vault_mutation_aborted reason crashed_pre_rename, unlink stale temps, re-emit mac
			_ = audit.Append(AuditRecord{
				Kind:   AuditKindVaultMutationAborted,
				Nonce:  intent.Nonce,
				Gen:    intent.Gen,
				Names:  intent.Names,
				Actor:  "cli",
				Reason: "crashed_pre_rename",
			})
			RemoveStaleTemps(stateDir)
			_ = ReemitEnvelopeMAC(stateDir, instanceKey)
			resolvedAny = true

		case doc.Generation > intent.Gen:
			// Persisted generation is greater: superseded
			_ = audit.Append(AuditRecord{
				Kind:  AuditKindVaultMutationSuperseded,
				Nonce: intent.Nonce,
				Gen:   intent.Gen,
				Names: intent.Names,
				Actor: "cli",
			})
			resolvedAny = true
		}
	}

	// CUSTOS §4.2: recovery's own vault rewrite runs its own intent/commit pair marked recovery
	// and recomputes the fingerprint mirror inside the recovery rewrite (§4.6).
	if resolvedAny {
		recoveryNonceBytes := make([]byte, 16)
		_, _ = rand.Read(recoveryNonceBytes)
		recoveryNonce := "recovery-" + hex.EncodeToString(recoveryNonceBytes)

		// 1. Audit recovery intent
		_ = audit.Append(AuditRecord{
			Kind:   AuditKindVaultMutationIntent,
			Nonce:  recoveryNonce,
			Gen:    doc.Generation,
			Actor:  "cli",
			Reason: "recovery",
		})

		// 2. Recompute fingerprint mirror per CUSTOS §4.6
		_ = WriteFingerprints(stateDir, instanceKey, doc.Credentials)

		// 3. Re-emit envelope MAC
		_ = ReemitEnvelopeMAC(stateDir, instanceKey)

		// 4. Audit recovery commit
		_ = audit.Append(AuditRecord{
			Kind:   AuditKindVaultMutation,
			Nonce:  recoveryNonce,
			Gen:    doc.Generation,
			Actor:  "cli",
			Reason: "recovery",
		})
	}

	// CUSTOS §4.1: strict MAC verification runs after first-unlock recovery
	macBytes, err := os.ReadFile(filepath.Join(stateDir, "vault.age.mac"))
	if err != nil {
		return nil, ErrVaultEnvelopeCorrupt
	}
	expectedMAC := strings.TrimSpace(string(macBytes))
	freshEnvelope, err := os.ReadFile(vaultAgePath)
	if err != nil || !VerifyEnvelopeMAC(instanceKey, freshEnvelope, expectedMAC) {
		return nil, ErrVaultEnvelopeCorrupt
	}

	return &doc, nil
}
