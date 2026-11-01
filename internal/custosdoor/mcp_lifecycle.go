package custosdoor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Connector lifecycle verbs (MCP-SPEC amendment A10, CONNECTOR-LIFE-SPEC D1):
//
//	MCP-ADD <name> <config-json>   -> OK | ERR <reason>
//	MCP-REMOVE <name>              -> OK | ERR <reason>
//	MCP-STATE <name> enabled|disabled -> OK | ERR <reason>
//	MCP-PROBE <config-json>        -> OK <n> <fingerprint>\nMCP-TOOLS _probe <json>
//	                                  | ERR <reason>
//
// Like Call, every verb here rides its OWN doors.sock connection (HELLO,
// one verb, terminal reply) rather than c.sess: a probe boots a child and
// can take a list_timeout, and the MCP-PROBE reply is connection-local —
// it never travels the broadcast subscription. Broadcast frames that
// interleave on the dedicated connection are fed to the normal mirror.

const (
	// probeToolsFrame is the connection-local MCP-TOOLS slot that follows an
	// MCP-PROBE success reply. It is outside the connector name grammar.
	probeToolsFrame = "_probe"

	probeTimeout      = 45 * time.Second // default list_timeout 30 s + grace
	addTimeout        = 45 * time.Second // child start + first tools/list
	lifecycleQuickTTL = 15 * time.Second // remove / state toggles
	probeFrameGrace   = 5 * time.Second  // MCP-TOOLS _probe after the OK line
)

// connectorNameRe is the connector name grammar (MCP-SPEC §3). Names are
// checked before they reach the wire so a verb can never be split into a
// second frame.
var connectorNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ErrVerbUnsupported means custosd did not recognise a lifecycle verb: the
// daemon predates amendment A10. Callers surface it as "daemon too old".
var ErrVerbUnsupported = errors.New("custos daemon does not support connector lifecycle verbs (daemon too old)")

// LifecycleError is a closed, secret-free refusal reason from custosd
// ("ERR <reason>" on a lifecycle verb).
type LifecycleError struct{ Reason string }

func (e *LifecycleError) Error() string { return e.Reason }

// ProbeResult is a successful MCP-PROBE outcome.
type ProbeResult struct {
	Fingerprint string
	Tools       []ToolInfo
}

// Probe boots a throwaway child from configJSON on custosd and returns its
// tool catalog and fingerprint. Nothing is registered or persisted.
func (c *Client) Probe(ctx context.Context, configJSON []byte) (*ProbeResult, error) {
	line, err := compactFrameJSON(configJSON)
	if err != nil {
		return nil, err
	}
	rest, tools, err := c.lifecycleRPC(ctx, "MCP-PROBE "+line, probeTimeout, true)
	if err != nil {
		return nil, err
	}
	res := &ProbeResult{Tools: tools}
	fields := strings.Fields(rest)
	if len(fields) >= 2 {
		res.Fingerprint = fields[1]
	}
	if n, perr := strconv.Atoi(firstField(fields)); perr == nil && n != len(tools) {
		return nil, fmt.Errorf("custos: probe tool count mismatch (%d vs %d)", n, len(tools))
	}
	return res, nil
}

// Add registers (or replaces) connector name with configJSON on custosd.
func (c *Client) Add(ctx context.Context, name string, configJSON []byte) error {
	if !connectorNameRe.MatchString(name) {
		return fmt.Errorf("custos: invalid connector name")
	}
	line, err := compactFrameJSON(configJSON)
	if err != nil {
		return err
	}
	_, _, err = c.lifecycleRPC(ctx, "MCP-ADD "+name+" "+line, addTimeout, false)
	return err
}

// Remove deletes connector name on custosd. custosd treats it idempotently.
func (c *Client) Remove(ctx context.Context, name string) error {
	if !connectorNameRe.MatchString(name) {
		return fmt.Errorf("custos: invalid connector name")
	}
	_, _, err := c.lifecycleRPC(ctx, "MCP-REMOVE "+name, lifecycleQuickTTL, false)
	return err
}

// SetState toggles the user enable/disable switch of connector name.
func (c *Client) SetState(ctx context.Context, name string, enabled bool) error {
	if !connectorNameRe.MatchString(name) {
		return fmt.Errorf("custos: invalid connector name")
	}
	word := "disabled"
	if enabled {
		word = "enabled"
	}
	_, _, err := c.lifecycleRPC(ctx, "MCP-STATE "+name+" "+word, addTimeout, false)
	return err
}

func firstField(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// compactFrameJSON validates and single-lines a JSON config for the wire.
func compactFrameJSON(raw []byte) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", errors.New("custos: config is not valid JSON")
	}
	s := buf.String()
	if strings.ContainsAny(s, "\r\n") {
		return "", errors.New("custos: config is not single-line JSON")
	}
	return s, nil
}

// mapLifecycleErr maps an "ERR <reason>" tail onto sentinels. A verb the
// daemon does not know (old custosd) is reported as ErrVerbUnsupported.
func mapLifecycleErr(reason string) error {
	reason = strings.TrimSpace(reason)
	switch reason {
	case "locked":
		return ErrLocked
	case "bad_token":
		return ErrBadToken
	case "unknown_command", "unknown_verb", "unsupported", "unknown":
		return ErrVerbUnsupported
	}
	return &LifecycleError{Reason: reason}
}

// lifecycleRPC runs one lifecycle verb on a dedicated doors.sock connection.
// It returns the text after "OK " and, when wantProbe is set, the tools of
// the connection-local "MCP-TOOLS _probe" frame.
func (c *Client) lifecycleRPC(ctx context.Context, frame string, ttl time.Duration, wantProbe bool) (string, []ToolInfo, error) {
	raw, err := os.ReadFile(c.opts.TokenPath)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrDoorDown, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n\x00") {
		return "", nil, fmt.Errorf("%w: empty or malformed door token", ErrDoorDown)
	}
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem > 0 && rem < ttl {
			ttl = rem
		}
	}

	var d net.Dialer
	dctx, cancelDial := context.WithTimeout(ctx, min(helloTimeout, ttl))
	conn, err := d.DialContext(dctx, "unix", c.opts.SockPath)
	cancelDial()
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("%w: %w", ErrDoorDown, err)
	}
	defer conn.Close()

	t := time.NewTimer(ttl)
	defer t.Stop()
	var timedOut atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-t.C:
			timedOut.Store(true)
			_ = conn.Close()
		case <-stop:
		}
	}()
	fail := func(err error) (string, []ToolInfo, error) {
		if timedOut.Load() {
			return "", nil, errors.New("custos: door ack timed out")
		}
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("%w: %w", ErrDoorDown, err)
	}

	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := conn.Write([]byte("HELLO " + tok + " " + c.name + "\n")); err != nil {
		return fail(err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	br := bufio.NewReaderSize(conn, 256<<10)
	_ = conn.SetReadDeadline(time.Now().Add(min(helloTimeout, ttl)))
	helloLine, err := readLine(br)
	if err != nil {
		return fail(err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch {
	case helloLine == "ERR bad_token":
		return "", nil, ErrBadToken
	case strings.HasPrefix(helloLine, "OK "):
	case strings.HasPrefix(helloLine, "ERR "):
		return "", nil, mapMCPErr(strings.TrimPrefix(helloLine, "ERR "))
	default:
		return "", nil, fmt.Errorf("custos: unexpected HELLO reply %q", truncate(helloLine))
	}

	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := conn.Write([]byte(frame + "\n")); err != nil {
		return fail(err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	var okRest string
	gotOK := false
	for {
		line, err := readLine(br)
		if err != nil {
			return fail(err)
		}
		switch {
		case strings.HasPrefix(line, "MCP-TOOLS "+probeToolsFrame+" "):
			if !wantProbe || !gotOK {
				continue
			}
			var tools []ToolInfo
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "MCP-TOOLS "+probeToolsFrame+" ")), &tools); err != nil {
				return "", nil, errors.New("custos: bad probe tools frame")
			}
			return okRest, tools, nil
		case strings.HasPrefix(line, "CARD "),
			strings.HasPrefix(line, "GONE "),
			strings.HasPrefix(line, "MCP-STATUS "),
			strings.HasPrefix(line, "MCP-TOOLS "):
			c.handleLine(nil, line)
		case strings.HasPrefix(line, "PARKED "):
		case line == "OK" || strings.HasPrefix(line, "OK "):
			okRest = strings.TrimSpace(strings.TrimPrefix(line, "OK"))
			gotOK = true
			if !wantProbe {
				return okRest, nil, nil
			}
			// The probe tools frame is written with the OK line; bound
			// the wait for it so a truncated reply cannot hang.
			_ = conn.SetReadDeadline(time.Now().Add(probeFrameGrace))
		case strings.HasPrefix(line, "ERR "):
			return "", nil, mapLifecycleErr(strings.TrimPrefix(line, "ERR "))
		default:
			return "", nil, fmt.Errorf("custos: unexpected reply %q", truncate(line))
		}
	}
}

// RailClient forwarding for the lifecycle verbs.

// Probe forwards to Client.Probe.
func (r *RailClient) Probe(ctx context.Context, configJSON []byte) (*ProbeResult, error) {
	return r.client.Probe(ctx, configJSON)
}

// Add forwards to Client.Add.
func (r *RailClient) Add(ctx context.Context, name string, configJSON []byte) error {
	return r.client.Add(ctx, name, configJSON)
}

// Remove forwards to Client.Remove.
func (r *RailClient) Remove(ctx context.Context, name string) error {
	return r.client.Remove(ctx, name)
}

// SetState forwards to Client.SetState.
func (r *RailClient) SetState(ctx context.Context, name string, enabled bool) error {
	return r.client.SetState(ctx, name, enabled)
}

// ToolsOf returns the mirrored tool rows of connector name (nil when none).
func (c *Client) ToolsOf(name string) []ToolInfo {
	c.mcp.mu.RLock()
	defer c.mcp.mu.RUnlock()
	src := c.mcp.tools[name]
	if len(src) == 0 {
		return nil
	}
	out := make([]ToolInfo, len(src))
	copy(out, src)
	return out
}
