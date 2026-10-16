// MCP connector lifecycle state machine, refresh, status, and registry
// per MCP-SPEC §4.3, §7, §8.3, §9, and CUSTOS-SPEC §8.1.

package custos

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Lifecycle states per MCP-SPEC §7.
const (
	MCPStateStarting = "starting"
	MCPStateReady    = "ready"
	MCPStateBackoff  = "backoff"
	MCPStateDisabled = "disabled"
)

// MCP-STATUS state-reasons per MCP-SPEC §9.
const (
	MCPReasonOK                    = "ok"
	MCPReasonAuthFailed            = "auth_failed"
	MCPReasonSpawnFailed           = "spawn_failed"
	MCPReasonListTimeout           = "list_timeout"
	MCPReasonProtocol              = "protocol"
	MCPReasonTransport             = "transport"
	MCPReasonNameOccupied          = "name_occupied"
	MCPReasonCredentialUnavailable = "credential_unavailable" //nolint:gosec // frozen reason constant, not a credential
)

// OnFirstUnlock performs the first-unlock sequence per MCP-SPEC §7:
//  1. Occupancy refusal check (rail name held as vault cred name -> disabled /
//     name_occupied)
//  2. Ensure identity slot (latched once per process)
//  3. Spawn (stdio) and first tools/list within list_timeout.
func (c *MCPConnector) OnFirstUnlock(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. Occupancy refusal check: vault names opaque while sealed,
	// checked unsealed
	if c.vaultGate != nil {
		occupied, err := c.vaultGate.IsOccupied(c.cfg.Name)
		if err == nil && occupied {
			c.state = MCPStateDisabled
			c.reason = MCPReasonNameOccupied
			c.cancelRetryLocked()
			return nil
		}
	}

	// 2. Ensure identity slot (latched once per process)
	if !c.ensured && c.vaultGate != nil {
		if err := c.vaultGate.EnsureSlot(c.cfg.Name); err != nil {
			return fmt.Errorf("ensure identity slot: %w", err)
		}
		c.ensured = true
	}

	// 3. Start connector (spawn and first list)
	return c.startLocked(ctx)
}

// startLocked spawns the child process and runs first tools/list within
// list_timeout.
//
//nolint:contextcheck // background retry timers and child monitors outlive startLocked context
func (c *MCPConnector) startLocked(ctx context.Context) error {
	if c.state == MCPStateDisabled && c.reason == MCPReasonNameOccupied {
		return nil
	}

	// Exit any previous live-child state with group-kill first (I7)
	c.teardownChildLocked()

	// Check 5 restarts rolling hour cap
	now := c.clock()
	var recent []time.Time
	for _, t := range c.restarts {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	c.restarts = recent

	if len(c.restarts) >= 5 {
		c.state = MCPStateDisabled
		if c.reason != MCPReasonProtocol {
			if c.reason == "" || c.reason == MCPReasonOK {
				c.reason = MCPReasonTransport
			}
		}
		c.cancelRetryLocked()
		return nil
	}
	c.restarts = append(c.restarts, now)

	// Compose child environment and resolve references
	connectorDir := filepath.Join(c.hearthDir, "connectors", c.cfg.Name)
	childEnv, needles, literalKeys, err := ComposeChildEnv(&c.cfg, connectorDir, c.envResolver)
	if err != nil {
		// Unresolved vault reference fails to disabled / credential_unavailable
		// per §3, §7
		c.state = MCPStateDisabled
		c.reason = MCPReasonCredentialUnavailable
		c.cancelRetryLocked()
		return nil
	}
	c.needles = needles

	// Audit literal env keys once per start per §3, A8
	if len(literalKeys) > 0 && c.auditSink != nil {
		sort.Strings(literalKeys)
		_ = c.auditSink.Append(AuditRecord{
			Kind:  AuditKindMCPConfigLiteralEnv,
			Cred:  c.cfg.Name,
			Host:  c.Host(),
			Names: literalKeys,
		})
	}

	// Create rotating stderr writer
	stderrWriter, err := NewRotatingStderrWriter(connectorDir, needles)
	if err != nil {
		c.transitionBackoffLocked(MCPReasonSpawnFailed)
		return nil
	}

	// Spawn child process with Setpgid: true
	child, stdout, stdin, err := SpawnChildProcess( //nolint:contextcheck // child process outlives startLocked context
		c.cfg.Command,
		c.cfg.Args,
		childEnv,
		connectorDir,
		stderrWriter,
		stderrWriter,
	)
	if err != nil {
		_ = stderrWriter.Close()
		c.transitionBackoffLocked(MCPReasonSpawnFailed)
		return nil
	}
	c.childProc.Store(child)

	// Connect MCP Go SDK client over stdio IOTransport
	transport := &mcp.IOTransport{
		Reader: stdout,
		Writer: stdin,
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "custos",
		Version: "v1.2",
	}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{
			Disabled: true, // v1 rejects MRTR
		},
		ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			// Schedule debounced async refresh per §4.3
			c.scheduleDebouncedRefresh()
		},
	})

	listTimeout := c.cfg.ListTimeout
	if listTimeout <= 0 {
		listTimeout = DefaultMCPListTimeout
	}
	listCtx, listCancel := context.WithTimeout(ctx, listTimeout)
	defer listCancel()

	session, err := client.Connect(listCtx, transport, nil)
	if err != nil {
		c.teardownChildLocked()
		if errors.Is(listCtx.Err(), context.DeadlineExceeded) {
			c.transitionBackoffLocked(MCPReasonListTimeout)
		} else {
			c.transitionBackoffLocked(MCPReasonSpawnFailed)
		}
		return nil
	}
	c.session.Store(session)

	// First tools/list
	listRes, err := session.ListTools(listCtx, nil)
	if err != nil {
		c.teardownChildLocked()
		if errors.Is(listCtx.Err(), context.DeadlineExceeded) {
			c.transitionBackoffLocked(MCPReasonListTimeout)
		} else {
			c.transitionBackoffLocked(MCPReasonSpawnFailed)
		}
		return nil
	}

	// Build and install initial snapshot
	ttl := time.Duration(listRes.GetTTLMs()) * time.Millisecond
	newSnap, err := BuildCatalogSnapshot(
		c.cfg.Name,
		listRes.Tools,
		c.cfg.ToolAllowlist,
		c.cfg.ToolDenylist,
		c.needles,
		ttl,
		c.clock(),
	)
	if err != nil {
		c.teardownChildLocked()
		c.transitionBackoffLocked(MCPReasonProtocol)
		return nil
	}

	// Audit dropped tools if any
	for _, raw := range newSnap.DroppedTools {
		if c.auditSink != nil {
			_ = c.auditSink.Append(AuditRecord{
				Kind:  AuditKindMCPToolDropped,
				Cred:  c.cfg.Name,
				Names: []string{raw},
			})
		}
	}

	c.snapshot.Store(newSnap)
	c.state = MCPStateReady
	c.reason = MCPReasonOK
	c.backoffDur = time.Second // reset backoff on success
	c.cancelRetryLocked()

	// Monitor child process death while ready
	go c.monitorChildCrash(child)

	return nil
}

func (c *MCPConnector) monitorChildCrash(child *ChildProcess) {
	<-child.WaitChan()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Only handle if this is still the active child
	if c.childProc.Load() != child {
		return
	}

	// Every exit from a live-child state kills the group before completing
	// transition (I7)
	c.teardownChildLocked()

	if c.state == MCPStateReady {
		c.transitionBackoffLocked(MCPReasonTransport)
	}
}

func (c *MCPConnector) transitionBackoffLocked(reason string) {
	c.state = MCPStateBackoff
	c.reason = reason

	// Check 5/hour cap
	now := c.clock()
	count := 0
	for _, t := range c.restarts {
		if now.Sub(t) < time.Hour {
			count++
		}
	}
	if count >= 5 {
		c.state = MCPStateDisabled
		if c.reason != MCPReasonProtocol {
			if c.reason == "" || c.reason == MCPReasonOK {
				c.reason = MCPReasonTransport
			}
		}
		c.cancelRetryLocked()
		return
	}

	retryDelay := c.backoffDur
	// Double backoff duration up to 5 minute cap
	c.backoffDur *= 2
	if c.backoffDur > 5*time.Minute {
		c.backoffDur = 5 * time.Minute
	}

	c.scheduleRetryLocked(retryDelay)
}

func (c *MCPConnector) teardownChildLocked() {
	if child := c.childProc.Swap(nil); child != nil {
		_ = child.KillGroup(c.sigkillWait)
	}
	if sess := c.session.Swap(nil); sess != nil {
		go func() {
			_ = sess.Close()
		}()
	}
}

func (c *MCPConnector) teardownChild() {
	c.teardownChildLocked()
}

func (c *MCPConnector) scheduleRetryLocked(delay time.Duration) {
	c.cancelRetryLocked()
	if c.state != MCPStateBackoff {
		return
	}
	c.retryTimer = c.afterFunc(delay, func() {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()

		c.mu.Lock()
		defer c.mu.Unlock()

		if c.state != MCPStateBackoff {
			return
		}
		c.retryTimer = nil
		_ = c.startLocked(context.Background())
	})
}

func (c *MCPConnector) cancelRetryLocked() {
	if c.retryTimer != nil {
		c.retryTimer.Stop()
		c.retryTimer = nil
	}
}

// scheduleDebouncedRefresh coalesces notifications per §4.3:
// if a refresh is already in flight, mark pending and run one more after it finishes;
// extra notifications during that are no-ops. Never blocks the notification callback.
func (c *MCPConnector) scheduleDebouncedRefresh() {
	c.debounceMu.Lock()
	if c.debRunning {
		c.debPending = true
		c.debounceMu.Unlock()
		return
	}
	c.debRunning = true
	c.debounceMu.Unlock()

	go func() {
		for {
			_ = c.Refresh(context.Background())

			c.debounceMu.Lock()
			if !c.debPending {
				c.debRunning = false
				c.debounceMu.Unlock()
				return
			}
			c.debPending = false
			c.debounceMu.Unlock()
		}
	}()
}

// NotifyCredentialGone handles credential deletion per MCP-SPEC §6:
// 1. Marks connector disabled with reason credential_unavailable under short lock
// 2. Kills child process group without holding c.mu across network/process I/O
// 3. Retains needles until in-flight calls settle.
func (c *MCPConnector) NotifyCredentialGone(name string) {
	matches := c.cfg.Name == name
	if !matches {
		target := fmt.Sprintf("${vault:%s}", name)
		for _, v := range c.cfg.Env {
			if v == target {
				matches = true
				break
			}
		}
		if c.cfg.Bearer == target {
			matches = true
		}
	}

	if !matches {
		return
	}

	// (a) Mark disabled, reason credential_unavailable under short lock
	c.mu.Lock()
	c.state = MCPStateDisabled
	c.reason = MCPReasonCredentialUnavailable
	c.cancelRetryLocked()
	c.mu.Unlock()

	// (b) Kill child process group without c.mu so kill path never lags behind network I/O
	c.teardownChild()

	// (c) Needles retained until inFlight drains to 0
	c.mu.Lock()
	for c.inFlight > 0 {
		c.drainCond.Wait()
	}
	c.mu.Unlock()
}

// Refresh re-lists tools from the live server per MCP-SPEC §4.3 / amendment A3.
// Automatic refresh: never clears the restart budget; if disabled, it does nothing.
func (c *MCPConnector) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	// Automatic refresh against a disabled connector must be a no-op
	if c.state == MCPStateDisabled {
		c.mu.Unlock()
		return nil
	}

	// Refresh re-ensures the identity slot if deleted
	if c.vaultGate != nil {
		_ = c.vaultGate.EnsureSlot(c.cfg.Name)
	}

	// If not live, re-start (without laundering budget)
	session := c.session.Load()
	if c.state != MCPStateReady || session == nil {
		err := c.startLocked(ctx)
		c.mu.Unlock()
		return err
	}

	needles := append([]string(nil), c.needles...)
	c.mu.Unlock()

	return c.refreshLive(ctx, session, needles)
}

// ManualRefresh handles explicit manual refresh per §7:
// resets restarts and backoff duration first, then starts/refreshes.
func (c *MCPConnector) ManualRefresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	// If disabled by occupancy refusal, cannot refresh
	if c.state == MCPStateDisabled && c.reason == MCPReasonNameOccupied {
		c.mu.Unlock()
		return errors.New("connector name occupied in vault")
	}

	// Manual refresh explicitly clears the restart budget and backoff duration per §7
	c.restarts = nil
	c.backoffDur = time.Second
	c.cancelRetryLocked()

	// Refresh re-ensures the identity slot if deleted
	if c.vaultGate != nil {
		_ = c.vaultGate.EnsureSlot(c.cfg.Name)
	}

	// If not live, start
	session := c.session.Load()
	if c.state != MCPStateReady || session == nil {
		err := c.startLocked(ctx)
		c.mu.Unlock()
		return err
	}

	needles := append([]string(nil), c.needles...)
	c.mu.Unlock()

	return c.refreshLive(ctx, session, needles)
}

func (c *MCPConnector) refreshLive(ctx context.Context, session *mcp.ClientSession, needles []string) error {
	listTimeout := c.cfg.ListTimeout
	if listTimeout <= 0 {
		listTimeout = DefaultMCPListTimeout
	}
	listCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	listRes, err := session.ListTools(listCtx, nil)
	if err != nil {
		c.mu.Lock()
		if c.state == MCPStateDisabled {
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		return fmt.Errorf("list tools during refresh: %w", err)
	}

	oldSnap := c.snapshot.Load()
	ttl := time.Duration(listRes.GetTTLMs()) * time.Millisecond
	newSnap, err := BuildCatalogSnapshot(
		c.cfg.Name,
		listRes.Tools,
		c.cfg.ToolAllowlist,
		c.cfg.ToolDenylist,
		needles,
		ttl,
		c.clock(),
	)
	if err != nil {
		return fmt.Errorf("build snapshot during refresh: %w", err)
	}

	c.mu.Lock()
	if c.state == MCPStateDisabled {
		c.mu.Unlock()
		return nil
	}
	c.snapshot.Store(newSnap)
	c.mu.Unlock()

	// Catalog diff audit + revocation
	diff := CatalogDiff(c.cfg.Name, oldSnap, newSnap)
	if len(diff) > 0 && c.auditSink != nil {
		_ = c.auditSink.Append(AuditRecord{
			Kind:  AuditKindMCPCatalogChanged,
			Cred:  c.cfg.Name,
			Names: diff,
		})
	}

	// Revoke auto-rules for removed or fingerprint-changed tools per A6
	if c.revoker != nil && oldSnap != nil {
		for toolName, oldFp := range oldSnap.Fingerprints {
			newFp, exists := newSnap.Fingerprints[toolName]
			if !exists || newFp != oldFp {
				_ = c.revoker.RevokeAlwaysRule(c.cfg.Name, c.cfg.Name+"/"+toolName)
			}
		}
	}

	// Audit mcp_refresh per A8
	if c.auditSink != nil {
		_ = c.auditSink.Append(AuditRecord{
			Kind: AuditKindMCPRefresh,
			Cred: c.cfg.Name,
		})
	}

	return nil
}

// ConnectorStatus represents the status row for MCP-STATUS per MCP-SPEC §9.
type ConnectorStatus struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Tools    int    `json:"tools"`
	Restarts int    `json:"restarts"`
	Reason   string `json:"reason"`
}

// Status returns current connector status for MCP-STATUS.
func (c *MCPConnector) Status() ConnectorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	numTools := 0
	if snap := c.snapshot.Load(); snap != nil {
		numTools = len(snap.Tools)
	}

	return ConnectorStatus{
		Name:     c.cfg.Name,
		State:    c.state,
		Tools:    numTools,
		Restarts: len(c.restarts),
		Reason:   c.reason,
	}
}

// ToolStatus represents the tool row for MCP-TOOLS per MCP-SPEC §9.
type ToolStatus struct {
	Name  string `json:"name"`
	Flags string `json:"flags"`
	Title string `json:"title"`
}

// ToolStatuses returns the tool list formatted for MCP-TOOLS.
func (c *MCPConnector) ToolStatuses() []ToolStatus {
	snap := c.snapshot.Load()
	if snap == nil {
		return nil
	}

	var res []ToolStatus
	for _, t := range snap.Tools {
		var flagParts []string
		if t.Annotations != nil {
			if t.Annotations.ReadOnlyHint {
				flagParts = append(flagParts, "read_only")
			}
			if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
				flagParts = append(flagParts, "destructive")
			}
			if t.Annotations.OpenWorldHint != nil && *t.Annotations.OpenWorldHint {
				flagParts = append(flagParts, "open_world")
			}
		}

		flags := "none"
		if len(flagParts) > 0 {
			flags = strings.Join(flagParts, ",")
		}

		res = append(res, ToolStatus{
			Name:  t.Canonical,
			Flags: flags,
			Title: t.Title,
		})
	}

	return res
}

// RailRegistry manages the configured MCP connectors.
type RailRegistry struct {
	mu               sync.RWMutex
	connectors       map[string]*MCPConnector
	names            []string
	firstUnlockCount atomic.Int64
}

// NewRailRegistry creates a registry from config entries.
func NewRailRegistry(cfgs []MCPServerConfig, hearthDir string, opts ...Option) *RailRegistry {
	reg := &RailRegistry{
		connectors: make(map[string]*MCPConnector),
	}
	for _, cfg := range cfgs {
		conn := NewMCPConnector(cfg, hearthDir, opts...)
		reg.connectors[cfg.Name] = conn
		reg.names = append(reg.names, cfg.Name)
	}
	return reg
}

// RegisterConnector adds or replaces a connector in the registry.
func (r *RailRegistry) RegisterConnector(c *MCPConnector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.connectors[c.Name()]; !ok {
		r.names = append(r.names, c.Name())
	}
	r.connectors[c.Name()] = c
}

// Connector returns the connector by name.
func (r *RailRegistry) Connector(name string) *MCPConnector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connectors[name]
}

// Connectors returns all connectors in registration order.
func (r *RailRegistry) Connectors() []*MCPConnector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var list []*MCPConnector
	for _, name := range r.names {
		list = append(list, r.connectors[name])
	}
	return list
}

// OnFirstUnlock broadcasts first-unlock signal to all connectors.
func (r *RailRegistry) OnFirstUnlock(ctx context.Context) {
	r.firstUnlockCount.Add(1)
	for _, conn := range r.Connectors() {
		_ = conn.OnFirstUnlock(ctx)
	}
}

// FirstUnlockCount returns the number of times OnFirstUnlock was called.
func (r *RailRegistry) FirstUnlockCount() int64 {
	return r.firstUnlockCount.Load()
}

// NotifyCredentialGone notifies all connectors of credential deletion.
func (r *RailRegistry) NotifyCredentialGone(name string) {
	for _, conn := range r.Connectors() {
		conn.NotifyCredentialGone(name)
	}
}

// ManualRefresh performs manual refresh for a connector by name per §7.
func (r *RailRegistry) ManualRefresh(ctx context.Context, name string) error {
	conn := r.Connector(name)
	if conn == nil {
		return fmt.Errorf("connector %q not found", name)
	}
	return conn.ManualRefresh(ctx)
}

// Refresh performs automatic refresh for a connector by name per §4.3.
func (r *RailRegistry) Refresh(ctx context.Context, name string) error {
	conn := r.Connector(name)
	if conn == nil {
		return fmt.Errorf("connector %q not found", name)
	}
	return conn.Refresh(ctx)
}
