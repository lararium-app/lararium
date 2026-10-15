// Package custos implements the credential custody vault core per CUSTOS-SPEC.
package custos

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// Rejection reason constants per MCP-SPEC §3, §12 V2.
const (
	ReasonUnknownKey       = "unknown_key"
	ReasonDuplicateName    = "duplicate_name"
	ReasonReservedName     = "reserved_name"
	ReasonBadTransport     = "bad_transport"
	ReasonMissingKey       = "missing_key"
	ReasonBadCommand       = "bad_command"
	ReasonBadURL           = "bad_url"
	ReasonBadMaxConcurrent = "bad_max_concurrent"
	ReasonBadName          = "bad_name"

	// DefaultMCPMaxConcurrentCalls is the execution concurrency ceiling per §3.
	DefaultMCPMaxConcurrentCalls = 4

	// DefaultMCPListTimeout is the budget for child boot and first tool list.
	DefaultMCPListTimeout = 30 * time.Second
)

// ConnectorsConfig defines external connector blocks per MCP-SPEC §3 (A4).
type ConnectorsConfig struct {
	MCP []MCPServerConfig `yaml:"mcp"`

	unmarshalErr error
}

// MCPServerConfig configures one external MCP connector per MCP-SPEC §3.
//
// Defaults per MCP-SPEC §3:
//   - max_concurrent_calls: 4 (valid range: 1..16)
//   - list_timeout: 30s (budget for spawn and first tools/list)
//   - tool_allowlist: empty (all listed tools allowed unless denied)
//   - tool_denylist: empty
//
// Environment variables and vault references (${vault:ID}, ${env:NAME})
// are stored unresolved at configuration load time; resolution is lazy
// at child process spawn (stdio) or request dispatch (http).
type MCPServerConfig struct {
	Name               string            `yaml:"name"`
	Transport          string            `yaml:"transport"`
	Command            string            `yaml:"command"`
	Args               []string          `yaml:"args"`
	Env                map[string]string `yaml:"env"`
	URL                string            `yaml:"url"`
	Bearer             string            `yaml:"bearer"`
	ToolAllowlist      []string          `yaml:"tool_allowlist"`
	ToolDenylist       []string          `yaml:"tool_denylist"`
	MaxConcurrentCalls int               `yaml:"max_concurrent_calls"`
	ListTimeout        time.Duration     `yaml:"list_timeout"`
}

func (m *MCPServerConfig) normalize() {
	if m.MaxConcurrentCalls <= 0 {
		m.MaxConcurrentCalls = DefaultMCPMaxConcurrentCalls
	}
	if m.ListTimeout <= 0 {
		m.ListTimeout = DefaultMCPListTimeout
	}
}

// ConfigError records a configuration rejection reason per MCP-SPEC §3.
type ConfigError struct {
	Reason  string
	Message string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s: %s", e.Reason, e.Message)
}

// ErrorReason returns the named rejection reason from err if it is
// a ConfigError.
func ErrorReason(err error) string {
	var ce *ConfigError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

var (
	nameGrammarRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

	knownCustosKeys = map[string]bool{
		"unlock_keyfile":       true,
		"lock_wait_timeout":    true,
		"audit_retention_days": true,
		"ask_hold_timeout":     true,
		"worker_exec_timeout":  true,
		"max_parked_per_cell":  true,
		"max_parked_global":    true,
		"header_extras":        true,
		"connectors":           true,
	}

	knownMCPKeys = map[string]bool{
		"name":                 true,
		"transport":            true,
		"command":              true,
		"args":                 true,
		"env":                  true,
		"url":                  true,
		"bearer":               true,
		"tool_allowlist":       true,
		"tool_denylist":        true,
		"max_concurrent_calls": true,
		"list_timeout":         true,
	}

	configStderr io.Writer = os.Stderr
	configExit             = os.Exit
)

type rawMCPServerCfg struct {
	Name               string            `yaml:"name"`
	Transport          string            `yaml:"transport"`
	Command            string            `yaml:"command"`
	Args               []string          `yaml:"args"`
	Env                map[string]string `yaml:"env"`
	URL                string            `yaml:"url"`
	Bearer             string            `yaml:"bearer"`
	ToolAllowlist      []string          `yaml:"tool_allowlist"`
	ToolDenylist       []string          `yaml:"tool_denylist"`
	MaxConcurrentCalls *int              `yaml:"max_concurrent_calls"`
	ListTimeout        *time.Duration    `yaml:"list_timeout"`
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func isRFC1918(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4[0] == 10 {
		return true
	}
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	return false
}

func validateExecutableCommand(cmdPath string) error {
	if !filepath.IsAbs(cmdPath) {
		return &ConfigError{
			Reason:  ReasonBadCommand,
			Message: fmt.Sprintf("command %q is not an absolute path", cmdPath),
		}
	}
	fi, err := os.Stat(cmdPath)
	if err != nil {
		return &ConfigError{
			Reason:  ReasonBadCommand,
			Message: fmt.Sprintf("command %q stat: %v", cmdPath, err),
		}
	}
	if fi.IsDir() || !fi.Mode().IsRegular() {
		return &ConfigError{
			Reason:  ReasonBadCommand,
			Message: fmt.Sprintf("command %q is not a regular file", cmdPath),
		}
	}
	if fi.Mode()&0o111 == 0 {
		return &ConfigError{
			Reason:  ReasonBadCommand,
			Message: fmt.Sprintf("command %q has no execute permissions", cmdPath),
		}
	}
	if err := unix.Access(cmdPath, unix.X_OK); err != nil {
		return &ConfigError{
			Reason:  ReasonBadCommand,
			Message: fmt.Sprintf("command %q not executable: %v", cmdPath, err),
		}
	}
	return nil
}

func validateHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return &ConfigError{
			Reason:  ReasonBadURL,
			Message: fmt.Sprintf("url %q is invalid", rawURL),
		}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return &ConfigError{
			Reason:  ReasonBadURL,
			Message: fmt.Sprintf("url scheme %q must be http or https", u.Scheme),
		}
	}
	if scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return &ConfigError{
			Reason:  ReasonBadURL,
			Message: fmt.Sprintf("url %q missing hostname", rawURL),
		}
	}
	if isLoopbackHost(host) {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && isRFC1918(ip) {
		return nil
	}
	return &ConfigError{
		Reason:  ReasonBadURL,
		Message: fmt.Sprintf("http url %q host is neither loopback nor RFC1918", rawURL),
	}
}

func validateRawMCPEntries(rawEntries []rawMCPServerCfg) ([]MCPServerConfig, error) {
	seenNames := make(map[string]bool)
	var out []MCPServerConfig

	for _, raw := range rawEntries {
		if !nameGrammarRe.MatchString(raw.Name) {
			return nil, &ConfigError{
				Reason:  ReasonBadName,
				Message: fmt.Sprintf("name %q does not match grammar [a-z][a-z0-9_-]{0,31}", raw.Name),
			}
		}
		if raw.Name == "gmail" || raw.Name == "custos" {
			return nil, &ConfigError{
				Reason:  ReasonReservedName,
				Message: fmt.Sprintf("name %q is reserved", raw.Name),
			}
		}
		if seenNames[raw.Name] {
			return nil, &ConfigError{
				Reason:  ReasonDuplicateName,
				Message: fmt.Sprintf("duplicate connector name %q", raw.Name),
			}
		}
		seenNames[raw.Name] = true

		switch raw.Transport {
		case "stdio", "http":
			// valid transport
		default:
			return nil, &ConfigError{
				Reason:  ReasonBadTransport,
				Message: fmt.Sprintf("transport %q must be stdio or http", raw.Transport),
			}
		}

		if raw.Transport == "stdio" {
			if raw.Command == "" {
				return nil, &ConfigError{
					Reason:  ReasonMissingKey,
					Message: "missing required key 'command' for stdio transport",
				}
			}
			if err := validateExecutableCommand(raw.Command); err != nil {
				return nil, err
			}
		} else if raw.Transport == "http" {
			if raw.URL == "" {
				return nil, &ConfigError{
					Reason:  ReasonMissingKey,
					Message: "missing required key 'url' for http transport",
				}
			}
			if err := validateHTTPURL(raw.URL); err != nil {
				return nil, err
			}
		}

		maxConcurrent := DefaultMCPMaxConcurrentCalls
		if raw.MaxConcurrentCalls != nil {
			if *raw.MaxConcurrentCalls < 1 || *raw.MaxConcurrentCalls > 16 {
				return nil, &ConfigError{
					Reason:  ReasonBadMaxConcurrent,
					Message: fmt.Sprintf("max_concurrent_calls %d outside 1..16", *raw.MaxConcurrentCalls),
				}
			}
			maxConcurrent = *raw.MaxConcurrentCalls
		}

		listTimeout := DefaultMCPListTimeout
		if raw.ListTimeout != nil && *raw.ListTimeout > 0 {
			listTimeout = *raw.ListTimeout
		}

		out = append(out, MCPServerConfig{
			Name:               raw.Name,
			Transport:          raw.Transport,
			Command:            raw.Command,
			Args:               raw.Args,
			Env:                raw.Env,
			URL:                raw.URL,
			Bearer:             raw.Bearer,
			ToolAllowlist:      raw.ToolAllowlist,
			ToolDenylist:       raw.ToolDenylist,
			MaxConcurrentCalls: maxConcurrent,
			ListTimeout:        listTimeout,
		})
	}
	return out, nil
}

// UnmarshalYAML implements strict parsing for Config per MCP-SPEC §3.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	if err := c.unmarshalNode(value); err != nil {
		c.unmarshalErr = err
		return err
	}
	return nil
}

func (c *Config) unmarshalNode(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return &ConfigError{
			Reason:  ReasonUnknownKey,
			Message: "custos block must be a mapping",
		}
	}
	for i := 0; i < len(value.Content); i += 2 {
		k := value.Content[i].Value
		if !knownCustosKeys[k] {
			return &ConfigError{
				Reason:  ReasonUnknownKey,
				Message: fmt.Sprintf("unknown key %q in custos config", k),
			}
		}
	}
	type plain Config
	if err := value.Decode((*plain)(c)); err != nil {
		return err
	}
	return nil
}

// UnmarshalYAML implements strict parsing for ConnectorsConfig per MCP-SPEC §3.
func (c *ConnectorsConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		err := &ConfigError{
			Reason:  ReasonUnknownKey,
			Message: "connectors must be a mapping",
		}
		c.unmarshalErr = err
		return err
	}
	for i := 0; i < len(value.Content); i += 2 {
		k := value.Content[i].Value
		if k != "mcp" {
			err := &ConfigError{
				Reason:  ReasonUnknownKey,
				Message: fmt.Sprintf("unknown key %q in connectors", k),
			}
			c.unmarshalErr = err
			return err
		}
	}

	for i := 0; i < len(value.Content); i += 2 {
		if value.Content[i].Value == "mcp" {
			mcpNode := value.Content[i+1]
			if mcpNode.Kind != yaml.SequenceNode {
				err := &ConfigError{
					Reason:  ReasonUnknownKey,
					Message: "mcp must be a sequence",
				}
				c.unmarshalErr = err
				return err
			}
			var rawEntries []rawMCPServerCfg
			for _, entryNode := range mcpNode.Content {
				if entryNode.Kind != yaml.MappingNode {
					err := &ConfigError{
						Reason:  ReasonUnknownKey,
						Message: "mcp entry must be a mapping",
					}
					c.unmarshalErr = err
					return err
				}
				for j := 0; j < len(entryNode.Content); j += 2 {
					ek := entryNode.Content[j].Value
					if !knownMCPKeys[ek] {
						err := &ConfigError{
							Reason:  ReasonUnknownKey,
							Message: fmt.Sprintf("unknown key %q in mcp entry", ek),
						}
						c.unmarshalErr = err
						return err
					}
				}
				var raw rawMCPServerCfg
				if err := entryNode.Decode(&raw); err != nil {
					err := &ConfigError{
						Reason:  ReasonUnknownKey,
						Message: fmt.Sprintf("decode mcp entry: %v", err),
					}
					c.unmarshalErr = err
					return err
				}
				rawEntries = append(rawEntries, raw)
			}
			entries, err := validateRawMCPEntries(rawEntries)
			if err != nil {
				c.unmarshalErr = err
				return err
			}
			c.MCP = entries
		}
	}
	return nil
}

func reportConfigErrorAndExit(err error) {
	reason := ErrorReason(err)
	if reason == "" {
		reason = ReasonUnknownKey
	}
	fmt.Fprintf(configStderr, "config error: %s: %v\n", reason, err)
	configExit(1)
}

// LoadConfigFile loads and strictly validates config from path.
// On rejection, it prints the named reason to stderr and exits non-zero
// per MCP-SPEC §3.
func LoadConfigFile(path string) *Config {
	cfg, err := LoadConfig(path)
	if err != nil {
		reportConfigErrorAndExit(err)
	}
	return cfg
}

// LoadConfigOrExit is an alias for LoadConfigFile.
func LoadConfigOrExit(path string) *Config {
	return LoadConfigFile(path)
}

// ParseConfigOrExit strictly parses and validates config data.
// On rejection, it prints the named reason to stderr and exits non-zero
// per MCP-SPEC §3.
func ParseConfigOrExit(data []byte) *Config {
	cfg, err := ParseConfig(data)
	if err != nil {
		reportConfigErrorAndExit(err)
	}
	return cfg
}

// LoadConfig reads and parses lararium.yaml from path with strict checking.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses custos YAML configuration with strict unknown-key
// checking and validates external MCP server entries per MCP-SPEC §3.
func ParseConfig(data []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, &ConfigError{
			Reason:  ReasonUnknownKey,
			Message: fmt.Sprintf("yaml parse: %v", err),
		}
	}

	cfg := &Config{}
	if len(root.Content) == 0 {
		cfg.Normalize()
		return cfg, nil
	}

	docNode := root.Content[0]
	if docNode.Kind != yaml.MappingNode {
		return nil, &ConfigError{
			Reason:  ReasonUnknownKey,
			Message: "top-level config must be a YAML mapping",
		}
	}

	var targetNode *yaml.Node
	for i := 0; i < len(docNode.Content); i += 2 {
		if docNode.Content[i].Value == "custos" {
			targetNode = docNode.Content[i+1]
			break
		}
	}

	if targetNode == nil {
		hasConnectors := false
		var mcpValueNode *yaml.Node
		for i := 0; i < len(docNode.Content); i += 2 {
			switch docNode.Content[i].Value {
			case "connectors":
				hasConnectors = true
			case "mcp":
				mcpValueNode = docNode.Content[i+1]
			}
		}
		if hasConnectors {
			targetNode = docNode
		} else if mcpValueNode != nil {
			targetNode = &yaml.Node{
				Kind: yaml.MappingNode,
				Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "connectors"},
					{
						Kind: yaml.MappingNode,
						Content: []*yaml.Node{
							{Kind: yaml.ScalarNode, Value: "mcp"},
							mcpValueNode,
						},
					},
				},
			}
		} else {
			targetNode = docNode
		}
	}

	if err := cfg.unmarshalNode(targetNode); err != nil {
		return nil, err
	}
	cfg.Normalize()
	return cfg, nil
}

// ParseMCPConfig parses an mcp array or connectors mapping from YAML bytes.
func ParseMCPConfig(data []byte) ([]MCPServerConfig, error) {
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, err
	}
	return cfg.Connectors.MCP, nil
}
