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
	// DefaultSigninTimeoutS is the default sign-in window timeout in seconds.
	DefaultSigninTimeoutS = 600
	// DefaultNoVNCDir is the default directory containing noVNC static assets.
	DefaultNoVNCDir = "/usr/share/novnc"
)

// SigninConfig configures the guided browser sign-in window capability.
type SigninConfig struct {
	// TimeoutS is the maximum session duration in seconds before auto-closing (default 600).
	TimeoutS int `yaml:"timeout_s"`
	// NoVNCDir is the filesystem path to noVNC static assets (default "/usr/share/novnc").
	NoVNCDir string `yaml:"novnc_dir"`
	// ViewBaseURL is the external base URL for sign-in viewer links (e.g. "http://tailnet-host:7717").
	// When empty, a request-less fallback "http://<hostname>:<serve port>" is used, or a relative URL
	// if unresolvable.
	ViewBaseURL string `yaml:"view_base_url"`
}

// DefaultSigninConfig returns the default sign-in configuration.
func DefaultSigninConfig() SigninConfig {
	return SigninConfig{
		TimeoutS:    DefaultSigninTimeoutS,
		NoVNCDir:    DefaultNoVNCDir,
		ViewBaseURL: "",
	}
}

// Config configures the web fetch and browse capabilities.
type Config struct {
	Enabled        bool   `yaml:"enabled"`
	BrowseTimeoutS int    `yaml:"browse_timeout_s"`
	MaxTextBytes   int    `yaml:"max_text_bytes"`
	ChromePath     string `yaml:"chrome_path"`
	// Isolate wraps every Chromium launch in a bubblewrap sandbox (default
	// true). See sandbox.go and docs/WEB-LAYER-SPEC.md "Process isolation".
	Isolate bool         `yaml:"isolate"`
	Signin  SigninConfig `yaml:"signin"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		BrowseTimeoutS: DefaultBrowseTimeoutS,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     DetectChrome(),
		Isolate:        true,
		Signin:         DefaultSigninConfig(),
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
	type rawSigninConfig struct {
		TimeoutS    *int    `yaml:"timeout_s"`
		NoVNCDir    *string `yaml:"novnc_dir"`
		ViewBaseURL *string `yaml:"view_base_url"`
	}

	type rawConfig struct {
		Enabled        *bool            `yaml:"enabled"`
		BrowseTimeoutS *int             `yaml:"browse_timeout_s"`
		MaxTextBytes   *int             `yaml:"max_text_bytes"`
		ChromePath     *string          `yaml:"chrome_path"`
		Isolate        *bool            `yaml:"isolate"`
		Signin         *rawSigninConfig `yaml:"signin"`
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

	if raw.Isolate != nil {
		c.Isolate = *raw.Isolate
	} else {
		c.Isolate = true
	}

	c.Signin = DefaultSigninConfig()
	if raw.Signin != nil {
		if raw.Signin.TimeoutS != nil {
			c.Signin.TimeoutS = *raw.Signin.TimeoutS
		}
		if raw.Signin.NoVNCDir != nil {
			c.Signin.NoVNCDir = strings.TrimSpace(*raw.Signin.NoVNCDir)
		}
		if raw.Signin.ViewBaseURL != nil {
			c.Signin.ViewBaseURL = strings.TrimSpace(*raw.Signin.ViewBaseURL)
		}
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
	if c.Signin.TimeoutS < 0 {
		return fmt.Errorf("web.signin.timeout_s cannot be negative: %d", c.Signin.TimeoutS)
	}
	return nil
}
