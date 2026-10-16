// MCP HTTP transport implementation per MCP-SPEC §3, §4.3, §5, §6, §7, §8, §9.

package custos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type httpStatusRecorderKeyType struct{}

var httpStatusRecorderKey = httpStatusRecorderKeyType{}

type httpStatusRecorder struct {
	statusCode int
	err        error
}

// mcpHTTPRoundTripper injects the Authorization: Bearer header when bearer is configured,
// records response status codes, and intercepts 401/403 to trigger terminal auth_failed.
type mcpHTTPRoundTripper struct {
	base   http.RoundTripper
	bearer string
	conn   *MCPConnector
}

// RoundTrip implements http.RoundTripper.
func (t *mcpHTTPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	if t.bearer != "" {
		req2.Header.Set("Authorization", "Bearer "+t.bearer)
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req2)
	if rec, ok := req.Context().Value(httpStatusRecorderKey).(*httpStatusRecorder); ok {
		if resp != nil {
			rec.statusCode = resp.StatusCode
		}
		rec.err = err
	}

	if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		if _, ok := req.Context().Value(httpStatusRecorderKey).(*httpStatusRecorder); !ok {
			go t.conn.recordHTTPAuthFailure()
		}
	}

	return resp, err
}

// resolveBearer resolves ${vault:X} / ${env:NAME} or literal bearer lazily at connect time per §3, §6.
func resolveBearer(bearer string, resolver func(string) (string, error)) (string, error) {
	if bearer == "" {
		return "", nil
	}
	switch {
	case strings.HasPrefix(bearer, "${vault:") && strings.HasSuffix(bearer, "}"):
		if resolver == nil {
			return "", errors.New("resolver required for vault reference")
		}
		val, err := resolver(bearer)
		if err != nil || val == "" {
			return "", fmt.Errorf("resolve bearer %s: %w", bearer, err)
		}
		return val, nil

	case strings.HasPrefix(bearer, "${env:") && strings.HasSuffix(bearer, "}"):
		var val string
		if resolver != nil {
			if r, err := resolver(bearer); err == nil {
				val = r
			}
		}
		if val == "" {
			envVar := strings.TrimSuffix(strings.TrimPrefix(bearer, "${env:"), "}")
			val = os.Getenv(envVar)
		}
		if val == "" {
			return "", fmt.Errorf("environment variable %s missing", bearer)
		}
		return val, nil

	default:
		return bearer, nil
	}
}

func (c *MCPConnector) recordHTTPAuthFailureLocked() {
	c.state = MCPStateDisabled
	c.reason = MCPReasonAuthFailed
	c.consecutiveFailures = 0
	c.cancelRetryLocked()
	c.teardownChildLocked()
}

func (c *MCPConnector) recordHTTPAuthFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordHTTPAuthFailureLocked()
}

func (c *MCPConnector) recordHTTPTransportFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != MCPStateReady {
		return
	}
	c.consecutiveFailures++
	if c.consecutiveFailures >= 3 {
		c.consecutiveFailures = 0
		c.transitionBackoffLocked(MCPReasonTransport)
		c.teardownChildLocked()
	}
}

func (c *MCPConnector) recordHTTPTransportSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveFailures = 0
}

//nolint:contextcheck // background retry timers and debounce routines outlive startHTTPLocked context
func (c *MCPConnector) startHTTPLocked(ctx context.Context) error {
	// 1. Resolve Bearer token lazily at connect time per §3, §6
	resolvedBearer, err := resolveBearer(c.cfg.Bearer, c.envResolver)
	if err != nil {
		c.state = MCPStateDisabled
		c.reason = MCPReasonCredentialUnavailable
		c.cancelRetryLocked()
		return nil
	}

	var needles []string
	if len(resolvedBearer) >= scrubMinLength {
		needles = []string{resolvedBearer}
	}
	c.needles = needles

	// 2. Build HTTP transport with custom RoundTripper
	httpTransport := &mcpHTTPRoundTripper{
		base:   c.baseTransport,
		bearer: resolvedBearer,
		conn:   c,
	}
	httpClient := &http.Client{
		Transport: httpTransport,
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   c.cfg.URL,
		HTTPClient: httpClient,
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "custos",
		Version: "v1.2",
	}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{
			Disabled: true, // v1 rejects MRTR
		},
		ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			c.scheduleDebouncedRefresh()
		},
	})

	listTimeout := c.cfg.ListTimeout
	if listTimeout <= 0 {
		listTimeout = DefaultMCPListTimeout
	}
	listCtx, listCancel := context.WithTimeout(ctx, listTimeout)
	defer listCancel()

	rec := &httpStatusRecorder{}
	connectCtx := context.WithValue(listCtx, httpStatusRecorderKey, rec)

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		c.teardownChildLocked()
		if rec.statusCode == http.StatusUnauthorized || rec.statusCode == http.StatusForbidden {
			c.recordHTTPAuthFailureLocked()
			return nil
		}
		if errors.Is(listCtx.Err(), context.DeadlineExceeded) || isDeadlineErr(err) {
			c.transitionBackoffLocked(MCPReasonListTimeout)
		} else {
			c.transitionBackoffLocked(MCPReasonTransport)
		}
		return nil
	}
	c.session.Store(session)

	// First tools/list within remaining listTimeout budget
	listRes, err := session.ListTools(connectCtx, nil)
	if err != nil {
		c.teardownChildLocked()
		if rec.statusCode == http.StatusUnauthorized || rec.statusCode == http.StatusForbidden {
			c.recordHTTPAuthFailureLocked()
			return nil
		}
		if errors.Is(listCtx.Err(), context.DeadlineExceeded) || isDeadlineErr(err) {
			c.transitionBackoffLocked(MCPReasonListTimeout)
		} else {
			c.transitionBackoffLocked(MCPReasonTransport)
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
	c.consecutiveFailures = 0
	c.cancelRetryLocked()

	return nil
}
