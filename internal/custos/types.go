package custos

import (
	"errors"
)

// Frozen failure strings and errors per CUSTOS-SPEC §4.1, §4.2, §10, §11, KEYS-SPEC §K3.
var (
	// ErrVaultExists is returned when init is called on an existing vault (CUSTOS §10 V1).
	ErrVaultExists = errors.New("vault exists")

	// ErrVaultEnvelopeCorrupt is returned on MAC mismatch or tamper (CUSTOS §4.1).
	ErrVaultEnvelopeCorrupt = errors.New("vault envelope corrupt")

	// ErrKeyfileModeAlwaysUnlocked is returned when custos lock is attempted in keyfile mode (CUSTOS §C3, §4.1).
	ErrKeyfileModeAlwaysUnlocked = errors.New("keyfile mode: always unlocked — remove config to change")

	// ErrCustosLocked is returned when operations require unlock (CUSTOS CA-1(d)).
	ErrCustosLocked = errors.New("custos locked — run: custos unlock")

	// ErrLockTimeout is returned when custos.lock acquisition times out (CUSTOS §4.2, CA-1(e)).
	ErrLockTimeout = errors.New("lock_timeout")

	// ErrEmpty is returned on empty key/passphrase input (KEYS-SPEC §K3).
	ErrEmpty = errors.New("empty key")

	// ErrBadName is returned on invalid provider name (KEYS-SPEC §K3).
	ErrBadName = errors.New("invalid provider name")

	// ErrFull is returned when key store capacity is exceeded (KEYS-SPEC §K3).
	ErrFull = errors.New("key store full")

	// ErrIncorrectPassphrase is returned when passphrase decryption fails.
	ErrIncorrectPassphrase = errors.New("incorrect passphrase")

	// ErrUnexpectedFile is returned when state root violates the C2 file law.
	ErrUnexpectedFile = errors.New("unexpected file in state root")

	// ErrSnapshotNotFound is returned when requested snapshot generation does not exist.
	ErrSnapshotNotFound = errors.New("snapshot not found")

	// ErrSnapshotCorrupt is returned when snapshot MAC verification fails.
	ErrSnapshotCorrupt = errors.New("snapshot corrupt")
)

// Frozen audit kinds per CUSTOS-SPEC §8.1.
const (
	AuditKindCustosStarted             = "custos_started"
	AuditKindCustosStopped             = "custos_stopped"
	AuditKindUnlocked                  = "unlocked"
	AuditKindLockFailed                = "lock_failed"
	AuditKindVaultMigratedKeys         = "vault_migrated_keys"
	AuditKindCredentialAdded           = "credential_added" //nolint:gosec // frozen audit kind name
	AuditKindLoginDenied               = "login_denied"
	AuditKindCredentialRotated         = "credential_rotated" //nolint:gosec // frozen audit kind name
	AuditKindCredentialRemoved         = "credential_removed" //nolint:gosec // frozen audit kind name
	AuditKindSurrogateCreated          = "surrogate_created"
	AuditKindSurrogateRevoked          = "surrogate_revoked"
	AuditKindSurrogateRejected         = "surrogate_rejected"
	AuditKindRegistryReconciled        = "registry_reconciled"
	AuditKindVaultMutationIntent       = "vault_mutation_intent"
	AuditKindVaultMutation             = "vault_mutation"
	AuditKindVaultMutationRecovered    = "vault_mutation_recovered"
	AuditKindVaultMutationAborted      = "vault_mutation_aborted"
	AuditKindVaultMutationSuperseded   = "vault_mutation_superseded"
	AuditKindStaleVerdict              = "stale_verdict"
	AuditKindListenerBindFailed        = "listener_bind_failed"
	AuditKindSwapAllowed               = "swap_allowed"
	AuditKindSwapDenied                = "swap_denied"
	AuditKindEgressAllowed             = "egress_allowed"
	AuditKindEgressDenied              = "egress_denied"
	AuditKindWorkerCallAllowed         = "worker_call_allowed"
	AuditKindWorkerCallDenied          = "worker_call_denied"
	AuditKindPolicyWritten             = "policy_written"
	AuditKindAlwaysRuleAdded           = "always_rule_added"
	AuditKindPolicyReset               = "policy_reset"
	AuditKindCustosRestartedAfterCrash = "custos_restarted_after_crash"
	AuditKindAuditPruned               = "audit_pruned"
)

// State is the daemon lifecycle state per CUSTOS-SPEC §11.
type State string

// Daemon status states per CUSTOS-SPEC §11.
const (
	StateLocked   State = "locked"
	StateUnlocked State = "unlocked"
	StateDegraded State = "degraded"
)

// Status reports the JSON status structure per CUSTOS-SPEC §11.
type Status struct {
	State             State  `json:"state"`
	Credentials       int    `json:"credentials"`
	Surrogates        int    `json:"surrogates"`
	SurrogatesDropped int    `json:"surrogates_dropped"`
	Door              string `json:"door"` // "custosd" | "none"
	ListenersFailed   int    `json:"listeners_failed"`
}

// Credential represents one stored secret in the vault per CUSTOS-SPEC §4.1, §4.5.
type Credential struct {
	Kind   string `json:"kind"` // "api_key" | "oauth2"
	Secret string `json:"secret,omitempty"`

	// OAuth2 fields (§4.5):
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	AccessExpiry string `json:"access_expiry,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
	TokenURI     string `json:"token_uri,omitempty"`
	AuthURI      string `json:"auth_uri,omitempty"`
	GrantedAt    string `json:"granted_at,omitempty"`
}

// VaultDoc represents the authenticated JSON vault document inside vault.age per CUSTOS-SPEC §4.1.
type VaultDoc struct {
	Version     int                   `json:"version"`
	Generation  int64                 `json:"generation"`
	Credentials map[string]Credential `json:"credentials"`
}
