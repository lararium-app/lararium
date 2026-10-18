package custosdoor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type mcpState struct {
	mu       sync.RWMutex
	statuses map[string]ConnectorStatus
	tools    map[string][]ToolInfo
	subs     map[int]func([]ConnectorStatus, []ToolInfo)
	nextSub  int
}

func (c *Client) initMCP() {
	c.mcp = &mcpState{
		statuses: make(map[string]ConnectorStatus),
		tools:    make(map[string][]ToolInfo),
		subs:     make(map[int]func([]ConnectorStatus, []ToolInfo)),
	}
}

func (c *Client) handleMCPLine(line string) {
	switch {
	case strings.HasPrefix(line, "MCP-STATUS "):
		var list []ConnectorStatus
		if err := json.Unmarshal([]byte(line[11:]), &list); err != nil {
			c.log.Printf("WARN custos door: dropped malformed MCP-STATUS frame: %v", err)
			return
		}

		c.mcp.mu.Lock()
		c.mcp.statuses = make(map[string]ConnectorStatus, len(list))
		for _, st := range list {
			c.mcp.statuses[st.Name] = st
		}
		// Drop cached tool rows for connectors absent from the new
		// status set, mirroring the wholesale status replacement (#96).
		for name := range c.mcp.tools {
			if _, ok := c.mcp.statuses[name]; !ok {
				delete(c.mcp.tools, name)
			}
		}
		snapStatuses := c.statusesLocked()
		snapTools := c.listToolsLocked()
		subs := c.mcpSubscribersLocked()
		c.mcp.mu.Unlock()

		go dispatchMCPSubs(subs, snapStatuses, snapTools)

	case strings.HasPrefix(line, "MCP-TOOLS "):
		rest := strings.TrimPrefix(line, "MCP-TOOLS ")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) < 2 {
			c.log.Printf("WARN custos door: dropped malformed MCP-TOOLS frame")
			return
		}
		connName := strings.TrimSpace(parts[0])
		var tools []ToolInfo
		if err := json.Unmarshal([]byte(parts[1]), &tools); err != nil {
			c.log.Printf("WARN custos door: dropped malformed MCP-TOOLS JSON: %v", err)
			return
		}
		for i := range tools {
			tools[i].Connector = connName
		}

		c.mcp.mu.Lock()
		c.mcp.tools[connName] = tools
		snapStatuses := c.statusesLocked()
		snapTools := c.listToolsLocked()
		subs := c.mcpSubscribersLocked()
		c.mcp.mu.Unlock()

		go dispatchMCPSubs(subs, snapStatuses, snapTools)
	}
}

func dispatchMCPSubs(subs []func([]ConnectorStatus, []ToolInfo), statuses []ConnectorStatus, tools []ToolInfo) {
	for _, fn := range subs {
		fn(statuses, tools)
	}
}

func (c *Client) mcpSubscribersLocked() []func([]ConnectorStatus, []ToolInfo) {
	out := make([]func([]ConnectorStatus, []ToolInfo), 0, len(c.mcp.subs))
	for _, fn := range c.mcp.subs {
		out = append(out, fn)
	}
	return out
}

func (c *Client) statusesLocked() []ConnectorStatus {
	out := make([]ConnectorStatus, 0, len(c.mcp.statuses))
	for _, st := range c.mcp.statuses {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (c *Client) listToolsLocked() []ToolInfo {
	var readyNames []string
	for name, st := range c.mcp.statuses {
		if st.State == "ready" {
			readyNames = append(readyNames, name)
		}
	}
	sort.Strings(readyNames)
	var out []ToolInfo
	for _, name := range readyNames {
		out = append(out, c.mcp.tools[name]...)
	}
	return out
}

// Status returns current connector statuses via synchronous doors.sock STATUS command.
func (c *Client) Status(ctx context.Context) ([]ConnectorStatus, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return nil, ErrDoorDown
	}

	select {
	case <-s.ack:
	default:
	}

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.conn.Write([]byte("MCP-STATUS\n")); err != nil {
		s.conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrDoorDown, err)
	}

	ackTimeout := c.opts.AckTimeout
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem < ackTimeout && rem > 0 {
			ackTimeout = rem
		}
	}
	t := time.NewTimer(ackTimeout)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.dead:
		return nil, ErrDoorDown
	case <-t.C:
		s.conn.Close()
		return nil, errors.New("custos: door ack timed out")
	case line := <-s.ack:
		if strings.HasPrefix(line, "OK ") {
			var list []ConnectorStatus
			if err := json.Unmarshal([]byte(line[3:]), &list); err != nil {
				return nil, fmt.Errorf("custos: bad STATUS reply: %w", err)
			}
			return list, nil
		}
		if strings.HasPrefix(line, "ERR ") {
			return nil, mapMCPErr(strings.TrimPrefix(line, "ERR "))
		}
		return nil, fmt.Errorf("custos: unexpected reply %q", truncate(line))
	}
}

// Tools returns tools for the specified connector via synchronous doors.sock TOOLS command.
func (c *Client) Tools(ctx context.Context, name string) ([]ToolInfo, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return nil, ErrDoorDown
	}

	select {
	case <-s.ack:
	default:
	}

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.conn.Write([]byte("MCP-TOOLS " + name + "\n")); err != nil {
		s.conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrDoorDown, err)
	}

	ackTimeout := c.opts.AckTimeout
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem < ackTimeout && rem > 0 {
			ackTimeout = rem
		}
	}
	t := time.NewTimer(ackTimeout)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.dead:
		return nil, ErrDoorDown
	case <-t.C:
		s.conn.Close()
		return nil, errors.New("custos: door ack timed out")
	case line := <-s.ack:
		if strings.HasPrefix(line, "OK ") {
			var list []ToolInfo
			if err := json.Unmarshal([]byte(line[3:]), &list); err != nil {
				return nil, fmt.Errorf("custos: bad TOOLS reply: %w", err)
			}
			for i := range list {
				list[i].Connector = name
			}
			return list, nil
		}
		if strings.HasPrefix(line, "ERR ") {
			return nil, mapMCPErr(strings.TrimPrefix(line, "ERR "))
		}
		return nil, fmt.Errorf("custos: unexpected reply %q", truncate(line))
	}
}

// Refresh triggers manual refresh for a connector via synchronous doors.sock REFRESH command.
func (c *Client) Refresh(ctx context.Context, name string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrDoorDown
	}

	select {
	case <-s.ack:
	default:
	}

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.conn.Write([]byte("MCP-REFRESH " + name + "\n")); err != nil {
		s.conn.Close()
		return fmt.Errorf("%w: %w", ErrDoorDown, err)
	}

	// Reply deadline: connector list_timeout + 5 s grace (A3)
	ackTimeout := 35 * time.Second
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem > 0 {
			ackTimeout = rem
		}
	}
	t := time.NewTimer(ackTimeout)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.dead:
		return ErrDoorDown
	case <-t.C:
		s.conn.Close()
		return errors.New("custos: door ack timed out")
	case line := <-s.ack:
		if line == "OK refreshed" || line == "OK" || strings.HasPrefix(line, "OK ") {
			return nil
		}
		if strings.HasPrefix(line, "ERR ") {
			return mapMCPErr(strings.TrimPrefix(line, "ERR "))
		}
		return fmt.Errorf("custos: unexpected reply %q", truncate(line))
	}
}

// Call executes one tool on a connector via doors.sock MCP-CALL per
// MCP-SPEC §9 (amendment A3): `MCP-CALL <connector> <tool> <args-json>`
// → `OK <result-json>` | `ERR <reason>`. argsJSON must be a raw JSON
// object matching the tool's InputSchema (empty → "{}"). The returned
// bytes are the raw execution result frame (MCP content[] shape, §6.3).
// Tool execution is slow-path (worker exec timeout + rail budget per
// §7), so unlike the metadata verbs the reply wait defaults to 90 s.
func (c *Client) Call(ctx context.Context, connector, tool string, argsJSON json.RawMessage) (json.RawMessage, error) {
	if connector == "" {
		return nil, fmt.Errorf("custos: empty connector name")
	}
	if tool == "" {
		return nil, fmt.Errorf("custos: empty tool name")
	}
	args := string(argsJSON)
	if args == "" {
		args = "{}"
	}

	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return nil, ErrDoorDown
	}

	select {
	case <-s.ack:
	default:
	}

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.conn.Write([]byte("MCP-CALL " + connector + " " + tool + " " + args + "\n")); err != nil {
		s.conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrDoorDown, err)
	}

	callTimeout := 90 * time.Second
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem > 0 && rem < callTimeout {
			callTimeout = rem
		}
	}
	t := time.NewTimer(callTimeout)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.dead:
		return nil, ErrDoorDown
	case <-t.C:
		s.conn.Close()
		return nil, errors.New("custos: MCP-CALL reply timed out")
	case line := <-s.ack:
		if strings.HasPrefix(line, "OK ") {
			return json.RawMessage(strings.TrimPrefix(line, "OK ")), nil
		}
		if strings.HasPrefix(line, "ERR ") {
			return nil, mapMCPErr(strings.TrimPrefix(line, "ERR "))
		}
		return nil, fmt.Errorf("custos: unexpected reply %q", truncate(line))
	}
}

// SubscribeMCP registers a callback for pushed rail status and tools frames.
// It converges state immediately by handing the current snapshot to the callback.
func (c *Client) SubscribeMCP(handler func(status []ConnectorStatus, tools []ToolInfo)) (cancel func()) {
	c.mcp.mu.Lock()
	id := c.mcp.nextSub
	c.mcp.nextSub++
	c.mcp.subs[id] = handler

	snapStatuses := c.statusesLocked()
	snapTools := c.listToolsLocked()
	c.mcp.mu.Unlock()

	go handler(snapStatuses, snapTools)

	return func() {
		c.mcp.mu.Lock()
		delete(c.mcp.subs, id)
		c.mcp.mu.Unlock()
	}
}

// Rail returns a scoped RailClient providing Subscribe and verb access.
func (c *Client) Rail() *RailClient {
	return &RailClient{client: c}
}

// RailClient wraps Client to provide rail-specific methods matching the Subscribe(handler) interface.
type RailClient struct {
	client *Client
}

// Status calls Status on the underlying client.
func (r *RailClient) Status(ctx context.Context) ([]ConnectorStatus, error) {
	return r.client.Status(ctx)
}

// Tools calls Tools on the underlying client.
func (r *RailClient) Tools(ctx context.Context, name string) ([]ToolInfo, error) {
	return r.client.Tools(ctx, name)
}

// Refresh calls Refresh on the underlying client.
func (r *RailClient) Refresh(ctx context.Context, name string) error {
	return r.client.Refresh(ctx, name)
}

// Call executes one connector tool (MCP-SPEC §9 amendment A3). argsJSON
// is a raw JSON object matching the tool's InputSchema; the result is
// the raw MCP result frame.
func (r *RailClient) Call(ctx context.Context, connector, tool string, argsJSON json.RawMessage) (json.RawMessage, error) {
	return r.client.Call(ctx, connector, tool, argsJSON)
}

// Subscribe registers a handler for pushed rail status and tool updates.
func (r *RailClient) Subscribe(handler func([]ConnectorStatus, []ToolInfo)) func() {
	return r.client.SubscribeMCP(handler)
}

// Statuses returns the last-seen status for all configured connectors, sorted by name.
func (c *Client) Statuses() []ConnectorStatus {
	c.mcp.mu.RLock()
	defer c.mcp.mu.RUnlock()
	return c.statusesLocked()
}

// ListTools returns tools from all currently ready connectors, sorted by connector name.
func (c *Client) ListTools() []ToolInfo {
	c.mcp.mu.RLock()
	defer c.mcp.mu.RUnlock()
	return c.listToolsLocked()
}

// AllTools returns all available tools from ready rail connectors.
func (c *Client) AllTools() []ToolInfo {
	return c.ListTools()
}

func mapMCPErr(reason string) error {
	switch reason {
	case "locked":
		return ErrLocked
	case "bad_token":
		return ErrBadToken
	default:
		return errors.New(reason)
	}
}
