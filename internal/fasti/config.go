package fasti

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	hhmmRegex      = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	quietSpanRegex = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]-([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// HeartbeatConfig configures the PENATUS heartbeat wake schedule (spec D6/D11).
type HeartbeatConfig struct {
	DailyAt string `yaml:"daily_at"`
}

// Config represents the fasti: block of lararium.yaml (spec D11).
// Unknown keys make decode fail (R9). Absent block = D11 defaults.
type Config struct {
	Enabled           bool            `yaml:"enabled"`
	LocalTimezone     string          `yaml:"local_timezone"`
	QuietHours        string          `yaml:"quiet_hours"`
	Heartbeat         HeartbeatConfig `yaml:"heartbeat"`
	MaxAnnouncePerDay int             `yaml:"max_announce_per_day"`
	CatchupWindow     time.Duration   `yaml:"catchup_window"`
}

// DefaultConfig returns the default configuration specified in D11.
func DefaultConfig() Config {
	return Config{
		Enabled:       false,
		LocalTimezone: "",
		QuietHours:    "",
		Heartbeat: HeartbeatConfig{
			DailyAt: "07:30",
		},
		MaxAnnouncePerDay: 8,
		CatchupWindow:     24 * time.Hour,
	}
}

type rawConfig struct {
	Enabled           bool            `yaml:"enabled"`
	LocalTimezone     string          `yaml:"local_timezone"`
	QuietHours        string          `yaml:"quiet_hours"`
	Heartbeat         HeartbeatConfig `yaml:"heartbeat"`
	MaxAnnouncePerDay *int            `yaml:"max_announce_per_day"`
	CatchupWindow     *time.Duration  `yaml:"catchup_window"`
}

func (r *rawConfig) toConfig() Config {
	cfg := DefaultConfig()
	cfg.Enabled = r.Enabled
	cfg.LocalTimezone = r.LocalTimezone
	cfg.QuietHours = r.QuietHours
	if r.Heartbeat.DailyAt != "" {
		cfg.Heartbeat.DailyAt = r.Heartbeat.DailyAt
	}
	// Spec M7: explicit max_announce_per_day: 0 means 0 (do not overwrite with default 8)
	if r.MaxAnnouncePerDay != nil {
		cfg.MaxAnnouncePerDay = *r.MaxAnnouncePerDay
	}
	// Spec M7: explicit catchup_window is preserved so 0/negative fails Validate()
	if r.CatchupWindow != nil {
		cfg.CatchupWindow = *r.CatchupWindow
	}
	return cfg
}

// ParseConfig decodes a fasti configuration block strictly (KnownFields(true)).
// An empty or whitespace-only input returns DefaultConfig().
// Both a raw block and a wrapped `fasti:` map are accepted.
func ParseConfig(data []byte) (Config, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return DefaultConfig(), nil
	}

	// Check if the input starts a top-level fasti: map
	var root yaml.Node
	if err := yaml.Unmarshal(trimmed, &root); err != nil {
		return Config{}, fmt.Errorf("decode fasti config: %w", err)
	}

	isWrapped := false
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		doc := root.Content[0]
		if doc.Kind == yaml.MappingNode {
			for i := 0; i < len(doc.Content); i += 2 {
				if doc.Content[i].Value == "fasti" {
					isWrapped = true
					break
				}
			}
		}
	}

	if isWrapped {
		type wrapped struct {
			Fasti *rawConfig `yaml:"fasti"`
		}
		var w wrapped
		dec := yaml.NewDecoder(bytes.NewReader(trimmed))
		dec.KnownFields(true)
		// Spec M8: if the input starts a top-level fasti: map, wrapped-decode errors must be returned verbatim.
		if err := dec.Decode(&w); err != nil {
			return Config{}, fmt.Errorf("decode fasti config: %w", err)
		}
		if w.Fasti == nil {
			return DefaultConfig(), nil
		}
		cfg := w.Fasti.toConfig()
		if err := cfg.Validate(); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	// Decode direct fasti block fields
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(trimmed))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode fasti config: %w", err)
	}

	cfg := raw.toConfig()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Location returns the configured local *time.Location, or time.Local if unset.
func (c Config) Location() (*time.Location, error) {
	if strings.TrimSpace(c.LocalTimezone) == "" {
		return time.Local, nil //nolint:gosmopolitan // spec D11: "" = system zone
	}
	loc, err := time.LoadLocation(c.LocalTimezone)
	if err != nil {
		return nil, fmt.Errorf("invalid local_timezone %q: %w", c.LocalTimezone, err)
	}
	return loc, nil
}

// Validate checks the configuration values against spec constraints.
func (c Config) Validate() error {
	if c.LocalTimezone != "" {
		if _, err := time.LoadLocation(c.LocalTimezone); err != nil {
			return fmt.Errorf("invalid local_timezone %q: %w", c.LocalTimezone, err)
		}
	}

	if c.QuietHours != "" && !quietSpanRegex.MatchString(c.QuietHours) {
		return fmt.Errorf("invalid quiet_hours %q: must match HH:MM-HH:MM", c.QuietHours)
	}

	if c.Heartbeat.DailyAt != "" && !hhmmRegex.MatchString(c.Heartbeat.DailyAt) {
		return fmt.Errorf("invalid heartbeat.daily_at %q: must match HH:MM", c.Heartbeat.DailyAt)
	}

	if c.MaxAnnouncePerDay < 0 {
		return fmt.Errorf("max_announce_per_day must be non-negative: %d", c.MaxAnnouncePerDay)
	}

	if c.CatchupWindow <= 0 {
		return fmt.Errorf("catchup_window must be positive: %v", c.CatchupWindow)
	}

	return nil
}
