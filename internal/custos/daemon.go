package custos

import (
	"fmt"
	"os"
	"strings"
)

// Daemon coordinates the background custosd process per CUSTOS-SPEC §3.
type Daemon struct {
	cfg       *Config
	vault     *Vault
	ctlServer *CtlServer
	stopChan  chan struct{}
}

// NewDaemon initializes a Daemon with configuration.
func NewDaemon(stateDir string, cfg *Config) *Daemon {
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Normalize()

	return &Daemon{
		cfg:      cfg,
		vault:    NewVault(stateDir, cfg.LockWaitTimeout),
		stopChan: make(chan struct{}),
	}
}

// Vault returns the internal Vault.
func (d *Daemon) Vault() *Vault {
	return d.vault
}

// Start boots custosd: checks C2 file law, checks crash doctrine, serves locked (or unlocks in keyfile mode), binds ctl.sock.
func (d *Daemon) Start() error {
	// 1. CUSTOS §C2: enforce state root and file law
	if err := d.vault.CheckFileLaw(); err != nil {
		return err
	}

	// 2. CUSTOS §C4: crash doctrine
	// Restart appends custos_restarted_after_crash if previous run left no clean custos_stopped tail
	lastRec, err := d.vault.Audit().ReadTailRecord()
	if err == nil && lastRec != nil && lastRec.Kind != AuditKindCustosStopped {
		_ = d.vault.Audit().Append(AuditRecord{
			Kind:  AuditKindCustosRestartedAfterCrash,
			Actor: "daemon",
		})
	}

	// 3. Keyfile mode check (CUSTOS §4.1, §C3, §12 Q1)
	if d.cfg.UnlockKeyfile != "" {
		keyBytes, err := os.ReadFile(d.cfg.UnlockKeyfile)
		if err != nil {
			return fmt.Errorf("read unlock keyfile %s: %w", d.cfg.UnlockKeyfile, err)
		}
		passphrase := strings.TrimSpace(string(keyBytes))
		// CUSTOS-SPEC §C3: keyfile doctrine — read once at spawn and zeroed
		zeroBytes(keyBytes)
		if passphrase == "" {
			return fmt.Errorf("empty unlock keyfile %s", d.cfg.UnlockKeyfile)
		}
		// CUSTOS §C3: spawn-time read IS the unlock
		if err := d.vault.Unlock(passphrase, true); err != nil {
			return fmt.Errorf("keyfile unlock: %w", err)
		}
	}

	// 4. Audit custos_started
	_ = d.vault.Audit().Append(AuditRecord{
		Kind:  AuditKindCustosStarted,
		Actor: "daemon",
	})

	// 5. Start ctl.sock listener
	ctlServer, err := StartCtlServer(d.vault.StateDir(), d.vault, d.Stop)
	if err != nil {
		return fmt.Errorf("start ctl server: %w", err)
	}
	d.ctlServer = ctlServer

	return nil
}

// Stop initiates graceful shutdown, auditing custos_stopped per CUSTOS §8.1.
func (d *Daemon) Stop() {
	if d.ctlServer != nil {
		_ = d.ctlServer.Close()
	}
	_ = d.vault.Audit().Append(AuditRecord{
		Kind:  AuditKindCustosStopped,
		Actor: "daemon",
	})
	select {
	case <-d.stopChan:
	default:
		close(d.stopChan)
	}
}

// Wait blocks until the daemon terminates.
func (d *Daemon) Wait() {
	<-d.stopChan
}
