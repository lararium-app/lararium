// Dynamic connector lifecycle on custosd per CONNECTOR-LIFE-SPEC D1/D2
// (MCP-SPEC amendment A10): MCP-ADD / MCP-PROBE / MCP-REMOVE / MCP-STATE
// execution, strict JSON config parsing (same grammar as the YAML lane),
// vault promotion of literal secrets, and the boot merge with the runtime
// store. The policy gate (probe-then-card) lives in internal/surface, not here.

package custos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// probeFrameName is the connector slot of the connection-local MCP-TOOLS
// frame that follows an MCP-PROBE reply. It is deliberately outside the name
// grammar so it can never collide with a real connector.
const probeFrameName = "_probe"

// maxConcurrentProbes bounds simultaneous probe children.
const maxConcurrentProbes = 4

// lifecycleError is a closed, secret-free refusal reason surfaced as
// "ERR <reason>".
type lifecycleError struct{ reason string }

func (e *lifecycleError) Error() string { return e.reason }

func lcErr(reason string) error { return &lifecycleError{reason: reason} }

func lifecycleReason(err error) string {
	var le *lifecycleError
	if errors.As(err, &le) {
		return le.reason
	}
	if r := ErrorReason(err); r != "" {
		return r
	}
	return "internal"
}

// jsonMCPServerCfg is the strict JSON twin of the YAML entry shape
// (MCP-SPEC §3). list_timeout is a Go duration string ("30s").
type jsonMCPServerCfg struct {
	Name               string            `json:"name"`
	Transport          string            `json:"transport"`
	Command            string            `json:"command"`
	Args               []string          `json:"args"`
	Env                map[string]string `json:"env"`
	URL                string            `json:"url"`
	Bearer             string            `json:"bearer"`
	ToolAllowlist      []string          `json:"tool_allowlist"`
	ToolDenylist       []string          `json:"tool_denylist"`
	MaxConcurrentCalls *int              `json:"max_concurrent_calls"`
	ListTimeout        *string           `json:"list_timeout"`
}

// ParseMCPConfigJSON strictly parses and validates one single-line JSON
// connector config with exactly the YAML lane's rules: unknown keys are
// rejected (unknown_key), the name grammar and reserved names are enforced,
// and transport/url/command/max_concurrent checks are shared.
func ParseMCPConfigJSON(data []byte) (MCPServerConfig, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return MCPServerConfig{}, lcErr("bad_request")
	}
	for k := range top {
		if !knownMCPKeys[k] {
			return MCPServerConfig{}, &ConfigError{
				Reason:  ReasonUnknownKey,
				Message: fmt.Sprintf("unknown key %q in mcp entry", k),
			}
		}
	}
	var j jsonMCPServerCfg
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		// Same mapping as the YAML lane's decode failures.
		return MCPServerConfig{}, &ConfigError{Reason: ReasonUnknownKey, Message: "decode mcp entry"}
	}
	raw := rawMCPServerCfg{
		Name:               j.Name,
		Transport:          j.Transport,
		Command:            j.Command,
		Args:               j.Args,
		Env:                j.Env,
		URL:                j.URL,
		Bearer:             j.Bearer,
		ToolAllowlist:      j.ToolAllowlist,
		ToolDenylist:       j.ToolDenylist,
		MaxConcurrentCalls: j.MaxConcurrentCalls,
	}
	if j.ListTimeout != nil {
		d, err := time.ParseDuration(*j.ListTimeout)
		if err != nil {
			return MCPServerConfig{}, &ConfigError{Reason: ReasonUnknownKey, Message: "decode mcp entry: list_timeout"}
		}
		raw.ListTimeout = &d
	}
	out, err := validateRawMCPEntries([]rawMCPServerCfg{raw})
	if err != nil {
		return MCPServerConfig{}, err
	}
	return out[0], nil
}

func isRefExpr(v string) bool {
	return (strings.HasPrefix(v, "${vault:") || strings.HasPrefix(v, "${env:")) && strings.HasSuffix(v, "}")
}

func sanitizeCredPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// promotedCredName derives the deterministic connector-scoped vault name for
// one secret-carrying field. It always lands inside the K4 grammar
// ^[a-z0-9][a-z0-9_-]{0,63}$ and carries a hash suffix so distinct
// (connector, field) pairs can never collide after sanitizing/truncating.
func promotedCredName(connector, field string) string {
	sum := sha256.Sum256([]byte("mcp-promote\x00" + connector + "\x00" + field))
	suffix := hex.EncodeToString(sum[:4])
	base := "mcp-" + sanitizeCredPart(connector) + "-" + sanitizeCredPart(field)
	if maxBase := 64 - 1 - len(suffix); len(base) > maxBase {
		base = base[:maxBase]
	}
	return base + "-" + suffix
}

type promotion struct {
	name    string
	literal string
}

// promoteLiterals replaces literal secrets in cfg (bearer, env values that
// are not ${vault:}/${env:} references) with ${vault:<derived-name>} refs and
// writes the literals into the vault in ONE mutation (all or nothing). The
// returned config is the one that may be stored. Literals never reach errors.
func promoteLiterals(v *Vault, cfg MCPServerConfig) (MCPServerConfig, error) {
	var proms []promotion
	out := cfg
	if cfg.Bearer != "" && !isRefExpr(cfg.Bearer) {
		name := promotedCredName(cfg.Name, "bearer")
		proms = append(proms, promotion{name: name, literal: cfg.Bearer})
		out.Bearer = "${vault:" + name + "}"
	}
	if len(cfg.Env) > 0 {
		out.Env = make(map[string]string, len(cfg.Env))
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := cfg.Env[k]
			if val == "" || isRefExpr(val) {
				out.Env[k] = val
				continue
			}
			name := promotedCredName(cfg.Name, "env:"+k)
			proms = append(proms, promotion{name: name, literal: val})
			out.Env[k] = "${vault:" + name + "}"
		}
	}
	if len(proms) == 0 {
		return out, nil
	}
	if v == nil || !v.IsUnlocked() {
		return MCPServerConfig{}, lcErr("locked")
	}
	var changed []string
	existed := make(map[string]bool)
	err := v.Mutate("", func(doc *VaultDoc) ([]string, error) {
		if doc.Credentials == nil {
			doc.Credentials = make(map[string]Credential)
		}
		changed = changed[:0]
		fresh := 0
		for _, p := range proms {
			cur, ok := doc.Credentials[p.name]
			if !ok {
				fresh++
				continue
			}
			if cur.Kind != "api_key" {
				return nil, lcErr("vault_conflict")
			}
		}
		if len(doc.Credentials)+fresh > maxCredentialNames {
			return nil, ErrFull
		}
		for _, p := range proms {
			cur, ok := doc.Credentials[p.name]
			if ok && cur.Secret == p.literal {
				continue // idempotent re-ADD of the same literal
			}
			existed[p.name] = ok
			doc.Credentials[p.name] = Credential{Kind: "api_key", Secret: p.literal}
			changed = append(changed, p.name)
		}
		return append([]string(nil), changed...), nil
	}, false)
	if err != nil {
		var le *lifecycleError
		switch {
		case errors.As(err, &le):
			return MCPServerConfig{}, le
		case errors.Is(err, ErrCustosLocked):
			return MCPServerConfig{}, lcErr("locked")
		case errors.Is(err, ErrFull):
			return MCPServerConfig{}, lcErr("store_full")
		default:
			return MCPServerConfig{}, lcErr("vault_failed")
		}
	}
	for _, n := range changed {
		kind := AuditKindCredentialAdded
		if existed[n] {
			kind = AuditKindCredentialRotated
		}
		_ = v.Audit().Append(AuditRecord{Kind: kind, Cred: n, Actor: "door"})
	}
	return out, nil
}

func envNames(cfg MCPServerConfig) []string {
	if len(cfg.Env) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// NewRailRegistryFromStore builds the boot registry: the lararium.yaml seed,
// overridden by the runtime store, with tombstoned names dropped (D2).
// Store-disabled connectors are constructed user-disabled so boot never
// starts them; persisted last_used is restored.
func NewRailRegistryFromStore(seed []MCPServerConfig, store *ConnectorStore, hearthDir string, opts ...Option) *RailRegistry {
	var merged []MergedConnector
	if store != nil {
		merged = store.Merge(seed)
	} else {
		for _, cfg := range seed {
			merged = append(merged, MergedConnector{Config: cfg, Enabled: true})
		}
	}
	cfgs := make([]MCPServerConfig, 0, len(merged))
	for _, m := range merged {
		cfgs = append(cfgs, m.Config)
	}
	reg := NewRailRegistry(cfgs, hearthDir, opts...)
	for _, m := range merged {
		c := reg.Connector(m.Config.Name)
		if c == nil {
			continue
		}
		if !m.Enabled {
			c.StartUserDisabled()
		}
		if m.LastUsed != nil {
			c.SetLastUsed(*m.LastUsed)
		}
	}
	return reg
}

// ConnectorManager executes the door lifecycle verbs against the live
// registry, the worker lane and the runtime store.
type ConnectorManager struct {
	mu        sync.Mutex
	reg       *RailRegistry
	store     *ConnectorStore
	vault     *Vault
	workers   *WorkerServer
	hearthDir string
	probeSem  chan struct{}
}

// NewConnectorManager wires a manager. vault and workers may be nil in
// harnesses (literal promotion then refuses).
func NewConnectorManager(reg *RailRegistry, store *ConnectorStore, v *Vault, ws *WorkerServer, hearthDir string) *ConnectorManager {
	return &ConnectorManager{
		reg:       reg,
		store:     store,
		vault:     v,
		workers:   ws,
		hearthDir: hearthDir,
		probeSem:  make(chan struct{}, maxConcurrentProbes),
	}
}

func (m *ConnectorManager) audit(rec AuditRecord) {
	if m.vault == nil || m.vault.Audit() == nil {
		return
	}
	_ = m.vault.Audit().Append(rec)
}

func (m *ConnectorManager) locked() bool {
	return m.vault != nil && !m.vault.IsUnlocked()
}

func (m *ConnectorManager) lastUsedOf(name string) *time.Time {
	if c := m.reg.Connector(name); c != nil {
		return c.LastUsed()
	}
	return nil
}

func (m *ConnectorManager) saveStore() error {
	var live []string
	for _, c := range m.reg.Connectors() {
		live = append(live, c.Name())
	}
	return m.store.Save(m.lastUsedOf, live)
}

func (m *ConnectorManager) newConnector(cfg MCPServerConfig) *MCPConnector {
	conn := NewMCPConnector(cfg, m.hearthDir, railOptionsForVault(m.vault)...)
	if m.workers != nil {
		conn.setWorkers(m.workers)
	}
	return conn
}

// Add implements MCP-ADD: add-or-replace, persist, start.
func (m *ConnectorManager) Add(name string, configJSON []byte) error {
	cfg, err := ParseMCPConfigJSON(configJSON)
	if err != nil {
		return lcErr(lifecycleReason(err))
	}
	if cfg.Name != name {
		return lcErr(ReasonBadName)
	}
	if m.locked() {
		return lcErr("locked")
	}
	if m.reg == nil || m.store == nil {
		return lcErr("no_registry")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stored, err := promoteLiterals(m.vault, cfg)
	if err != nil {
		return lcErr(lifecycleReason(err))
	}

	old := m.reg.Connector(name)
	rec := RecordFromConfig(stored)
	prev, hadPrev, wasTomb := m.store.Get(name)
	if hadPrev {
		rec.LastUsed = prev.LastUsed
	}
	if old != nil {
		if t := old.LastUsed(); t != nil {
			s := t.UTC().Format(time.RFC3339)
			rec.LastUsed = &s
		}
	}
	m.store.Put(rec)
	if err := m.saveStore(); err != nil {
		m.store.Restore(name, prev, hadPrev, wasTomb)
		return lcErr("store_failed")
	}

	// Stop-and-replace: the old child is fully gone before the new one
	// starts (no dual occupancy).
	if old != nil {
		old.Shutdown(false)
	}
	conn := m.newConnector(stored)
	if old != nil {
		if t := old.LastUsed(); t != nil {
			conn.SetLastUsed(*t)
		}
	}
	m.reg.Replace(conn)
	if m.workers != nil {
		_ = m.workers.RegisterConnector(conn)
	}
	// Same start path as the boot lane (occupancy refusal, identity slot,
	// spawn, first tools/list within list_timeout).
	_ = conn.OnFirstUnlock(context.Background())
	m.reg.NotifyChanged(name, true)

	m.audit(AuditRecord{
		Kind:        AuditKindMCPAdded,
		Cred:        name,
		Actor:       "door",
		Names:       envNames(stored),
		Fingerprint: conn.Fingerprint(),
	})
	return nil
}

// Remove implements MCP-REMOVE: always allowed, idempotent.
func (m *ConnectorManager) Remove(name string) error {
	if m.reg == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	conn := m.reg.Unregister(name)
	if conn == nil {
		return nil
	}
	fp := conn.Fingerprint()
	env := envNames(conn.Config())
	conn.Shutdown(false)
	if m.workers != nil {
		m.workers.UnregisterConnector(name)
	}
	var saveErr error
	if m.store != nil {
		m.store.Remove(name)
		saveErr = m.saveStore()
	}
	m.reg.NotifyChanged(name, false)
	m.audit(AuditRecord{
		Kind:        AuditKindMCPRemoved,
		Cred:        name,
		Actor:       "door",
		Names:       env,
		Fingerprint: fp,
	})
	if saveErr != nil {
		return lcErr("store_failed")
	}
	return nil
}

// SetState implements MCP-STATE: user enable/disable toggle, persisted.
func (m *ConnectorManager) SetState(name string, enabled bool) error {
	if m.reg == nil {
		return lcErr("not_found")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	conn := m.reg.Connector(name)
	if conn == nil {
		return lcErr("not_found")
	}
	if enabled && m.locked() {
		return lcErr("locked")
	}
	if m.store != nil {
		prev, hadPrev, wasTomb := m.store.Get(name)
		m.store.SetEnabled(name, enabled)
		if err := m.saveStore(); err != nil {
			m.store.Restore(name, prev, hadPrev, wasTomb)
			return lcErr("store_failed")
		}
	}
	if err := m.reg.SetUserEnabled(context.Background(), name, enabled); err != nil {
		return lcErr("internal")
	}
	reason := "disabled"
	if enabled {
		reason = "enabled"
	}
	m.audit(AuditRecord{
		Kind:        AuditKindMCPToggled,
		Cred:        name,
		Actor:       "door",
		Reason:      reason,
		Names:       envNames(conn.Config()),
		Fingerprint: conn.Fingerprint(),
	})
	return nil
}

// ProbeResult is a successful MCP-PROBE outcome.
type ProbeResult struct {
	Tools       []ToolStatus
	Fingerprint string
}

// Probe implements MCP-PROBE: boot a throwaway child connector from configJSON,
// run one tools/list, compute the catalog fingerprint, then kill the process
// group and reap it on every path. It never registers, persists, audits, or
// touches policy; it does not use the identity slot or the live connector dir.
func (m *ConnectorManager) Probe(configJSON []byte) (*ProbeResult, error) {
	cfg, err := ParseMCPConfigJSON(configJSON)
	if err != nil {
		return nil, lcErr(lifecycleReason(err))
	}
	select {
	case m.probeSem <- struct{}{}:
		defer func() { <-m.probeSem }()
	default:
		return nil, lcErr("busy")
	}

	tmp, err := os.MkdirTemp("", "custos-probe-")
	if err != nil {
		return nil, lcErr("internal")
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var opts []Option
	if m.vault != nil {
		opts = append(opts, WithEnvResolver(newEnvResolver(m.vault)))
	}
	// No retry timers: a failed probe boot must not reschedule itself.
	opts = append(opts, WithAfterFunc(func(time.Duration, func()) *time.Timer {
		return time.NewTimer(time.Hour)
	}))
	conn := NewMCPConnector(cfg, filepath.Clean(tmp), opts...)

	listTimeout := cfg.ListTimeout
	if listTimeout <= 0 {
		listTimeout = DefaultMCPListTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout+5*time.Second)
	defer cancel()

	conn.mu.Lock()
	_ = conn.startLocked(ctx)
	state, reason := conn.state, conn.reason
	var res *ProbeResult
	if state == MCPStateReady {
		snap := conn.snapshot.Load()
		tools := conn.ToolStatuses()
		if tools == nil {
			tools = []ToolStatus{}
		}
		res = &ProbeResult{Tools: tools, Fingerprint: snap.CatalogFingerprint()}
	}
	// Kill + reap on ALL paths (ready, backoff, disabled, timeout).
	conn.cancelRetryLocked()
	conn.state = MCPStateDisabled
	conn.reason = MCPReasonUserDisabled
	conn.teardownChildLocked()
	conn.mu.Unlock()

	if res == nil {
		if reason == "" || reason == MCPReasonOK {
			reason = MCPReasonProtocol
		}
		return nil, lcErr(reason)
	}
	return res, nil
}
