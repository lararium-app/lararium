package custos

import (
	"fmt"
	"os"
	"strings"

	"github.com/lararium-app/lararium/internal/surface"
)

// Daemon coordinates the background custosd process per CUSTOS-SPEC §3.
type Daemon struct {
	cfg       *Config
	vault     *Vault
	hub       *surface.ApprovalHub
	proxy     *Proxy
	ctlServer *CtlServer
	stopChan  chan struct{}
}

// NewDaemon initializes a Daemon with configuration.
func NewDaemon(stateDir string, cfg *Config) *Daemon {
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Normalize()

	v := NewVault(stateDir, cfg.LockWaitTimeout)
	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	v.SetApprovalHub(hub)
	proxy := NewProxy(v, hub, cfg)

	return &Daemon{
		cfg:      cfg,
		vault:    v,
		hub:      hub,
		proxy:    proxy,
		stopChan: make(chan struct{}),
	}
}

// Vault returns the internal Vault.
func (d *Daemon) Vault() *Vault {
	return d.vault
}

// Proxy returns the internal Proxy.
func (d *Daemon) Proxy() *Proxy {
	return d.proxy
}

// Hub returns the internal ApprovalHub.
func (d *Daemon) Hub() *surface.ApprovalHub {
	return d.hub
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
	ctlServer.SetProxy(d.proxy)
	d.ctlServer = ctlServer

	return nil
}

// Stop initiates graceful shutdown, auditing custos_stopped per CUSTOS §8.1.
func (d *Daemon) Stop() {
	if d.ctlServer != nil {
		_ = d.ctlServer.Close()
	}
	if d.proxy != nil {
		_ = d.proxy.Close()
	}
	if d.hub != nil {
		d.hub.DenyAllAll("shutdown", "shutdown")
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
