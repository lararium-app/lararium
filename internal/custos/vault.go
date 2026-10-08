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

	"github.com/lararium-app/lararium/internal/surface"
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
	mu                sync.RWMutex
	stateDir          string
	lockFile          *LockFile
	audit             *AuditLogger
	snapshots         *SnapshotManager
	policy            *PolicyEngine
	hub               *surface.ApprovalHub
	instanceKey       []byte
	loadedDoc         *VaultDoc
	loadedRegistry    *SurrogateRegistry
	surrogatesDropped int
	derivedKey        []byte // CUSTOS-SPEC §C3: derived secret retained in memory for process lifetime while unlocked; zeroed in Lock()
	isUnlocked        bool
	isKeyfile         bool
	firstUnlockDone   bool // CUSTOS-SPEC §4.1, §4.2: landedness recovery runs once per boot per Vault instance
	degradedHook      func(reason string)

	// Slice 4 (§4.5): per-credential refresh single-flight and the
	// injectable token-endpoint forward seam (V13 fake servers).
	refreshInflight map[string]*refreshCall
	tokenForward    TokenForwardFunc
}

// SetTokenForward overrides the token-endpoint forward used by refresh
// (V13 injects a loopback fake; production keeps DefaultTokenForward).
func (v *Vault) SetTokenForward(f TokenForwardFunc) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tokenForward = f
}

// NewVault initializes a Vault instance rooted at stateDir (<hearth>/custos).
func NewVault(stateDir string, timeout time.Duration) *Vault {
	lockFile := NewLockFile(stateDir, timeout)
	audit := NewAuditLogger(stateDir)
	return &Vault{
		stateDir:  stateDir,
		lockFile:  lockFile,
		audit:     audit,
		snapshots: NewSnapshotManager(stateDir),
		policy:    NewPolicyEngine(stateDir, audit, lockFile),
	}
}

// SetDegradedHook exposes the callback seam that hearthd wiring will use per CUSTOS §2, §C4.
func (v *Vault) SetDegradedHook(hook func(reason string)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.degradedHook = hook
}

// SetApprovalHub connects the surface ApprovalHub for CA-3 lock settlement and revocation.
func (v *Vault) SetApprovalHub(hub *surface.ApprovalHub) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.hub = hub
}

// ApprovalHub returns the attached ApprovalHub, if any.
func (v *Vault) ApprovalHub() *surface.ApprovalHub {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.hub
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

// Policy returns the PolicyEngine.
func (v *Vault) Policy() *PolicyEngine {
	return v.policy
}

// LoadedCredentials returns copies of all credential records from the
// in-memory doc, for the §5.1a whole-set detail scrub. Empty when locked
// (locked custody holds no plaintext to scrub against).
func (v *Vault) LoadedCredentials() []Credential {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.isUnlocked || v.loadedDoc == nil {
		return nil
	}
	out := make([]Credential, 0, len(v.loadedDoc.Credentials))
	for _, c := range v.loadedDoc.Credentials {
		out = append(out, c)
	}
	return out
}

// Surrogates returns the loaded SurrogateRegistry or nil if locked.
func (v *Vault) Surrogates() *SurrogateRegistry {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.loadedRegistry
}

// SurrogatesDropped returns the count of dropped surrogate records per CUSTOS §6.6.
func (v *Vault) SurrogatesDropped() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.surrogatesDropped
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
		"doors.sock":        true,
		"door.token":        true,
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

// Unlock performs unlock and first-unlock recovery under custos.lock per CUSTOS-SPEC §4.1, §4.2, §C3, §6.6, §8.1.
// Serialized exactly-once; concurrent unlock blocks behind it.
func (v *Vault) Unlock(passphrase string, isKeyfile bool) error {
	passphrase = strings.TrimSpace(passphrase)
	if passphrase == "" {
		return ErrEmpty
	}
	// CUSTOS-SPEC §C3: zero the scratch passphrase string after derivation (best-effort)
	defer zeroString(passphrase)

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

	// CUSTOS-SPEC §C3: derive age identity / scrypt output once and retain derived secret
	v.mu.Lock()
	if v.derivedKey != nil {
		zeroBytes(v.derivedKey)
	}
	dk := make([]byte, len(passphrase))
	copy(dk, passphrase)
	v.derivedKey = dk
	v.mu.Unlock()

	// Load instance key
	key, err := v.LoadInstanceKey()
	if err != nil {
		v.clearDerivedKey()
		return err
	}

	// CUSTOS-SPEC §6.6, §8.1: capture wasFirstUnlock before firstUnlockDone is set true
	wasFirstUnlock := !v.firstUnlockDone
	var doc *VaultDoc
	if wasFirstUnlock {
		// CUSTOS-SPEC §4.1, §4.2: First-unlock landedness pass runs once per boot per Vault instance,
		// under flock, serialized exactly-once, BEFORE envelope MAC verification.
		// Resolves mid-pair crash state (new-MAC/old-envelope or landed-lagging-MAC)
		// by inspecting the generation under passphrase, re-emitting MAC, and resolving intents.
		d, err := FirstUnlockRecovery(v.stateDir, key, passphrase, v.audit)
		if err != nil {
			v.clearDerivedKey()
			// CUSTOS §8.1: audit lock_failed on failure
			_ = v.audit.Append(AuditRecord{
				Kind:   AuditKindLockFailed,
				Actor:  "cli",
				Reason: err.Error(),
			})
			return err
		}
		doc = d
		v.firstUnlockDone = true
	} else {
		// Subsequent unlock on same instance: strict envelope MAC verification runs before decryption
		d, err := v.readOnDiskDoc(key, passphrase)
		if err != nil {
			v.clearDerivedKey()
			_ = v.audit.Append(AuditRecord{
				Kind:   AuditKindLockFailed,
				Actor:  "cli",
				Reason: err.Error(),
			})
			return err
		}
		doc = d
	}

	// Strict-parse policy.json at startup per CUSTOS §6.1, §6.6
	_ = v.policy.LoadStrict()

	// Load and reconcile surrogates per CUSTOS-SPEC §6.6, §8.1, §4.3:
	// wasFirstUnlock indicates boot recovery reconcile; orphan drops emit registry_reconciled
	reg, dropped, err := LoadAndReconcileSurrogates(v.stateDir, passphrase, doc.Credentials, v.audit, wasFirstUnlock)
	if err != nil {
		v.clearDerivedKey()
		_ = v.audit.Append(AuditRecord{
			Kind:   AuditKindLockFailed,
			Actor:  "cli",
			Reason: err.Error(),
		})
		return err
	}

	v.mu.Lock()
	v.loadedDoc = doc
	v.loadedRegistry = reg
	v.surrogatesDropped = dropped
	// CUSTOS-SPEC §C3: never retain passphrase cleartext string; retained derived secret lives in v.derivedKey
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

func (v *Vault) clearDerivedKey() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.derivedKey != nil {
		zeroBytes(v.derivedKey)
		v.derivedKey = nil
	}
}

// readOnDiskDocWithKey reads and authenticates vault.age under flock with strict envelope MAC verification per CUSTOS-SPEC §4.1, §4.2, §C3.
func (v *Vault) readOnDiskDocWithKey(key []byte, derivedKey []byte) (*VaultDoc, error) {
	vaultAgePath := filepath.Join(v.stateDir, "vault.age")
	macPath := filepath.Join(v.stateDir, "vault.age.mac")

	macBytes, err := os.ReadFile(macPath)
	if err != nil {
		return nil, ErrVaultEnvelopeCorrupt
	}
	expectedMAC := strings.TrimSpace(string(macBytes))

	envelopeBytes, err := os.ReadFile(vaultAgePath)
	if err != nil {
		return nil, fmt.Errorf("read vault.age: %w", err)
	}

	if !VerifyEnvelopeMAC(key, envelopeBytes, expectedMAC) {
		return nil, ErrVaultEnvelopeCorrupt
	}

	docBytes, err := DecryptAgeWithKey(envelopeBytes, derivedKey)
	if err != nil {
		return nil, err
	}

	var doc VaultDoc
	if err := json.Unmarshal(docBytes, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal vault doc: %w", err)
	}
	return &doc, nil
}

// readOnDiskDoc reads and authenticates vault.age under flock with strict envelope MAC verification per CUSTOS-SPEC §4.1, §4.2.
func (v *Vault) readOnDiskDoc(key []byte, passphrase string) (*VaultDoc, error) {
	return v.readOnDiskDocWithKey(key, []byte(passphrase))
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

	// CUSTOS-SPEC §6.4b: lock flush pushes GONE {reason: custos_locked}
	// for every pending card BEFORE the vault drops its state
	hub := v.hub
	if hub != nil {
		hub.SettleCustosLocked("custos")
	}

	// CUSTOS-SPEC §3, §C3, §P4: lock zeroizes loaded state and retained secret key
	v.loadedDoc = nil
	v.loadedRegistry = nil
	v.surrogatesDropped = 0
	if v.derivedKey != nil {
		zeroBytes(v.derivedKey)
		v.derivedKey = nil
	}
	v.isUnlocked = false
	return nil
}

// IsUnlocked returns true if loaded state is present in memory.
func (v *Vault) IsUnlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.isUnlocked
}

// GetCredential returns a copy of the credential by name if the vault is unlocked.
func (v *Vault) GetCredential(name string) (Credential, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.isUnlocked || v.loadedDoc == nil {
		return Credential{}, false
	}
	c, ok := v.loadedDoc.Credentials[name]
	return c, ok
}

// Status returns the current status JSON per CUSTOS-SPEC §11.
func (v *Vault) Status(isDaemon bool, listenersFailed int) Status {
	v.mu.RLock()
	defer v.mu.RUnlock()

	state := StateLocked
	if v.isUnlocked {
		state = StateUnlocked
	}

	// Check if degraded: WAL recovery unresolved, listeners failed, surrogates dropped, or policy corrupt (§11, §6.6)
	dangling, _ := v.audit.FindDanglingIntents()
	if len(dangling) > 0 || listenersFailed > 0 || v.surrogatesDropped > 0 || (v.policy != nil && v.policy.IsCorrupt()) {
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

	surrogatesCount := 0
	if v.isUnlocked && v.loadedRegistry != nil {
		surrogatesCount = v.loadedRegistry.Count()
	}

	door := "none"
	if isDaemon {
		door = "custosd"
	}

	return Status{
		State:             state,
		Credentials:       credCount,
		Surrogates:        surrogatesCount,
		SurrogatesDropped: v.surrogatesDropped,
		Door:              door,
		ListenersFailed:   listenersFailed,
	}
}

// RegistryMutator is the callback signature for mutations touching both vault and registry per CUSTOS §4.2, §5.3.
type RegistryMutator func(doc *VaultDoc, surDoc *SurrogateDoc) (touchedNames []string, auditEvents []AuditRecord, err error)

// Mutate executes a vault mutation under custos.lock using the §4.2 temp-pair protocol.
// CUSTOS §4.2: intent -> encrypt -> temps fsynced -> rename mac first, envelope second ->
// fsync(dir) -> WAL commit fsynced ONLY AFTER rename+dir fsyncs returned.
func (v *Vault) Mutate(passphrase string, mutateFn func(doc *VaultDoc) ([]string, error), isEphemeral bool) error {
	return v.MutateWithRegistry(passphrase, func(doc *VaultDoc, surDoc *SurrogateDoc) ([]string, []AuditRecord, error) {
		names, err := mutateFn(doc)
		return names, nil, err
	}, isEphemeral)
}

func (v *Vault) resolveMutationKey(passphrase string) ([]byte, error) {
	passphrase = strings.TrimSpace(passphrase)
	var keyBytes []byte

	// CUSTOS-SPEC §C3: mutation path re-encrypts using the retained derived key, never a stored passphrase.
	// CLI file-direct calls keep passing what they prompted; ctl.sock handlers pass "" and get the derived key.
	v.mu.RLock()
	if len(v.derivedKey) > 0 {
		keyBytes = make([]byte, len(v.derivedKey))
		copy(keyBytes, v.derivedKey)
	}
	v.mu.RUnlock()

	if len(keyBytes) == 0 {
		if passphrase == "" {
			return nil, ErrCustosLocked
		}
		defer zeroString(passphrase)
		keyBytes = []byte(passphrase)
	} else if passphrase != "" {
		defer zeroString(passphrase)
	}
	return keyBytes, nil
}

func (v *Vault) loadOrRecoverDoc(key, keyBytes []byte) (*VaultDoc, error) {
	if !v.firstUnlockDone {
		d, err := FirstUnlockRecovery(v.stateDir, key, string(keyBytes), v.audit)
		if err != nil {
			return nil, err
		}
		v.firstUnlockDone = true
		return d, nil
	}
	return v.readOnDiskDocWithKey(key, keyBytes)
}

func (v *Vault) readSurrogatesDocWithKey(keyBytes []byte) (*SurrogateDoc, error) {
	surPath := filepath.Join(v.stateDir, "surrogates.age")
	surData, err := os.ReadFile(surPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read surrogates.age: %w", err)
		}
		return &SurrogateDoc{
			Version:    1,
			Surrogates: make(map[string]SurrogateRecord),
		}, nil
	}
	sd, err := DecryptSurrogatesWithKey(surData, keyBytes)
	if err != nil {
		// CUSTOS-SPEC §6.6: corruption posture — present-but-undecryptable aborts mutation
		return nil, fmt.Errorf("decrypt surrogates: %w", err)
	}
	return sd, nil
}

func (v *Vault) writeSyncTempFile(pattern string, data []byte, nonce string, newGen int64, auditAborted bool) (string, error) {
	tmp, err := os.CreateTemp(v.stateDir, pattern)
	if err != nil {
		if auditAborted {
			_ = v.audit.Append(AuditRecord{
				Kind:   AuditKindVaultMutationAborted,
				Nonce:  nonce,
				Gen:    newGen,
				Reason: "create_temp_failed",
				Actor:  "cli",
			})
		}
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	cleanup = false
	return tmp.Name(), nil
}

// MutateWithRegistry executes a mutation touching vault and/or surrogate registry under flock.
// CUSTOS-SPEC §4.2, §8.1a: intent -> encrypt vault+registry temps fsynced -> rename order: mac first, envelope second,
// surrogates third, then fsync(dir) -> WAL commit fsynced ONLY AFTER rename+dir fsyncs returned.
// A crash inside the rename window yields a dangling intent with registry possibly ahead of vault.
// CUSTOS-SPEC §6.6 load-time reconcile already drops orphan entries (credential missing).
// CUSTOS-SPEC §C3: re-encrypts using the retained derived key, never a stored passphrase.
// CUSTOS-SPEC §6.6: corruption posture — present-but-undecryptable surrogates.age aborts mutation.
func (v *Vault) MutateWithRegistry(passphrase string, mutateFn RegistryMutator, isEphemeral bool) error {
	keyBytes, err := v.resolveMutationKey(passphrase)
	if err != nil {
		return err
	}
	defer zeroBytes(keyBytes)

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

	// 2. Decrypt current document from disk under flock (CUSTOS-SPEC §4.2 file law: re-read disk)
	doc, err := v.loadOrRecoverDoc(key, keyBytes)
	if err != nil {
		return err
	}

	// Decrypt current surrogates document from disk
	// CUSTOS-SPEC §6.6 corruption posture: a present-but-undecryptable surrogates.age
	// must abort the mutation with the corruption error (missing file = empty registry is fine).
	surDoc, err := v.readSurrogatesDocWithKey(keyBytes)
	if err != nil {
		return err
	}

	oldGen := doc.Generation

	// 3. Modify document and registry
	touchedNames, extraAuditEvents, err := mutateFn(doc, surDoc)
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

	// 7. Encrypt updated document (CUSTOS-SPEC §4.2, §C3)
	newDocBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	envelopeBytes, err := EncryptAgeWithKey(newDocBytes, keyBytes)
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

	// Encrypt surrogates temp before renames begin (CUSTOS-SPEC §4.2, §8.1a)
	surEnvelopeBytes, err := EncryptSurrogatesWithKey(surDoc, keyBytes)
	if err != nil {
		_ = v.audit.Append(AuditRecord{
			Kind:   AuditKindVaultMutationAborted,
			Nonce:  nonce,
			Gen:    newGen,
			Reason: "encrypt_surrogates_failed",
			Actor:  "cli",
		})
		return fmt.Errorf("encrypt surrogates: %w", err)
	}

	// 9. Write temp pair and surrogates temp (CUSTOS-SPEC §4.2, §8.1a)
	tmpEnvName, err := v.writeSyncTempFile("vault-mut-*.tmp", envelopeBytes, nonce, newGen, true)
	if err != nil {
		return err
	}
	defer os.Remove(tmpEnvName)

	tmpMacName, err := v.writeSyncTempFile("vaultmac-mut-*.tmp", []byte(mac+"\n"), nonce, newGen, false)
	if err != nil {
		return err
	}
	defer os.Remove(tmpMacName)

	tmpSurName, err := v.writeSyncTempFile("surrogates-mut-*.tmp", surEnvelopeBytes, nonce, newGen, true)
	if err != nil {
		return err
	}
	defer os.Remove(tmpSurName)

	triggerKillPoint("fsync_before_rename")

	// 10. CUSTOS-SPEC §4.2, §8.1a: Rename order: mac first, envelope second, surrogates third, then fsync(dir).
	// Within MutateWithRegistry, encrypt and write registry temp+fsync BEFORE renames begin,
	// rename surrogates.age together with vault renames (after mac, after envelope: order mac, envelope,
	// surrogates, then fsync(dir)), and keep WAL commit last.
	// A crash inside this window yields dangling intent + registry possibly ahead;
	// CUSTOS-SPEC §6.6 load-time reconcile already drops orphan entries (credential missing).
	macTarget := filepath.Join(v.stateDir, "vault.age.mac")
	if err := os.Rename(tmpMacName, macTarget); err != nil {
		return fmt.Errorf("rename mac: %w", err)
	}

	triggerKillPoint("between_renames")

	// 11. Rename envelope second
	envTarget := filepath.Join(v.stateDir, "vault.age")
	if err := os.Rename(tmpEnvName, envTarget); err != nil {
		return fmt.Errorf("rename envelope: %w", err)
	}

	// 12. Rename surrogates third (between "between_renames"/envelope and dir fsync point)
	surTarget := filepath.Join(v.stateDir, "surrogates.age")
	if err := os.Rename(tmpSurName, surTarget); err != nil {
		return fmt.Errorf("rename surrogates: %w", err)
	}

	// 13. CUSTOS-SPEC §4.2: fsync(dir) after renames
	df, err := os.Open(v.stateDir)
	if err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	triggerKillPoint("after_dir_fsync")

	// 14. Update fingerprints.json atomically (CUSTOS §4.6)
	if err := WriteFingerprints(v.stateDir, key, doc.Credentials); err != nil {
		return fmt.Errorf("write fingerprints: %w", err)
	}

	triggerKillPoint("after_rename_before_commit")

	// 15. CUSTOS-SPEC §4.2, §8.1a: WAL commit fsynced ONLY AFTER rename+dir fsyncs returned.
	// Keep WAL commit last.
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

	// 16. Append extra action audit events (e.g. surrogate_created, surrogate_revoked, credential_removed)
	for _, ev := range extraAuditEvents {
		ev.Gen = newGen
		_ = v.audit.Append(ev)
	}

	// 17. In-memory loaded state swap inside flock (K6 doctrine inherited)
	v.mu.Lock()
	v.loadedDoc = doc
	reg := NewSurrogateRegistry()
	for tok, r := range surDoc.Surrogates {
		r.Token = tok
		reg.records[tok] = r
	}
	v.loadedRegistry = reg
	v.isUnlocked = true
	// CUSTOS-SPEC §C3: retain derived secret in memory, never cleartext passphrase string
	if len(v.derivedKey) == 0 && len(keyBytes) > 0 {
		dk := make([]byte, len(keyBytes))
		copy(dk, keyBytes)
		v.derivedKey = dk
	}
	v.mu.Unlock()

	return nil
}

// AddSurrogate registers a surrogate token bound to host, port, path prefix per CUSTOS §5.3.
func (v *Vault) AddSurrogate(passphrase string, credName, host string, port int, pathPrefix string, allowBinary bool, actor string) (string, error) {
	v.mu.RLock()
	hasKey := len(v.derivedKey) > 0
	isUnlocked := v.isUnlocked
	v.mu.RUnlock()
	if !isUnlocked && passphrase == "" && !hasKey {
		return "", ErrCustosLocked
	}
	canonHost, ports, canonPfx, err := ValidateSurrogateBinding(host, port, pathPrefix)
	if err != nil {
		return "", err
	}
	token, err := GenerateSurrogateToken()
	if err != nil {
		return "", err
	}

	var surRec SurrogateRecord
	err = v.MutateWithRegistry(passphrase, func(doc *VaultDoc, surDoc *SurrogateDoc) ([]string, []AuditRecord, error) {
		if _, exists := doc.Credentials[credName]; !exists {
			return nil, nil, fmt.Errorf("credential %q not found", credName)
		}
		surRec = SurrogateRecord{
			Token:       token,
			Credential:  credName,
			Lane:        "bearer",
			Host:        canonHost,
			Ports:       ports,
			PathPrefix:  canonPfx,
			AllowBinary: allowBinary,
			AddedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		if surDoc.Surrogates == nil {
			surDoc.Surrogates = make(map[string]SurrogateRecord)
		}
		surDoc.Surrogates[token] = surRec

		event := AuditRecord{
			Kind:  AuditKindSurrogateCreated,
			Cred:  credName,
			Sur:   surRec.Fingerprint(),
			Host:  canonHost,
			Actor: actor,
		}
		return []string{credName}, []AuditRecord{event}, nil
	}, false)
	if err != nil {
		return "", err
	}
	return token, nil
}

// RevokeSurrogate revokes a surrogate by its 8-hex fingerprint per CUSTOS §5.3, §11.
func (v *Vault) RevokeSurrogate(passphrase string, id8 string, actor string) error {
	v.mu.RLock()
	hasKey := len(v.derivedKey) > 0
	isUnlocked := v.isUnlocked
	v.mu.RUnlock()
	if !isUnlocked && passphrase == "" && !hasKey {
		return ErrCustosLocked
	}

	var revokedCred string
	err := v.MutateWithRegistry(passphrase, func(doc *VaultDoc, surDoc *SurrogateDoc) ([]string, []AuditRecord, error) {
		var targetToken string
		var targetRec SurrogateRecord
		found := false
		for tok, r := range surDoc.Surrogates {
			if strings.EqualFold(r.Fingerprint(), id8) || strings.EqualFold(SHA256Hex8(tok), id8) {
				targetToken = tok
				targetRec = r
				found = true
				break
			}
		}
		if !found {
			return nil, nil, ErrSurrogateNotFound
		}

		delete(surDoc.Surrogates, targetToken)
		revokedCred = targetRec.Credential

		event := AuditRecord{
			Kind:  AuditKindSurrogateRevoked,
			Cred:  targetRec.Credential,
			Sur:   targetRec.Fingerprint(),
			Actor: actor,
		}
		return []string{targetRec.Credential}, []AuditRecord{event}, nil
	}, false)
	if err != nil {
		return err
	}
	if hub := v.ApprovalHub(); hub != nil && revokedCred != "" {
		hub.CancelByCredential(revokedCred, "")
	}
	return nil
}

// RevokeCredential removes a credential and atomically revokes all its surrogates per CUSTOS §4.4, §5.3.
// Single generation, both audit kinds, one intent/commit pair.
func (v *Vault) RevokeCredential(passphrase string, credName string, actor string) error {
	v.mu.RLock()
	hasKey := len(v.derivedKey) > 0
	isUnlocked := v.isUnlocked
	v.mu.RUnlock()
	if !isUnlocked && passphrase == "" && !hasKey {
		return ErrCustosLocked
	}

	err := v.MutateWithRegistry(passphrase, func(doc *VaultDoc, surDoc *SurrogateDoc) ([]string, []AuditRecord, error) {
		if _, exists := doc.Credentials[credName]; !exists {
			return nil, nil, fmt.Errorf("credential %q not found", credName)
		}
		delete(doc.Credentials, credName)

		var revokedSurrogates []SurrogateRecord
		for tok, r := range surDoc.Surrogates {
			if r.Credential == credName {
				revokedSurrogates = append(revokedSurrogates, r)
				delete(surDoc.Surrogates, tok)
			}
		}

		var events []AuditRecord
		events = append(events, AuditRecord{
			Kind:  AuditKindCredentialRemoved,
			Cred:  credName,
			Actor: actor,
		})
		for _, s := range revokedSurrogates {
			events = append(events, AuditRecord{
				Kind:  AuditKindSurrogateRevoked,
				Cred:  credName,
				Sur:   s.Fingerprint(),
				Actor: actor,
			})
		}

		return []string{credName}, events, nil
	}, false)
	if err != nil {
		return err
	}
	if hub := v.ApprovalHub(); hub != nil {
		hub.CancelByCredential(credName, "")
	}
	return nil
}

// ListSurrogates returns all registered surrogates or ErrCustosLocked if locked per CUSTOS §11.
func (v *Vault) ListSurrogates() ([]SurrogateRecord, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.isUnlocked || v.loadedRegistry == nil {
		return nil, ErrCustosLocked
	}
	return v.loadedRegistry.List(), nil
}

// ChangePassphrase re-encrypts the vault and surrogates under a new passphrase.
// Snapshotted as a material mutation per CUSTOS-SPEC §11, §8.4.
func (v *Vault) ChangePassphrase(oldPassphrase, newPassphrase string) error {
	oldPassphrase = strings.TrimSpace(oldPassphrase)
	newPassphrase = strings.TrimSpace(newPassphrase)
	if oldPassphrase == "" || newPassphrase == "" {
		return ErrEmpty
	}
	// CUSTOS-SPEC §C3: zero scratch passphrases after use (best-effort)
	defer zeroString(oldPassphrase)
	defer zeroString(newPassphrase)

	unlock, err := v.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	key, err := v.LoadInstanceKey()
	if err != nil {
		return err
	}

	// Decrypt document with old passphrase (CUSTOS-SPEC §4.2 file law: re-read disk)
	var doc *VaultDoc
	if !v.firstUnlockDone {
		d, err := FirstUnlockRecovery(v.stateDir, key, oldPassphrase, v.audit)
		if err != nil {
			return err
		}
		v.firstUnlockDone = true
		doc = d
	} else {
		d, err := v.readOnDiskDoc(key, oldPassphrase)
		if err != nil {
			return err
		}
		doc = d
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

	// Re-encrypt surrogates.age if present and non-empty per CUSTOS §8.4, §11
	surPath := filepath.Join(v.stateDir, "surrogates.age")
	if surData, err := os.ReadFile(surPath); err == nil && len(surData) > 0 {
		surDoc, err := DecryptSurrogates(surData, oldPassphrase)
		if err == nil {
			_ = WriteSurrogatesFile(v.stateDir, surDoc, newPassphrase)
		}
	}

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
	// CUSTOS-SPEC §C3: retain derived secret in memory, never cleartext passphrase string
	if v.derivedKey != nil {
		zeroBytes(v.derivedKey)
	}
	dk := make([]byte, len(newPassphrase))
	copy(dk, newPassphrase)
	v.derivedKey = dk
	v.mu.Unlock()

	return nil
}
