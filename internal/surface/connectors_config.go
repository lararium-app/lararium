package surface

import (
	"bytes"
	"encoding/json"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Connector config grammar (CONNECTOR-LIFE-SPEC D3/D8, MCP-SPEC §3).
//
// internal/surface cannot import internal/custos (custos imports surface),
// so the grammar constants live here and cmd/hearthd carries a parity test
// (connectors_parity_test.go) that pins them — and the rejection reasons —
// to custos.ParseMCPConfigJSON so the two copies cannot drift. custosd
// re-validates every config at MCP-PROBE/MCP-ADD; this copy exists so bad
// input is refused with a 400 before any child is booted.
const (
	// ConnectorNamePattern is the connector name grammar.
	ConnectorNamePattern = `^[a-z][a-z0-9_-]{0,31}$`
	// ConnectorNameMaxLen is the longest accepted connector name.
	ConnectorNameMaxLen = 32
	// ConnectorDefaultMaxConcurrent is the execution concurrency default.
	ConnectorDefaultMaxConcurrent = 4
	// ConnectorMinMaxConcurrent / ConnectorMaxMaxConcurrent bound it.
	ConnectorMinMaxConcurrent = 1
	ConnectorMaxMaxConcurrent = 16
	// ConnectorDefaultListTimeout is the child boot + first tools/list budget.
	ConnectorDefaultListTimeout = "30s"

	// Rejection reasons (the frozen §3 vocabulary, shared with custosd).
	connReasonUnknownKey       = "unknown_key"
	connReasonReservedName     = "reserved_name"
	connReasonBadTransport     = "bad_transport"
	connReasonMissingKey       = "missing_key"
	connReasonBadCommand       = "bad_command"
	connReasonBadURL           = "bad_url"
	connReasonBadMaxConcurrent = "bad_max_concurrent"
	connReasonBadName          = "bad_name"
	connReasonBadRequest       = "bad_request"
)

// ConnectorTransports is the transport enum.
var ConnectorTransports = []string{"stdio", "http"}

// connectorReservedNames are refused as connector names. "events" is
// reserved by the surface itself: /v1/mcp/connectors/events is the SSE rail.
var connectorReservedNames = []string{"gmail", "custos", "events"}

var (
	connectorNameRe = regexp.MustCompile(ConnectorNamePattern)

	connectorKnownKeys = map[string]bool{
		"name": true, "transport": true, "command": true, "args": true,
		"env": true, "url": true, "bearer": true, "tool_allowlist": true,
		"tool_denylist": true, "max_concurrent_calls": true, "list_timeout": true,
	}
)

// connectorConfig is the validated POST/PATCH body. It carries literal
// secrets (env values, bearer, URL userinfo) and therefore must never be
// logged, echoed or marshalled into a response: view() is the only
// projection allowed to leave a handler.
type connectorConfig struct {
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

func isReservedConnectorName(n string) bool {
	for _, r := range connectorReservedNames {
		if n == r {
			return true
		}
	}
	return false
}

// validateConnectorConfig strictly validates one connector config object
// and returns it with a closed reason on refusal (reason is "" on success).
// The check order mirrors custos.ParseMCPConfigJSON. The only deliberate
// difference: the stdio command's on-disk stat (exists, regular, +x) is the
// daemon's job at probe time — this process may not share its filesystem
// view — so only the absolute-path rule is enforced here.
func validateConnectorConfig(data []byte) (connectorConfig, string) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return connectorConfig{}, connReasonBadRequest
	}
	for k := range top {
		if !connectorKnownKeys[k] {
			return connectorConfig{}, connReasonUnknownKey
		}
	}
	var c connectorConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return connectorConfig{}, connReasonUnknownKey
	}
	if c.ListTimeout != nil {
		// Same mapping as custos.ParseMCPConfigJSON (a bad duration is
		// reported as a decode failure, unknown_key).
		if _, err := time.ParseDuration(*c.ListTimeout); err != nil {
			return connectorConfig{}, connReasonUnknownKey
		}
	}

	if !connectorNameRe.MatchString(c.Name) {
		return connectorConfig{}, connReasonBadName
	}
	if isReservedConnectorName(c.Name) {
		return connectorConfig{}, connReasonReservedName
	}
	switch c.Transport {
	case "stdio":
		if c.Command == "" {
			return connectorConfig{}, connReasonMissingKey
		}
		if !filepath.IsAbs(c.Command) {
			return connectorConfig{}, connReasonBadCommand
		}
	case "http":
		if c.URL == "" {
			return connectorConfig{}, connReasonMissingKey
		}
		if !validConnectorHTTPURL(c.URL) {
			return connectorConfig{}, connReasonBadURL
		}
	default:
		return connectorConfig{}, connReasonBadTransport
	}
	if c.MaxConcurrentCalls != nil &&
		(*c.MaxConcurrentCalls < ConnectorMinMaxConcurrent || *c.MaxConcurrentCalls > ConnectorMaxMaxConcurrent) {
		return connectorConfig{}, connReasonBadMaxConcurrent
	}
	return c, ""
}

func validConnectorHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if scheme == "https" {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	}
	return false
}

// envNames returns the sorted env var NAMES of the config; values never leave.
func (c connectorConfig) envNames() []string {
	if len(c.Env) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(c.Env))
	for k := range c.Env {
		out = append(out, logSanitize(k))
	}
	sort.Strings(out)
	return out
}

// secretFlagRe matches argument names that conventionally carry a secret.
var secretFlagRe = regexp.MustCompile(`(?i)(token|secret|key|passw(or)?d|pwd|auth|bearer|credential|cookie)`)

// redactArgs masks the values of secret-looking flags ("--token x",
// "--api-key=x") and strips URL userinfo from URL-shaped arguments.
// Positional secrets cannot be recognised; configs should reference
// ${vault:NAME} instead — and the card says args are shown as supplied.
func redactArgs(args []string) []string {
	out := make([]string, 0, len(args))
	maskNext := false
	for _, a := range args {
		a = logSanitize(a)
		switch {
		case maskNext:
			maskNext = false
			if strings.HasPrefix(a, "-") {
				out = append(out, a)
			} else {
				out = append(out, "[redacted]")
			}
			continue
		case strings.HasPrefix(a, "-"):
			if i := strings.IndexByte(a, '='); i > 0 {
				if secretFlagRe.MatchString(a[:i]) && !isRefExprText(a[i+1:]) {
					out = append(out, a[:i+1]+"[redacted]")
					continue
				}
			} else if secretFlagRe.MatchString(a) {
				maskNext = true
			}
		case strings.Contains(a, "://"):
			a = summarizeURL(a)
		}
		out = append(out, a)
	}
	return out
}

func isRefExprText(v string) bool {
	return (strings.HasPrefix(v, "${vault:") || strings.HasPrefix(v, "${env:")) && strings.HasSuffix(v, "}")
}

// summarizeURL renders scheme://host/path with userinfo, query and fragment
// removed (a "?…" marker notes a dropped query).
func summarizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(unparseable url)"
	}
	hadQuery := u.RawQuery != "" || u.ForceQuery
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	s := u.String()
	if hadQuery {
		s += "?…"
	}
	return logSanitize(s)
}

// targetSummary is the url_or_command text (secret-free by construction).
func (c connectorConfig) targetSummary() string {
	if c.Transport == "http" {
		return summarizeURL(c.URL)
	}
	parts := append([]string{logSanitize(c.Command)}, redactArgs(c.Args)...)
	return strings.Join(parts, " ")
}
