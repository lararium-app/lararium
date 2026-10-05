package custos_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// D1 probe: crash inside the rename window of MutateWithRegistry must
// leave a state that first-unlock recovery resolves per §4.2/§6.6 —
// dangling intent reported, never corruption-locked, registry never
// orphaning credentials (one-way safe).
func TestD1_Probe_CrashBetweenDirFsyncAndCommit(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "custos")
	key := "probe-passphrase-1"

	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init(key); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(key, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := v.Mutate(key, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: "sk-test-1234567890"}
		return []string{"openai"}, nil
	}, false); err != nil {
		t.Fatalf("add cred: %v", err)
	}

	// Crash AFTER dir fsync (renames durable, registry renamed too)
	// but BEFORE the WAL commit line.
	custos.SetKillPointHook(func(point string) {
		if point == "after_dir_fsync" {
			panic("injected crash")
		}
	})
	func() {
		defer func() { _ = recover() }()
		_, _ = v.AddSurrogate(key, "openai", "api.openai.com", 443, "", false, "cli")
	}()
	custos.SetKillPointHook(nil)

	// State on disk: vault+mac+surrogates renamed, intent dangling.
	if _, err := os.Stat(filepath.Join(stateDir, "surrogates.age")); err != nil {
		t.Fatalf("surrogates.age should be renamed before dir fsync: %v", err)
	}

	// Fresh instance: first unlock must resolve, not lock out.
	v2 := custos.NewVault(stateDir, 5*time.Second)
	if err := v2.Unlock(key, false); err != nil {
		t.Fatalf("post-crash unlock must recover, got: %v", err)
	}
	sur, err := v2.ListSurrogates()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sur) != 1 {
		t.Fatalf("registry one-way safe: surrogate must survive with its credential present, got %d", len(sur))
	}

	// Audit: dangling intent must be reported as resolved, never silently dropped.
	auditDir := filepath.Join(stateDir, "audit")
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("audit dir: %v", err)
	}
	var logged string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(auditDir, e.Name()))
		if err == nil {
			logged += string(b)
		}
	}
	if !strings.Contains(logged, "vault_mutation_recovered") &&
		!strings.Contains(logged, "vault_mutation_superseded") {
		t.Fatalf("expected recovery audit event, got:\n%s", logged)
	}
}
