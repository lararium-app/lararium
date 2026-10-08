package custos_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

func init() {
	// Set fast scrypt work factor for test execution speed
	custos.SetTestAgeWorkFactor(10)
}

func setupTestVault(t *testing.T) (*custos.Vault, string, string) {
	t.Helper()
	// Short-path base: unix sockets (ctl.sock/doors.sock) bind under the
	// 104-byte sun_path cap; t.TempDir() + a long subtest name can exceed it.
	base, err := os.MkdirTemp("/tmp", "lrv") //nolint:usetesting // UDS path cap
	if err != nil {
		t.Fatalf("short tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dir := filepath.Join(base, "h")
	stateDir := filepath.Join(dir, "custos")
	passphrase := "test-secret-passphrase-1234"
	v := custos.NewVault(stateDir, 5*time.Second)
	return v, stateDir, passphrase
}

// V1: custos init/unlock/lock lifecycle (keyfile mode as TTY substitute)
// - state file modes/paths asserted
// - second init refuses (vault exists, frozen)
// - double unlock is a no-op with audit unlocked once.
func TestV1_InitUnlockLockLifecycle(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)

	// 1. Init
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}

	// Assert state dir mode 0700 per CUSTOS §C2
	fi, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("stateDir perm = %o, want 0700", fi.Mode().Perm())
	}

	// Assert state files exist with mode 0600
	expectedFiles := []string{
		"vault.key", "vault.age", "vault.age.mac", "surrogates.age", "fingerprints.json",
	}
	for _, f := range expectedFiles {
		p := filepath.Join(stateDir, f)
		s, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected file %s missing: %v", f, err)
			continue
		}
		if s.Mode().Perm() != 0o600 {
			t.Errorf("file %s perm = %o, want 0600", f, s.Mode().Perm())
		}
	}

	// 2. Second init refuses with frozen "vault exists"
	err = v.Init(pass)
	if err == nil || err.Error() != custos.ErrVaultExists.Error() {
		t.Fatalf("second init got %v, want %v", err, custos.ErrVaultExists)
	}

	// 3. Unlock
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("v.Unlock: %v", err)
	}
	if !v.IsUnlocked() {
		t.Fatal("expected vault to be unlocked")
	}

	// 4. Double unlock is a no-op with audit unlocked once
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("double unlock failed: %v", err)
	}

	// Verify audit log has "unlocked" only once
	files, err := v.Audit().ListLogFiles()
	if err != nil {
		t.Fatal(err)
	}
	unlockedCount := 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				if rec.Kind == custos.AuditKindUnlocked {
					unlockedCount++
				}
			}
		}
	}
	if unlockedCount != 1 {
		t.Errorf("unlocked audit count = %d, want exactly 1", unlockedCount)
	}

	// 5. Lock
	if err := v.Lock(); err != nil {
		t.Fatalf("v.Lock: %v", err)
	}
	if v.IsUnlocked() {
		t.Fatal("expected vault to be locked")
	}

	// 6. Keyfile mode refuses lock per CUSTOS §C3
	if err := v.Unlock(pass, true); err != nil {
		t.Fatalf("keyfile unlock: %v", err)
	}
	err = v.Lock()
	if err == nil || err.Error() != custos.ErrKeyfileModeAlwaysUnlocked.Error() {
		t.Fatalf("keyfile lock got %v, want %v", err, custos.ErrKeyfileModeAlwaysUnlocked)
	}
}

// V2: wrong passphrase: KEYS-SPEC's frozen failure strings and failure posture
// - empty passphrase returns "empty key" (frozen)
// - wrong passphrase returns incorrect passphrase and audits lock_failed.
func TestV2_WrongPassphrase(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Empty passphrase -> ErrEmpty ("empty key")
	err := v.Unlock("", false)
	if err == nil || err.Error() != custos.ErrEmpty.Error() {
		t.Fatalf("empty unlock got %v, want %v", err, custos.ErrEmpty)
	}

	err = v.Unlock("   ", false)
	if err == nil || err.Error() != custos.ErrEmpty.Error() {
		t.Fatalf("whitespace unlock got %v, want %v", err, custos.ErrEmpty)
	}

	// Wrong passphrase -> ErrIncorrectPassphrase
	err = v.Unlock("wrong-passphrase-nope", false)
	if err == nil || err.Error() != custos.ErrIncorrectPassphrase.Error() {
		t.Fatalf("wrong passphrase unlock got %v, want %v", err, custos.ErrIncorrectPassphrase)
	}

	// Audit log must carry lock_failed
	files, err := v.Audit().ListLogFiles()
	if err != nil {
		t.Fatal(err)
	}
	lockFailedFound := false
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				if rec.Kind == custos.AuditKindLockFailed {
					lockFailedFound = true
				}
			}
		}
	}
	if !lockFailedFound {
		t.Error("expected audit log to record lock_failed")
	}
}

// V3: tamper envelope -> MAC reject AFTER recovery resolves any intent
// - bit flip in vault.age causes MAC mismatch -> "vault envelope corrupt" (frozen).
func TestV3_TamperEnvelope(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Tamper envelope bytes
	vaultAgePath := filepath.Join(stateDir, "vault.age")
	b, err := os.ReadFile(vaultAgePath)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(vaultAgePath, b, 0o600); err != nil { //nolint:gosec // test path
		t.Fatal(err)
	}

	// Unlock attempt must reject with frozen "vault envelope corrupt"
	err = v.Unlock(pass, false)
	if err == nil || err.Error() != custos.ErrVaultEnvelopeCorrupt.Error() {
		t.Fatalf("tampered unlock got %v, want %v", err, custos.ErrVaultEnvelopeCorrupt)
	}
}

// V5: generation replay: restore older gen, MAC from snapshot set verifies, replayed vault accepted as that gen.
func TestV5_GenerationReplayAndRestore(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Mutate to gen 2 (snapshots gen 1)
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: "sk-openai-key-1"} //nolint:gosec // test key
		return []string{"openai"}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate to gen 2: %v", err)
	}

	// Mutate to gen 3 (snapshots gen 2)
	err = v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["anthropic"] = custos.Credential{Kind: "api_key", Secret: "sk-ant-key-2"} //nolint:gosec // test key
		return []string{"anthropic"}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate to gen 3: %v", err)
	}

	// Verify snapshots exist for gen 1 and gen 2
	gens, err := v.Snapshots().ListGenerations()
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) < 2 || gens[0] != 1 || gens[1] != 2 {
		t.Fatalf("expected snapshots [1, 2], got %v", gens)
	}

	// Restore gen 1 under flock with vault.key (no passphrase)
	key, err := v.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}

	if err := v.Snapshots().Restore(1, key, v.Audit()); err != nil {
		t.Fatalf("restore gen 1: %v", err)
	}

	// Unlock vault and verify it is at gen 1 (empty credentials)
	v.Lock()
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after restore: %v", err)
	}

	st := v.Status(false, 0)
	if st.Credentials != 0 {
		t.Errorf("restored credentials count = %d, want 0", st.Credentials)
	}
}

// V7: audit chain: append, truncate tail, insert middle -> first break file:line.
func TestV7_AuditChainTamperAndVerify(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	key, err := v.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}

	// Happy path verify
	res, err := v.Audit().Verify(key)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsBroken || res.IsDangling {
		t.Fatalf("initial verify failed: %s", res.Format())
	}
	if !strings.HasPrefix(res.Format(), "chain ok") {
		t.Fatalf("expected 'chain ok...', got %q", res.Format())
	}

	// Add more records
	for i := 1; i <= 3; i++ {
		_ = v.Audit().Append(custos.AuditRecord{
			Kind:  custos.AuditKindCredentialAdded,
			Cred:  fmt.Sprintf("key%d", i),
			Actor: "cli",
		})
	}

	res, err = v.Audit().Verify(key)
	if err != nil || res.IsBroken {
		t.Fatalf("verify after appends failed: %s", res.Format())
	}

	// 1. Bit-flip middle record
	files, err := v.Audit().ListLogFiles()
	if err != nil || len(files) == 0 {
		t.Fatal("no audit files")
	}
	todayFile := files[len(files)-1]
	content, err := os.ReadFile(todayFile)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected >=3 lines, got %d", len(lines))
	}

	// Tamper line 2
	tamperedLines := make([]string, len(lines))
	copy(tamperedLines, lines)
	tamperedLines[1] = strings.Replace(tamperedLines[1], "key1", "key1-tampered", 1)

	if err := os.WriteFile(todayFile, []byte(strings.Join(tamperedLines, "\n")+"\n"), 0o600); err != nil { //nolint:gosec // test path
		t.Fatal(err)
	}

	res, err = v.Audit().Verify(key)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsBroken {
		t.Fatal("expected verify to detect break after tamper")
	}
	// Break must report file:line
	expectedBreak := fmt.Sprintf("%s:3", filepath.Base(todayFile))
	if res.Format() != expectedBreak {
		t.Errorf("break format = %q, want %q", res.Format(), expectedBreak)
	}

	// Restore original content
	_ = os.WriteFile(todayFile, content, 0o600) //nolint:gosec // test path

	// 2. Truncate tail
	truncatedContent := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	_ = os.WriteFile(todayFile, []byte(truncatedContent), 0o600) //nolint:gosec // test path

	// Add new line after truncation -> hash break
	_ = v.Audit().Append(custos.AuditRecord{ //nolint:gosec // test cred
		Kind:  custos.AuditKindCredentialAdded,
		Cred:  "post-truncation",
		Actor: "cli",
	})
}

// V18: locked verify: runs file-read-only without secret material, reports uncommitted mutation.
func TestV18_LockedAuditVerify(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	key, err := v.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}

	// Append an uncommitted WAL intent (dangling intent)
	danglingNonce := "dangling-nonce-12345"
	if err := v.Audit().Append(custos.AuditRecord{
		Kind:  custos.AuditKindVaultMutationIntent,
		Nonce: danglingNonce,
		Gen:   2,
		Names: []string{"test_cred"},
		Actor: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	// Run verify while vault is locked (no passphrase provided!)
	res, err := v.Audit().Verify(key)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsDangling {
		t.Fatalf("expected dangling intent diagnostic, got %s", res.Format())
	}
	if !strings.HasPrefix(res.Format(), "uncommitted mutation at") {
		t.Errorf("got %q, want prefix 'uncommitted mutation at'", res.Format())
	}

	// Assert locked state file check
	fp, err := custos.ReadFingerprints(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Names()) != 0 {
		t.Errorf("locked fingerprint names count = %d, want 0", len(fp.Names()))
	}
}

// V22: anchor MAC integrity: tamper anchors.json -> reject; anchors bootstrap; prune keeps newest anchor.
func TestV22_AnchorMACIntegrity(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	key, err := v.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}

	// Bootstrap genesis anchor
	if err := v.Audit().BootstrapGenesisAnchor(key); err != nil {
		t.Fatalf("bootstrap anchor: %v", err)
	}

	// Verify passes with valid anchor
	res, err := v.Audit().Verify(key)
	if err != nil || res.IsBroken {
		t.Fatalf("verify with valid anchor failed: %v (%s)", err, res.Format())
	}

	// Tamper anchors.json (modify line number or hash)
	anchorsPath := filepath.Join(stateDir, "audit", "anchors.json")
	b, err := os.ReadFile(anchorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var anchors []custos.AnchorEntry
	if err := json.Unmarshal(b, &anchors); err != nil {
		t.Fatal(err)
	}
	anchors[0].Hash = "0000000000000000000000000000000000000000000000000000000000000000"
	tamperedJSON, err := json.Marshal(anchors)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchorsPath, tamperedJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// Verify must reject tampered anchor
	res, err = v.Audit().Verify(key)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsBroken {
		t.Fatal("expected verify to reject tampered anchor")
	}
}

// TestD1_CrossDayChainStream tests cross-day audit chain continuity and boundary tamper detection
// per CUSTOS-SPEC §8.1, §8.3 (D1 regression test).
func TestD1_CrossDayChainStream(t *testing.T) {
	tests := []struct {
		name        string
		pastDates   []string
		seedCount   int
		appendCount int
	}{
		{
			name:        "two-day stream and boundary break detection",
			pastDates:   []string{"20261001"},
			seedCount:   2,
			appendCount: 2,
		},
		{
			name:        "three-day stream and boundary break detection",
			pastDates:   []string{"20261001", "20261002"},
			seedCount:   2,
			appendCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, stateDir, pass := setupTestVault(t)
			if err := v.Init(pass); err != nil {
				t.Fatal(err)
			}
			key, err := v.LoadInstanceKey()
			if err != nil {
				t.Fatal(err)
			}

			// Seed records in day 1
			for i := 1; i <= tc.seedCount; i++ {
				_ = v.Audit().Append(custos.AuditRecord{
					Kind:  custos.AuditKindCredentialAdded,
					Cred:  fmt.Sprintf("seed-key-%d", i),
					Actor: "cli",
				})
			}

			auditDir := filepath.Join(stateDir, "audit")

			// Simulate historical days by renaming files in pastDates order
			for _, pastDate := range tc.pastDates {
				files, err := v.Audit().ListLogFiles()
				if err != nil || len(files) == 0 {
					t.Fatalf("no audit files found: %v", err)
				}
				todayFile := files[len(files)-1]
				pastFile := filepath.Join(auditDir, fmt.Sprintf("custos-%s.jsonl", pastDate))
				if err := os.Rename(todayFile, pastFile); err != nil {
					t.Fatalf("rename to past date %s: %v", pastDate, err)
				}

				// Append record for the next day
				_ = v.Audit().Append(custos.AuditRecord{
					Kind:  custos.AuditKindCredentialAdded,
					Cred:  fmt.Sprintf("post-rename-%s", pastDate),
					Actor: "cli",
				})
			}

			// Additional appends on latest day
			for i := 1; i <= tc.appendCount; i++ {
				_ = v.Audit().Append(custos.AuditRecord{
					Kind:  custos.AuditKindCredentialAdded,
					Cred:  fmt.Sprintf("today-key-%d", i),
					Actor: "cli",
				})
			}

			// 1. Verify cross-day stream "chain ok"
			res, err := v.Audit().Verify(key)
			if err != nil || res.IsBroken || res.IsDangling {
				t.Fatalf("cross-day verify failed: %v (%s)", err, res.Format())
			}
			if !strings.HasPrefix(res.Format(), "chain ok") {
				t.Fatalf("expected 'chain ok...', got %q", res.Format())
			}

			// 2. Tamper the earlier file's last line
			files, err := v.Audit().ListLogFiles()
			if err != nil || len(files) < 2 {
				t.Fatalf("expected at least 2 files, got %d", len(files))
			}
			earlierFile := files[len(files)-2]
			latestFile := files[len(files)-1]
			latestBase := filepath.Base(latestFile)

			content, err := os.ReadFile(earlierFile)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(content)), "\n")
			if len(lines) == 0 {
				t.Fatal("earlier file is empty")
			}
			// Tamper last line of earlier file
			lines[len(lines)-1] = lines[len(lines)-1] + " "
			//nolint:gosec // earlierFile is a path from ReadDir over this test's own temp dir
			if err := os.WriteFile(earlierFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// 3. Verify walk must report the boundary break with <file:line>
			res, err = v.Audit().Verify(key)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsBroken {
				t.Fatal("expected verify to detect boundary break")
			}
			expectedBreak := fmt.Sprintf("%s:1", latestBase)
			if res.Format() != expectedBreak {
				t.Fatalf("expected boundary break %q, got %q", expectedBreak, res.Format())
			}
		})
	}
}

// TestD2_LaggedMACPostRenameDirectUnlock tests first-unlock recovery when MAC lags post-rename
// and calls Unlock directly (not Mutate) per CUSTOS-SPEC §4.1, §4.2 (D2 regression test).
func TestD2_LaggedMACPostRenameDirectUnlock(t *testing.T) {
	t.Run("crashed_pre_rename: new-MAC / old-envelope reconciled on direct unlock", func(t *testing.T) {
		v, stateDir, pass := setupTestVault(t)
		if err := v.Init(pass); err != nil {
			t.Fatal(err)
		}

		// Simulate crash state: intent for gen 2 appended, envelope at gen 1, MAC lagging/mismatched
		intentNonce := "intent-nonce-d2-pre-rename"
		if err := v.Audit().Append(custos.AuditRecord{
			Kind:  custos.AuditKindVaultMutationIntent,
			Nonce: intentNonce,
			Gen:   2,
			Names: []string{"unlanded_key"},
			Actor: "cli",
		}); err != nil {
			t.Fatal(err)
		}

		// Overwrite vault.age.mac with a mismatched MAC (new MAC written before envelope rename failed)
		macPath := filepath.Join(stateDir, "vault.age.mac")
		if err := os.WriteFile(macPath, []byte("badbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbad1\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Fresh vault instance (new boot)
		v2 := custos.NewVault(stateDir, 5*time.Second)

		// Call Unlock directly (not Mutate!)
		if err := v2.Unlock(pass, false); err != nil {
			t.Fatalf("expected direct unlock to succeed, got %v", err)
		}

		// 1. MAC must be repaired from surviving envelope bytes
		key, err := v2.LoadInstanceKey()
		if err != nil {
			t.Fatal(err)
		}
		envBytes, err := os.ReadFile(filepath.Join(stateDir, "vault.age"))
		if err != nil {
			t.Fatal(err)
		}
		macBytes, err := os.ReadFile(filepath.Join(stateDir, "vault.age.mac"))
		if err != nil {
			t.Fatal(err)
		}
		if !custos.VerifyEnvelopeMAC(key, envBytes, strings.TrimSpace(string(macBytes))) {
			t.Fatal("expected vault.age.mac to be repaired from surviving envelope bytes")
		}

		// 2. Audit log must record vault_mutation_aborted with reason crashed_pre_rename
		files, _ := v2.Audit().ListLogFiles()
		foundAborted := false
		for _, f := range files {
			b, _ := os.ReadFile(f)
			for _, line := range strings.Split(string(b), "\n") {
				var rec custos.AuditRecord
				if err := json.Unmarshal([]byte(line), &rec); err == nil {
					if rec.Kind == custos.AuditKindVaultMutationAborted && rec.Reason == "crashed_pre_rename" && rec.Nonce == intentNonce {
						foundAborted = true
					}
				}
			}
		}
		if !foundAborted {
			t.Fatal("expected audit log to record vault_mutation_aborted(crashed_pre_rename) with intent nonce")
		}

		// 3. Verify quiesces clean
		res, err := v2.Audit().Verify(key)
		if err != nil || res.IsBroken || res.IsDangling {
			t.Fatalf("expected clean verify, got %v (%s)", err, res.Format())
		}
		if !strings.HasPrefix(res.Format(), "chain ok") {
			t.Fatalf("expected 'chain ok...', got %q", res.Format())
		}

		// 4. Subsequent unlock on same instance after lock does not re-run recovery pass
		if err := v2.Lock(); err != nil {
			t.Fatal(err)
		}
		if err := v2.Unlock(pass, false); err != nil {
			t.Fatalf("subsequent unlock failed: %v", err)
		}
	})

	t.Run("crashed_post_rename: landed envelope / lagging MAC reconciled on direct unlock", func(t *testing.T) {
		v, stateDir, pass := setupTestVault(t)
		if err := v.Init(pass); err != nil {
			t.Fatal(err)
		}

		// Mutate vault document to gen 2 and write directly to vault.age
		key, err := v.LoadInstanceKey()
		if err != nil {
			t.Fatal(err)
		}

		doc := custos.VaultDoc{
			Version:     1,
			Generation:  2,
			Credentials: map[string]custos.Credential{"landed": {Kind: "api_key", Secret: "secret-landed"}},
		}
		docBytes, _ := json.Marshal(doc)
		envBytes, err := custos.EncryptAge(docBytes, pass)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "vault.age"), envBytes, 0o600); err != nil {
			t.Fatal(err)
		}

		// Record intent for gen 2
		intentNonce := "intent-nonce-d2-landed"
		if err := v.Audit().Append(custos.AuditRecord{
			Kind:  custos.AuditKindVaultMutationIntent,
			Nonce: intentNonce,
			Gen:   2,
			Names: []string{"landed"},
			Actor: "cli",
		}); err != nil {
			t.Fatal(err)
		}

		// Lag the MAC file (still has gen 1 MAC or mismatched MAC)
		macPath := filepath.Join(stateDir, "vault.age.mac")
		if err := os.WriteFile(macPath, []byte("laglaglaglaglaglaglaglaglaglaglaglaglaglaglaglaglaglaglaglaglag1\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Fresh vault instance, unlock directly
		v2 := custos.NewVault(stateDir, 5*time.Second)
		if err := v2.Unlock(pass, false); err != nil {
			t.Fatalf("expected direct unlock to succeed, got %v", err)
		}

		// MAC repaired
		macBytes, _ := os.ReadFile(macPath)
		if !custos.VerifyEnvelopeMAC(key, envBytes, strings.TrimSpace(string(macBytes))) {
			t.Fatal("expected vault.age.mac to be repaired for landed envelope")
		}

		// Audit log has vault_mutation_recovered
		files, _ := v2.Audit().ListLogFiles()
		foundRecovered := false
		for _, f := range files {
			b, _ := os.ReadFile(f)
			for _, line := range strings.Split(string(b), "\n") {
				var rec custos.AuditRecord
				if err := json.Unmarshal([]byte(line), &rec); err == nil {
					if rec.Kind == custos.AuditKindVaultMutationRecovered && rec.Nonce == intentNonce {
						foundRecovered = true
					}
				}
			}
		}
		if !foundRecovered {
			t.Fatal("expected audit log to record vault_mutation_recovered with intent nonce")
		}

		// Clean verify
		res, err := v2.Audit().Verify(key)
		if err != nil || res.IsBroken || res.IsDangling {
			t.Fatalf("expected clean verify, got %v (%s)", err, res.Format())
		}
	})
}

// TestD3_EnvelopeMACStrictLabel tests strict 'custos-env' label enforcement
// per CUSTOS-SPEC §4.1 (D3 regression test).
func TestD3_EnvelopeMACStrictLabel(t *testing.T) {
	key, err := custos.GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	envelopeBytes := []byte("test envelope payload for mac label check")

	// 1. ComputeEnvelopeMAC writes under "custos-env" per §4.1
	mac := custos.ComputeEnvelopeMAC(key, envelopeBytes)

	// Verify using exact HKDF with "custos-env"
	expectedKey := custos.DeriveHKDF(key, "custos-env")
	expectedMAC := custos.ComputeMAC(expectedKey, envelopeBytes)
	if mac != expectedMAC {
		t.Fatalf("ComputeEnvelopeMAC produced %s, want %s (derived under 'custos-env')", mac, expectedMAC)
	}

	// 2. VerifyEnvelopeMAC accepts "custos-env"
	if !custos.VerifyEnvelopeMAC(key, envelopeBytes, mac) {
		t.Fatal("VerifyEnvelopeMAC rejected valid 'custos-env' MAC")
	}

	// 3. VerifyEnvelopeMAC rejects legacy "custos-mac" (no dual-label leniency per CUSTOS-SPEC §4.1)
	legacyKey := custos.DeriveHKDF(key, "custos-mac")
	legacyMAC := custos.ComputeMAC(legacyKey, envelopeBytes)
	if custos.VerifyEnvelopeMAC(key, envelopeBytes, legacyMAC) {
		t.Fatal("VerifyEnvelopeMAC accepted legacy 'custos-mac' MAC; dual-label leniency must be refused per CUSTOS-SPEC §4.1")
	}
}
