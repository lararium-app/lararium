package surface

import (
	"errors"
	"net"
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

// isLoopbackHost is the loopback test used by both the start gate and
// the server's bind-time check (spec §2).
func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
