package custos

import (
	"time"
)

// Default configuration constants per CUSTOS-SPEC §4.2, §7, §8.3, §12.
const (
	DefaultLockWaitTimeout    = 10 * time.Second  // CUSTOS §4.2
	DefaultAuditRetentionDays = 365               // CUSTOS §8.3, §12 Q5
	DefaultAskHoldTimeout     = 330 * time.Second // CUSTOS §7
	DefaultMaxParkedPerCell   = 16                // CUSTOS §7
	DefaultMaxParkedGlobal    = 128               // CUSTOS §7

	// DefaultWorkerExecTimeout bounds one worker tool execution after the
	// ask hold settles (§5.1: Execute budget, distinct from the hold
	// budget; reusing the 330s hold for execution stacked two full
	// windows onto one connection).
	DefaultWorkerExecTimeout = 60 * time.Second
)

// Config represents the custos block in lararium.yaml per CUSTOS-SPEC §4.1, §8.3, §12.
// No new config keys are declared beyond the spec.
type Config struct {
	// UnlockKeyfile is the optional path to a keyfile for unlocked-from-boot mode (CUSTOS §4.1, §C3, §12 Q1).
	UnlockKeyfile string `yaml:"unlock_keyfile"`

	// LockWaitTimeout is the acquisition timeout for custos.lock (CUSTOS §4.2).
	LockWaitTimeout time.Duration `yaml:"lock_wait_timeout"`

	// AuditRetentionDays is the retention window for daily audit files (CUSTOS §8.3, §12 Q5).
	AuditRetentionDays int `yaml:"audit_retention_days"`

	// AskHoldTimeout is the maximum duration an ask flow may park before timing out (CUSTOS §7).
	AskHoldTimeout time.Duration `yaml:"ask_hold_timeout"`

	// WorkerExecTimeout bounds one worker-lane tool execution after the
	// ask settles (§5.1; separate budget from the hold).
	WorkerExecTimeout time.Duration `yaml:"worker_exec_timeout"`

	// MaxParkedPerCell is the per-cell limit for parked ask flows (CUSTOS §7).
	MaxParkedPerCell int `yaml:"max_parked_per_cell"`

	// MaxParkedGlobal is the host-wide ceiling for parked ask flows (CUSTOS §7).
	MaxParkedGlobal int `yaml:"max_parked_global"`

	// HeaderExtras is the list of additional request header names to inspect for surrogates (CUSTOS §5.2).
	HeaderExtras []string `yaml:"header_extras"`
}

// Normalize sets specification defaults for zero-valued configuration fields.
func (c *Config) Normalize() {
	if c.LockWaitTimeout <= 0 {
		c.LockWaitTimeout = DefaultLockWaitTimeout
	}
	if c.AuditRetentionDays <= 0 {
		c.AuditRetentionDays = DefaultAuditRetentionDays
	}
	if c.AskHoldTimeout <= 0 {
		c.AskHoldTimeout = DefaultAskHoldTimeout
	}
	if c.WorkerExecTimeout <= 0 {
		c.WorkerExecTimeout = DefaultWorkerExecTimeout
	}
	if c.MaxParkedPerCell <= 0 {
		c.MaxParkedPerCell = DefaultMaxParkedPerCell
	}
	if c.MaxParkedGlobal <= 0 {
		c.MaxParkedGlobal = DefaultMaxParkedGlobal
	}
}
