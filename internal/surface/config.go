package surface

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ServeConfig is the serve: block of lararium.yaml (spec §2): where the
// surface listens and its two timeouts. All zero values normalize to
// spec defaults.
type ServeConfig struct {
	Listen          string        `yaml:"listen"`
	AllowedHosts    []string      `yaml:"allowed_hosts"`
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`
	TurnTimeout     time.Duration `yaml:"turn_timeout"`
}

// Normalize fills spec defaults (port 7717, approval 5m, turn 10m) and
// enforces the non-loopback rule: binding outside loopback without an
// allowed_hosts allowlist refuses to start (spec §2).
func (c *ServeConfig) Normalize() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7717"
	}
	if c.ApprovalTimeout == 0 {
		c.ApprovalTimeout = 5 * time.Minute
	}
	if c.TurnTimeout == 0 {
		c.TurnTimeout = 10 * time.Minute
	}

	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		host = c.Listen
	}

	if !isLoopbackHost(host) && len(c.AllowedHosts) == 0 {
		return errors.New("non-loopback bind requires serve.allowed_hosts")
	}

	return nil
}

// IsLoopbackLiteral reports whether raw represents a loopback IP literal (L-16 law).
// It parses the IP via netip, verifies IsLoopback (covering 127.0.0.0/8 and ::1),
// and strictly rejects zone IDs (such as %eth0), hostnames, and invalid
// notations (such as octal leading zeros). Bracketed IPv6 forms like [::1]
// are unwrapped and checked.
func IsLoopbackLiteral(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "%") {
		return false
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return false
	}
	if addr.Zone() != "" {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return addr.IsLoopback()
}

// isLoopbackHost is the loopback test used by both the start gate and
// the server's bind-time check (spec §2, L-16).
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	return IsLoopbackLiteral(h)
}
