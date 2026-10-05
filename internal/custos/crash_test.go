package custos_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestHelperProcess runs as a subprocess when GO_WANT_HELPER_PROCESS=1.
// It executes a vault mutation and gets killed at the named CUSTOS_KILL_POINT seam.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	stateDir := os.Getenv("CUSTOS_STATE_DIR")
	passphrase := os.Getenv("CUSTOS_PASSPHRASE")
	killPoint := os.Getenv("CUSTOS_KILL_POINT")

	custos.SetTestAgeWorkFactor(10)
	custos.SetKillPointHook(func(point string) {
		if point == killPoint {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	})

	v := custos.NewVault(stateDir, 5*time.Second)
	_ = v.Mutate(passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["mutated_key"] = custos.Credential{
			Kind:   "api_key",
			Secret: "secret-val-999",
		}
		return []string{"mutated_key"}, nil
	}, false)

	os.Exit(0)
}

func runSubprocessMutationWithKill(t *testing.T, stateDir, passphrase, killPoint string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--") //nolint:gosec // test helper executes test binary
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"CUSTOS_STATE_DIR="+stateDir,
		"CUSTOS_PASSPHRASE="+passphrase,
		"CUSTOS_KILL_POINT="+killPoint,
	)
	_ = cmd.Run()
}

// V25: Durability + quiescence crash matrix per CUSTOS-SPEC §10, §4.2, §8.1a
// 1. intent-only abort+re-emit.
func TestV25_Case1_IntentOnlyAbortReemit(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Run mutation in subprocess killed after intent line
	runSubprocessMutationWithKill(t, stateDir, pass, "after_intent")

	// First unlock recovery
	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after intent kill: %v", err)
	}

	// Verify quiesces cleanly (no dangling intent)
	key, err := v2.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	res, err := v2.Audit().Verify(key)
	if err != nil || res.IsBroken || res.IsDangling {
		t.Fatalf("verify failed after abort: %v (%s)", err, res.Format())
	}
	if !strings.HasPrefix(res.Format(), "chain ok") {
		t.Fatalf("expected chain ok, got %s", res.Format())
	}

	// Verify audit log has vault_mutation_aborted with reason crashed_pre_rename
	files, _ := v2.Audit().ListLogFiles()
	foundAborted := false
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			var rec custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				if rec.Kind == custos.AuditKindVaultMutationAborted && rec.Reason == "crashed_pre_rename" {
					foundAborted = true
				}
			}
		}
	}
	if !foundAborted {
		t.Error("expected audit log to record vault_mutation_aborted with reason crashed_pre_rename")
	}
}

// 2. between-renames kill resolves mac re-emit.
func TestV25_Case2_BetweenRenamesKillResolvesMacReemit(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Run mutation killed between mac and envelope renames
	runSubprocessMutationWithKill(t, stateDir, pass, "between_renames")

	// First unlock recovery
	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after between_renames kill: %v", err)
	}

	// MAC on disk must match surviving envelope
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
		t.Fatal("envelope MAC does not verify after recovery re-emit")
	}

	// Verify quiesces
	res, err := v2.Audit().Verify(key)
	if err != nil || res.IsBroken || res.IsDangling {
		t.Fatalf("verify failed: %v (%s)", err, res.Format())
	}
}

// 3. landed-uncommitted -> nonce-bearing recovered + verify quiesces.
func TestV25_Case3_LandedUncommittedRecovered(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Run mutation killed after renames and dir fsync, before commit line
	runSubprocessMutationWithKill(t, stateDir, pass, "after_rename_before_commit")

	// First unlock recovery
	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after uncommitted kill: %v", err)
	}

	// Verify quiesces
	key, err := v2.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	res, err := v2.Audit().Verify(key)
	if err != nil || res.IsBroken || res.IsDangling {
		t.Fatalf("verify failed: %v (%s)", err, res.Format())
	}

	// Verify nonce-bearing vault_mutation_recovered is recorded
	files, _ := v2.Audit().ListLogFiles()
	foundRecovered := false
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			var rec custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				if rec.Kind == custos.AuditKindVaultMutationRecovered && rec.Nonce != "" {
					foundRecovered = true
				}
			}
		}
	}
	if !foundRecovered {
		t.Error("expected audit log to record nonce-bearing vault_mutation_recovered")
	}
}

// 4. post-restore supersede.
func TestV25_Case4_PostRestoreSupersede(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Mutate cleanly to gen 2 (snapshot gen 1 created)
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["key1"] = custos.Credential{Kind: "api_key", Secret: "v1"}
		return []string{"key1"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Kill mutation to gen 3 after intent (leaving dangling intent for gen 3)
	runSubprocessMutationWithKill(t, stateDir, pass, "after_intent")

	// Restore prior generation 1 offline under flock
	v2 := custos.NewVault(stateDir, 5*time.Second)
	key, err := v2.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}

	if err := v2.Snapshots().Restore(1, key, v2.Audit()); err != nil {
		t.Fatalf("restore prior gen 1: %v", err)
	}

	// First unlock after restore
	if err := v2.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after restore: %v", err)
	}

	// Verify must quiesce cleanly (dangling intent superseded)
	res, err := v2.Audit().Verify(key)
	if err != nil || res.IsBroken || res.IsDangling {
		t.Fatalf("verify after restore supersede failed: %v (%s)", err, res.Format())
	}
}

// 5. recovery idempotent under replay of its own intent/commit pair.
func TestV25_Case5_RecoveryIdempotent(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// Leave a dangling uncommitted mutation
	runSubprocessMutationWithKill(t, stateDir, pass, "after_intent")

	// Run recovery first time
	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(pass, false); err != nil {
		t.Fatalf("first recovery unlock: %v", err)
	}

	key, err := v2.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	res1, err := v2.Audit().Verify(key)
	if err != nil || res1.IsBroken || res1.IsDangling {
		t.Fatalf("first verify failed: %v (%s)", err, res1.Format())
	}

	// Lock and re-unlock (replaying check)
	_ = v2.Lock()
	v3 := custos.NewVault(stateDir, 5*time.Second)
	if err := v3.Unlock(pass, false); err != nil {
		t.Fatalf("second recovery unlock: %v", err)
	}

	res2, err := v3.Audit().Verify(key)
	if err != nil || res2.IsBroken || res2.IsDangling {
		t.Fatalf("second verify failed: %v (%s)", err, res2.Format())
	}
}
