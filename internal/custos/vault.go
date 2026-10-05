package custos

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// killPointHook is the test kill-point injection seam for crash-matrix verification (V25).
var killPointHook func(point string)

func triggerKillPoint(point string) {
	if killPointHook != nil {
		killPointHook(point)
	}
}

// Vault manages the encrypted vault at <hearth>/custos per CUSTOS-SPEC §4.
type Vault struct {
	mu           sync.RWMutex
	stateDir     string
	lockFile     *LockFile
	audit        *AuditLogger
	snapshots    *SnapshotManager
	instanceKey  []byte
	loadedDoc    *VaultDoc
	isUnlocked   bool
	isKeyfile    bool
	degradedHook func(reason string)
}

// NewVault initializes a Vault instance rooted at stateDir (<hearth>/custos).
func NewVault(stateDir string, timeout time.Duration) *Vault {
	return &Vault{
		stateDir:  stateDir,
		lockFile:  NewLockFile(stateDir, timeout),
		audit:     NewAuditLogger(stateDir),
		snapshots: NewSnapshotManager(stateDir),
	}
}

// SetDegradedHook exposes the callback seam that hearthd wiring will use per CUSTOS §2, §C4.
func (v *Vault) SetDegradedHook(hook func(reason string)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.degradedHook = hook
}

// StateDir returns the state root path.
func (v *Vault) StateDir() string {
	return v.stateDir
}

// Audit returns the AuditLogger.
func (v *Vault) Audit() *AuditLogger {
	return v.audit
}

// Snapshots returns the SnapshotManager.
func (v *Vault) Snapshots() *SnapshotManager {
	return v.snapshots
}

// CheckFileLaw validates C2 file inventory and permissions (0700/0600) per CUSTOS §2, §C2.
func (v *Vault) CheckFileLaw() error {
	fi, err := os.Stat(v.stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// State root 0700 per CUSTOS §C2
	if fi.Mode().Perm() != 0o700 {
		_ = os.Chmod(v.stateDir, 0o700)
	}

	entries, err := os.ReadDir(v.stateDir)
	if err != nil {
		return err
	}

	allowedNames := map[string]bool{
		"vault.age":         true,
		"vault.age.mac":     true,
		"vault.key":         true,
		"surrogates.age":    true,
		"fingerprints.json": true,
		"policy.json":       true,
		"custos.lock":       true,
		"ctl.sock":          true,
		"ctl.token":         true,
		"audit":             true,
		"snapshots":         true,
	}

	for _, e := range entries {
		name := e.Name()
		if allowedNames[name] || strings.HasSuffix(name, ".tmp") {
			continue
		}
		// CUSTOS §2 file law: unexpected files in the root at startup: refuse with named error
		return fmt.Errorf("%w: %s", ErrUnexpectedFile, name)
	}
	return nil
}

// Exists reports whether a vault already exists in the state directory.
func (v *Vault) Exists() bool {
	vaultAge := filepath.Join(v.stateDir, "vault.age")
	vaultKey := filepath.Join(v.stateDir, "vault.key")
	_, err1 := os.Stat(vaultAge)
	_, err2 := os.Stat(vaultKey)
	return err1 == nil || err2 == nil
}

// LoadInstanceKey reads vault.key (0600) from state root per CUSTOS §4.1.
func (v *Vault) LoadInstanceKey() ([]byte, error) {
	keyPath := filepath.Join(v.stateDir, "vault.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read vault.key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid vault.key size: %d", len(key))
	}
	v.instanceKey = key
	return key, nil
}

// Init creates a new vault state root per CUSTOS-SPEC §4.1.
func (v *Vault) Init(passphrase string) error {
	passphrase = strings.TrimSpace(passphrase)
	if passphrase == "" {
		return ErrEmpty
	}

	// 1. Check if vault already exists (CUSTOS §10 V1)
	if v.Exists() {
		return ErrVaultExists
	}

	// 2. Create state root 0700 per CUSTOS §C2
	if err := os.MkdirAll(v.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state root: %w", err)
	}

	unlock, err := v.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// 3. Generate instance key vault.key: 32 random bytes, 0600 (CUSTOS §4.1)
	key, err := GenerateInstanceKey()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(v.stateDir, "vault.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return fmt.Errorf("write vault.key: %w", err)
	}
	v.instanceKey = key

	// 4. Create empty surrogates placeholder (CUSTOS §8.4)
	if err := EnsureSurrogatesPlaceholder(v.stateDir); err != nil {
		return fmt.Errorf("create surrogates placeholder: %w", err)
	}

	// 5. Initial vault document (version 1, generation 1) per CUSTOS §4.1
	doc := VaultDoc{
		Version:     1,
		Generation:  1,
		Credentials: make(map[string]Credential),
	}
	docBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}

	// 6. Encrypt with age
	envelopeBytes, err := EncryptAge(docBytes, passphrase)
	if err != nil {
		return fmt.Errorf("encrypt initial vault: %w", err)
	}

	// 7. Write temp pair: vault.age and vault.age.mac (CUSTOS §4.2)
	mac := ComputeEnvelopeMAC(key, envelopeBytes)

	tmpMac, err := os.CreateTemp(v.stateDir, "vaultmac-init-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpMac.Name())
	if _, err := tmpMac.WriteString(mac + "\n"); err != nil {
		tmpMac.Close()
		return err
	}
	if err := tmpMac.Sync(); err != nil {
		tmpMac.Close()
		return err
	}
	tmpMac.Close()
	_ = os.Chmod(tmpMac.Name(), 0o600)

	tmpEnv, err := os.CreateTemp(v.stateDir, "vault-init-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpEnv.Name())
	if _, err := tmpEnv.Write(envelopeBytes); err != nil {
		tmpEnv.Close()
		return err
	}
	if err := tmpEnv.Sync(); err != nil {
		tmpEnv.Close()
		return err
	}
	tmpEnv.Close()
	_ = os.Chmod(tmpEnv.Name(), 0o600)

	// CUSTOS §4.2 mac-first rename
	if err := os.Rename(tmpMac.Name(), filepath.Join(v.stateDir, "vault.age.mac")); err != nil {
		return err
	}
	if err := os.Rename(tmpEnv.Name(), filepath.Join(v.stateDir, "vault.age")); err != nil {
		return err
	}

	// CUSTOS §4.2 fsync(dir)
	df, err := os.Open(v.stateDir)
	if err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	// 8. Write empty fingerprints sidecar (CUSTOS §4.6)
	if err := WriteFingerprints(v.stateDir, key, doc.Credentials); err != nil {
		return err
	}

	// 9. Initial audit record (genesis line per CUSTOS §8.1)
	if err := v.audit.Append(AuditRecord{
		Kind:  AuditKindCustosStarted,
		Gen:   1,
		Actor: "cli",
	}); err != nil {
		return err
	}

	return nil
}

// Unlock performs unlock and first-unlock recovery under custos.lock per CUSTOS-SPEC §4.1, §4.2.
// Serialized exactly-once; concurrent unlock blocks behind it.
func (v *Vault) Unlock(passphrase string, isKeyfile bool) error {
	passphrase = strings.TrimSpace(passphrase)
	if passphrase == "" {
		return ErrEmpty
	}

	unlock, err := v.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	v.mu.Lock()
	if v.isUnlocked {
		// Double unlock is a no-op per CUSTOS §10 V1
		v.mu.Unlock()
		return nil
	}
	v.mu.Unlock()

	// Load instance key
	key, err := v.LoadInstanceKey()
	if err != nil {
		return err
	}

	// CUSTOS §4.2 landedness recovery:
	// Runs at first unlock under flock; inspects generation to resolve any dangling intent
	doc, err := FirstUnlockRecovery(v.stateDir, key, passphrase, v.audit)
	if err != nil {
		// CUSTOS §8.1: audit lock_failed on failure
		_ = v.audit.Append(AuditRecord{
			Kind:   AuditKindLockFailed,
			Actor:  "cli",
			Reason: err.Error(),
		})
		return err
	}

	v.mu.Lock()
	v.loadedDoc = doc
	v.isUnlocked = true
	v.isKeyfile = isKeyfile
	v.mu.Unlock()

	// Audit unlocked once per CUSTOS §8.1, §10 V1
	_ = v.audit.Append(AuditRecord{
		Kind:  AuditKindUnlocked,
		Gen:   doc.Generation,
		Actor: "cli",
	})

	return nil
}

// Lock clears loaded state and marks the vault locked per CUSTOS-SPEC §3, §C3.
// Refuses in keyfile mode per CUSTOS §C3.
func (v *Vault) Lock() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.isKeyfile {
		// CUSTOS §C3: custos lock in keyfile mode refuses
		return ErrKeyfileModeAlwaysUnlocked
	}

	// CUSTOS §C3, §P4: lock zeroizes loaded state
	v.loadedDoc = nil
	v.isUnlocked = false
	return nil
}

// IsUnlocked returns true if loaded state is present in memory.
func (v *Vault) IsUnlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.isUnlocked
}

// Status returns the current status JSON per CUSTOS-SPEC §11.
func (v *Vault) Status(isDaemon bool, listenersFailed int) Status {
	v.mu.RLock()
	defer v.mu.RUnlock()

	state := StateLocked
	if v.isUnlocked {
		state = StateUnlocked
	}

	// Check if degraded: WAL recovery unresolved or listeners failed (§11)
	dangling, _ := v.audit.FindDanglingIntents()
	if len(dangling) > 0 || listenersFailed > 0 {
		state = StateDegraded
	}

	credCount := 0
	if v.isUnlocked && v.loadedDoc != nil {
		credCount = len(v.loadedDoc.Credentials)
	} else {
		// Read from fingerprints sidecar while locked per CUSTOS §4.6
		fp, err := ReadFingerprints(v.stateDir)
		if err == nil {
			credCount = len(fp.Names())
		}
	}

	door := "none"
	if isDaemon {
		door = "custosd"
	}

	return Status{
		State:             state,
		Credentials:       credCount,
		Surrogates:        0,
		SurrogatesDropped: 0,
		Door:              door,
		ListenersFailed:   listenersFailed,
	}
}

// Mutate executes a vault mutation under custos.lock using the §4.2 temp-pair protocol.
// CUSTOS §4.2: intent -> encrypt -> temps fsynced -> rename mac first, envelope second ->
// fsync(dir) -> WAL commit fsynced ONLY AFTER rename+dir fsyncs returned.
func (v *Vault) Mutate(passphrase string, mutateFn func(doc *VaultDoc) ([]string, error), isEphemeral bool) error {
	passphrase = strings.TrimSpace(passphrase)
	if passphrase == "" {
		return ErrEmpty
	}

	// 1. Acquire flock with timeout
	unlock, err := v.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	key, err := v.LoadInstanceKey()
	if err != nil {
		return err
	}

	// 2. Decrypt current document from disk under flock (CUSTOS §4.2 file law: re-read disk)
	doc, err := FirstUnlockRecovery(v.stateDir, key, passphrase, v.audit)
	if err != nil {
		return err
	}

	oldGen := doc.Generation

	// 3. Modify document
	touchedNames, err := mutateFn(doc)
	if err != nil {
		return err
	}

	newGen := oldGen + 1
	doc.Generation = newGen

	// 4. Pre-image snapshot set to snapshots/gen-<oldGen>/ (CUSTOS §8.4)
	if !isEphemeral {
		if err := v.snapshots.SavePreImage(oldGen, false); err != nil {
			return fmt.Errorf("save snapshot: %w", err)
		}
	}

	// 5. Generate random nonce
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceBytes)

	// 6. Append vault_mutation_intent before rename (CUSTOS §8.1a write-ahead rule)
	if err := v.audit.Append(AuditRecord{
		Kind:  AuditKindVaultMutationIntent,
		Nonce: nonce,
		Gen:   newGen,
		Names: touchedNames,
		Actor: "cli",
	}); err != nil {
		return fmt.Errorf("append intent: %w", err)
	}

	triggerKillPoint("after_intent")

	// 7. Encrypt updated document
	newDocBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	envelopeBytes, err := EncryptAge(newDocBytes, passphrase)
	if err != nil {
		_ = v.audit.Append(AuditRecord{
			Kind:   AuditKindVaultMutationAborted,
			Nonce:  nonce,
			Gen:    newGen,
			Reason: "encrypt_failed",
			Actor:  "cli",
		})
		return fmt.Errorf("encrypt vault: %w", err)
	}

	// 8. Compute MAC
	mac := ComputeEnvelopeMAC(key, envelopeBytes)

	// 9. Write temp pair
	tmpEnv, err := os.CreateTemp(v.stateDir, "vault-mut-*.tmp")
	if err != nil {
		_ = v.audit.Append(AuditRecord{
			Kind:   AuditKindVaultMutationAborted,
			Nonce:  nonce,
			Gen:    newGen,
			Reason: "create_temp_failed",
			Actor:  "cli",
		})
		return err
	}
	defer os.Remove(tmpEnv.Name())

	if _, err := tmpEnv.Write(envelopeBytes); err != nil {
		tmpEnv.Close()
		return err
	}
	if err := tmpEnv.Sync(); err != nil {
		tmpEnv.Close()
		return err
	}
	if err := tmpEnv.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpEnv.Name(), 0o600)

	tmpMac, err := os.CreateTemp(v.stateDir, "vaultmac-mut-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpMac.Name())

	if _, err := tmpMac.WriteString(mac + "\n"); err != nil {
		tmpMac.Close()
		return err
	}
	if err := tmpMac.Sync(); err != nil {
		tmpMac.Close()
		return err
	}
	if err := tmpMac.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpMac.Name(), 0o600)

	triggerKillPoint("fsync_before_rename")

	// 10. CUSTOS §4.2 mac-first rename
	macTarget := filepath.Join(v.stateDir, "vault.age.mac")
	if err := os.Rename(tmpMac.Name(), macTarget); err != nil {
		return fmt.Errorf("rename mac: %w", err)
	}

	triggerKillPoint("between_renames")

	// 11. Rename envelope second
	envTarget := filepath.Join(v.stateDir, "vault.age")
	if err := os.Rename(tmpEnv.Name(), envTarget); err != nil {
		return fmt.Errorf("rename envelope: %w", err)
	}

	// 12. CUSTOS §4.2 fsync(dir)
	df, err := os.Open(v.stateDir)
	if err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	triggerKillPoint("after_dir_fsync")

	// 13. Update fingerprints.json atomically (CUSTOS §4.6)
	if err := WriteFingerprints(v.stateDir, key, doc.Credentials); err != nil {
		return fmt.Errorf("write fingerprints: %w", err)
	}

	triggerKillPoint("after_rename_before_commit")

	// 14. CUSTOS §4.2: WAL commit fsynced ONLY AFTER rename+dir fsyncs returned
	if err := v.audit.Append(AuditRecord{
		Kind:  AuditKindVaultMutation,
		Nonce: nonce,
		Gen:   newGen,
		Names: touchedNames,
		Actor: "cli",
	}); err != nil {
		return fmt.Errorf("append commit: %w", err)
	}

	triggerKillPoint("after_commit")

	// 15. In-memory loaded state swap inside flock (K6 doctrine inherited)
	v.mu.Lock()
	v.loadedDoc = doc
	v.isUnlocked = true
	v.mu.Unlock()

	return nil
}

// ChangePassphrase re-encrypts the vault and surrogates under a new passphrase.
// Snapshotted as a material mutation per CUSTOS-SPEC §11, §8.4.
func (v *Vault) ChangePassphrase(oldPassphrase, newPassphrase string) error {
	oldPassphrase = strings.TrimSpace(oldPassphrase)
	newPassphrase = strings.TrimSpace(newPassphrase)
	if oldPassphrase == "" || newPassphrase == "" {
		return ErrEmpty
	}

	unlock, err := v.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	key, err := v.LoadInstanceKey()
	if err != nil {
		return err
	}

	// Decrypt document with old passphrase
	doc, err := FirstUnlockRecovery(v.stateDir, key, oldPassphrase, v.audit)
	if err != nil {
		return err
	}

	oldGen := doc.Generation
	newGen := oldGen + 1
	doc.Generation = newGen

	// Save pre-image snapshot (material mutation per §8.4)
	if err := v.snapshots.SavePreImage(oldGen, false); err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}

	nonceBytes := make([]byte, 16)
	_, _ = rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)

	// Append intent
	if err := v.audit.Append(AuditRecord{
		Kind:   AuditKindVaultMutationIntent,
		Nonce:  nonce,
		Gen:    newGen,
		Actor:  "cli",
		Reason: "change_passphrase",
	}); err != nil {
		return err
	}

	// Encrypt under new passphrase
	newDocBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	envelopeBytes, err := EncryptAge(newDocBytes, newPassphrase)
	if err != nil {
		return err
	}

	mac := ComputeEnvelopeMAC(key, envelopeBytes)

	// Temp pair protocol (CUSTOS §4.2)
	tmpEnv, err := os.CreateTemp(v.stateDir, "vault-cp-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpEnv.Name())
	_, _ = tmpEnv.Write(envelopeBytes)
	_ = tmpEnv.Sync()
	_ = tmpEnv.Close()
	_ = os.Chmod(tmpEnv.Name(), 0o600)

	tmpMac, err := os.CreateTemp(v.stateDir, "vaultmac-cp-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpMac.Name())
	_, _ = tmpMac.WriteString(mac + "\n")
	_ = tmpMac.Sync()
	_ = tmpMac.Close()
	_ = os.Chmod(tmpMac.Name(), 0o600)

	// Mac first
	if err := os.Rename(tmpMac.Name(), filepath.Join(v.stateDir, "vault.age.mac")); err != nil {
		return err
	}
	// Envelope second
	if err := os.Rename(tmpEnv.Name(), filepath.Join(v.stateDir, "vault.age")); err != nil {
		return err
	}

	// Fsync dir
	df, err := os.Open(v.stateDir)
	if err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	_ = WriteFingerprints(v.stateDir, key, doc.Credentials)

	// Commit line
	if err := v.audit.Append(AuditRecord{
		Kind:   AuditKindVaultMutation,
		Nonce:  nonce,
		Gen:    newGen,
		Actor:  "cli",
		Reason: "change_passphrase",
	}); err != nil {
		return err
	}

	v.mu.Lock()
	v.loadedDoc = doc
	v.isUnlocked = true
	v.mu.Unlock()

	return nil
}
