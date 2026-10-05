package custos

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxSnapshots = 8 // CUSTOS §8.4: keep 8 snapshots

// SnapshotManager handles snapshot creation and restore per CUSTOS-SPEC §8.4.
type SnapshotManager struct {
	stateDir     string
	snapshotsDir string
}

// NewSnapshotManager returns a SnapshotManager rooted at stateDir.
func NewSnapshotManager(stateDir string) *SnapshotManager {
	return &SnapshotManager{
		stateDir:     stateDir,
		snapshotsDir: filepath.Join(stateDir, "snapshots"),
	}
}

// EnsureSnapshotsDir creates <hearth>/custos/snapshots/ 0700 per CUSTOS §C2.
func (sm *SnapshotManager) EnsureSnapshotsDir() error {
	return os.MkdirAll(sm.snapshotsDir, 0o700)
}

// ListGenerations returns all available snapshot generations sorted ascending.
func (sm *SnapshotManager) ListGenerations() ([]int64, error) {
	if err := sm.EnsureSnapshotsDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(sm.snapshotsDir)
	if err != nil {
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}
	var gens []int64
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "gen-") {
			s := strings.TrimPrefix(e.Name(), "gen-")
			if g, err := strconv.ParseInt(s, 10, 64); err == nil {
				gens = append(gens, g)
			}
		}
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	return gens, nil
}

// SnapshotDir returns the path to snapshots/gen-<gen>/.
func (sm *SnapshotManager) SnapshotDir(gen int64) string {
	return filepath.Join(sm.snapshotsDir, fmt.Sprintf("gen-%d", gen))
}

// CopyFile copies src to dst enforcing mode 0600.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// EnsureSurrogatesPlaceholder ensures surrogates.age exists (empty placeholder if absent).
// CUSTOS §8.4: surrogates.age (create empty-if-absent placeholder so the set is always complete).
func EnsureSurrogatesPlaceholder(stateDir string) error {
	p := filepath.Join(stateDir, "surrogates.age")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		return f.Close()
	}
	return nil
}

// SavePreImage copies vault.age, vault.age.mac, surrogates.age, fingerprints.json to snapshots/gen-<gen>/.
// CUSTOS §8.4: keep 8; ephemeral access-token refreshes skip snapshot.
func (sm *SnapshotManager) SavePreImage(currentGen int64, isEphemeral bool) error {
	if isEphemeral || currentGen <= 0 {
		return nil
	}

	vaultAge := filepath.Join(sm.stateDir, "vault.age")
	if _, err := os.Stat(vaultAge); os.IsNotExist(err) {
		// No pre-image exists yet (e.g. init)
		return nil
	}

	if err := sm.EnsureSnapshotsDir(); err != nil {
		return err
	}

	targetDir := sm.SnapshotDir(currentGen)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir %s: %w", targetDir, err)
	}

	if err := EnsureSurrogatesPlaceholder(sm.stateDir); err != nil {
		return err
	}

	// Copy the complete set (§8.4)
	files := []string{"vault.age", "vault.age.mac", "surrogates.age", "fingerprints.json"}
	for _, fname := range files {
		src := filepath.Join(sm.stateDir, fname)
		dst := filepath.Join(targetDir, fname)
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("copy %s to snapshot: %w", fname, err)
			}
		}
	}

	// Prune older snapshots, keeping at most maxSnapshots (8)
	return sm.PruneSnapshots()
}

// PruneSnapshots removes old snapshots past the newest 8 generations.
func (sm *SnapshotManager) PruneSnapshots() error {
	gens, err := sm.ListGenerations()
	if err != nil {
		return err
	}
	if len(gens) <= maxSnapshots {
		return nil
	}

	// Remove oldest until we have maxSnapshots
	removeCount := len(gens) - maxSnapshots
	for i := range removeCount {
		dir := sm.SnapshotDir(gens[i])
		_ = os.RemoveAll(dir)
	}
	return nil
}

// VerifySnapshotMAC verifies the snapshot's vault.age.mac using vault.key (no passphrase needed).
// CUSTOS §8.4: verify snapshot .mac with vault.key (no passphrase needed).
func (sm *SnapshotManager) VerifySnapshotMAC(gen int64, instanceKey []byte) error {
	dir := sm.SnapshotDir(gen)
	vaultAgePath := filepath.Join(dir, "vault.age")
	macPath := filepath.Join(dir, "vault.age.mac")

	envelopeBytes, err := os.ReadFile(vaultAgePath)
	if err != nil {
		return fmt.Errorf("read snapshot vault.age: %w", err)
	}

	macBytes, err := os.ReadFile(macPath)
	if err != nil {
		return fmt.Errorf("read snapshot vault.age.mac: %w", err)
	}
	expectedMAC := strings.TrimSpace(string(macBytes))

	if !VerifyEnvelopeMAC(instanceKey, envelopeBytes, expectedMAC) {
		return ErrSnapshotCorrupt
	}
	return nil
}

// Restore restores snapshot gen into place atomically under custos.lock.
// CUSTOS §8.4: verify snapshot .mac with vault.key; rename set into place;
// audit intent/commit pair reason restore; bootstrap anchors if empty; supersede dangling intents.
func (sm *SnapshotManager) Restore(gen int64, instanceKey []byte, audit *AuditLogger) error {
	targetDir := sm.SnapshotDir(gen)
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		return ErrSnapshotNotFound
	}

	// 1. Verify snapshot .mac with vault.key (§8.4)
	if err := sm.VerifySnapshotMAC(gen, instanceKey); err != nil {
		return err
	}

	// 2. Bootstrap anchors if missing or empty before verifying (§8.4)
	if err := audit.BootstrapGenesisAnchor(instanceKey); err != nil {
		return fmt.Errorf("bootstrap anchor: %w", err)
	}

	// 3. Supersede all currently dangling intents (§4.2, §8.4)
	files, err := audit.ListLogFiles()
	if err != nil {
		return err
	}
	type danglingIntent struct {
		nonce string
		names []string
		gen   int64
	}
	dangling := make(map[string]danglingIntent)
	for _, fpath := range files {
		lines, err := readNonEmptyLines(fpath)
		if err != nil {
			return err
		}
		for _, li := range lines {
			var rec AuditRecord
			if err := json.Unmarshal(li.raw, &rec); err != nil {
				continue
			}
			if rec.Nonce != "" {
				switch rec.Kind {
				case AuditKindVaultMutationIntent:
					dangling[rec.Nonce] = danglingIntent{nonce: rec.Nonce, names: rec.Names, gen: rec.Gen}
				case AuditKindVaultMutation, AuditKindVaultMutationRecovered,
					AuditKindVaultMutationAborted, AuditKindVaultMutationSuperseded:
					delete(dangling, rec.Nonce)
				}
			}
		}
	}

	for _, d := range dangling {
		// Audit vault_mutation_superseded, names only (§8.4)
		_ = audit.Append(AuditRecord{
			Kind:   AuditKindVaultMutationSuperseded,
			Nonce:  d.nonce,
			Names:  d.names,
			Gen:    d.gen,
			Actor:  "cli",
			Reason: "restore",
		})
	}

	// 4. Audit intent/commit pair for restore (§8.4)
	restoreNonce := fmt.Sprintf("restore-%d-%d", gen, time.Now().UnixNano())
	if err := audit.Append(AuditRecord{
		Kind:   AuditKindVaultMutationIntent,
		Nonce:  restoreNonce,
		Gen:    gen,
		Actor:  "cli",
		Reason: "restore",
	}); err != nil {
		return fmt.Errorf("append restore intent: %w", err)
	}

	// 5. Copy/rename snapshot set into place (§8.4)
	setFiles := []string{"vault.age", "vault.age.mac", "surrogates.age", "fingerprints.json"}
	for _, fname := range setFiles {
		src := filepath.Join(targetDir, fname)
		dst := filepath.Join(sm.stateDir, fname)
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("restore copy %s: %w", fname, err)
			}
		}
	}

	// Fsync stateDir
	dirF, err := os.Open(sm.stateDir)
	if err == nil {
		_ = dirF.Sync()
		_ = dirF.Close()
	}

	// 6. Audit commit line (§8.4)
	if err := audit.Append(AuditRecord{
		Kind:   AuditKindVaultMutation,
		Nonce:  restoreNonce,
		Gen:    gen,
		Actor:  "cli",
		Reason: "restore",
	}); err != nil {
		return fmt.Errorf("append restore commit: %w", err)
	}

	return nil
}
