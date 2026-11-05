package web

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultBrowseTimeoutS is the default browser navigation timeout in seconds.
	DefaultBrowseTimeoutS = 30
	// DefaultMaxTextBytes is the default maximum extracted text size in bytes.
	DefaultMaxTextBytes = 200000
)

// Config configures the web fetch and browse capabilities.
type Config struct {
	Enabled        bool   `yaml:"enabled"`
	BrowseTimeoutS int    `yaml:"browse_timeout_s"`
	MaxTextBytes   int    `yaml:"max_text_bytes"`
	ChromePath     string `yaml:"chrome_path"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		BrowseTimeoutS: DefaultBrowseTimeoutS,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     DetectChrome(),
	}
}

// DetectChrome detects a Chromium/Chrome binary from:
// 1. $LARARIUM_CHROME
// 2. chromium on PATH
// 3. chrome on PATH.
func DetectChrome() string {
	if p := strings.TrimSpace(os.Getenv("LARARIUM_CHROME")); p != "" {
		return p
	}
	if p, err := exec.LookPath("chromium"); err == nil {
		return p
	}
	if p, err := exec.LookPath("chrome"); err == nil {
		return p
	}
	return ""
}

// UnmarshalYAML implements yaml.Unmarshaler to validate and apply defaults
// without silent fallbacks for invalid values.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	type rawConfig struct {
		Enabled        *bool   `yaml:"enabled"`
		BrowseTimeoutS *int    `yaml:"browse_timeout_s"`
		MaxTextBytes   *int    `yaml:"max_text_bytes"`
		ChromePath     *string `yaml:"chrome_path"`
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("decode web config: %w", err)
	}
	_ = enc.Close()

	var raw rawConfig
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("decode web config: %w", err)
	}

	if raw.Enabled != nil {
		c.Enabled = *raw.Enabled
	} else {
		c.Enabled = true
	}

	if raw.BrowseTimeoutS != nil {
		c.BrowseTimeoutS = *raw.BrowseTimeoutS
	} else {
		c.BrowseTimeoutS = DefaultBrowseTimeoutS
	}

	if raw.MaxTextBytes != nil {
		c.MaxTextBytes = *raw.MaxTextBytes
	} else {
		c.MaxTextBytes = DefaultMaxTextBytes
	}

	if raw.ChromePath != nil {
		c.ChromePath = strings.TrimSpace(*raw.ChromePath)
	} else {
		c.ChromePath = DetectChrome()
	}

	return c.Validate()
}

// Validate checks the configuration values for validity.
func (c Config) Validate() error {
	if c.BrowseTimeoutS <= 0 {
		return fmt.Errorf("web.browse_timeout_s must be positive: %d", c.BrowseTimeoutS)
	}
	if c.MaxTextBytes <= 0 {
		return fmt.Errorf("web.max_text_bytes must be positive: %d", c.MaxTextBytes)
	}
	return nil
}
