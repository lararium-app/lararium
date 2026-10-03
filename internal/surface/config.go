package surface

import (
	"errors"
	"net"
	"time"
)

type ServeConfig struct {
	Listen          string        `yaml:"listen"`
	AllowedHosts    []string      `yaml:"allowed_hosts"`
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`
	TurnTimeout     time.Duration `yaml:"turn_timeout"`
}

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

	isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if !isLoopback && len(c.AllowedHosts) == 0 {
		return errors.New("non-loopback bind requires serve.allowed_hosts")
	}

	return nil
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
