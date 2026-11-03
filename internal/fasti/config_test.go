package fasti

import (
	"strings"
	"testing"
	"time"
)

func TestConfig_Defaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Enabled != false {
		t.Errorf("default Enabled = %v, want false", cfg.Enabled)
	}
	if cfg.LocalTimezone != "" {
		t.Errorf("default LocalTimezone = %q, want empty", cfg.LocalTimezone)
	}
	if cfg.QuietHours != "" {
		t.Errorf("default QuietHours = %q, want empty", cfg.QuietHours)
	}
	if cfg.Heartbeat.DailyAt != "07:30" {
		t.Errorf("default Heartbeat.DailyAt = %q, want 07:30", cfg.Heartbeat.DailyAt)
	}
	if cfg.MaxAnnouncePerDay != 8 {
		t.Errorf("default MaxAnnouncePerDay = %d, want 8", cfg.MaxAnnouncePerDay)
	}
	if cfg.CatchupWindow != 24*time.Hour {
		t.Errorf("default CatchupWindow = %v, want 24h", cfg.CatchupWindow)
	}
}

func TestConfig_ParseStrict(t *testing.T) {
	// 1. Absent / empty block -> defaults
	cfg, err := ParseConfig([]byte(""))
	if err != nil {
		t.Fatalf("ParseConfig empty: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Errorf("empty config = %+v, want %+v", cfg, DefaultConfig())
	}

	// 2. Valid block
	yamlData := `
enabled: true
local_timezone: "America/New_York"
quiet_hours: "22:00-07:00"
heartbeat:
  daily_at: "08:15"
max_announce_per_day: 12
catchup_window: 12h
`
	cfg, err = ParseConfig([]byte(yamlData))
	if err != nil {
		t.Fatalf("ParseConfig valid: %v", err)
	}
	if !cfg.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if cfg.LocalTimezone != "America/New_York" {
		t.Errorf("LocalTimezone = %q, want America/New_York", cfg.LocalTimezone)
	}
	if cfg.QuietHours != "22:00-07:00" {
		t.Errorf("QuietHours = %q, want 22:00-07:00", cfg.QuietHours)
	}
	if cfg.Heartbeat.DailyAt != "08:15" {
		t.Errorf("Heartbeat.DailyAt = %q, want 08:15", cfg.Heartbeat.DailyAt)
	}
	if cfg.MaxAnnouncePerDay != 12 {
		t.Errorf("MaxAnnouncePerDay = %d, want 12", cfg.MaxAnnouncePerDay)
	}
	if cfg.CatchupWindow != 12*time.Hour {
		t.Errorf("CatchupWindow = %v, want 12h", cfg.CatchupWindow)
	}

	// 3. Unknown keys must fail (spec D11, KnownFields(true) strictness)
	unknownYAML := `
enabled: true
unknown_key_typo: "surprise"
`
	_, err = ParseConfig([]byte(unknownYAML))
	if err == nil {
		t.Fatalf("ParseConfig with unknown key expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "unknown") {
		t.Logf("expected unknown field error message, got %v", err)
	}

	// 4. Wrapped fasti: block
	wrappedYAML := `
fasti:
  enabled: true
  max_announce_per_day: 5
`
	cfg, err = ParseConfig([]byte(wrappedYAML))
	if err != nil {
		t.Fatalf("ParseConfig wrapped: %v", err)
	}
	if !cfg.Enabled || cfg.MaxAnnouncePerDay != 5 {
		t.Errorf("wrapped config = %+v", cfg)
	}

	// 5. Spec M7: explicit max_announce_per_day: 0 must remain 0 (not become 8)
	zeroYAML := `
enabled: true
max_announce_per_day: 0
`
	cfg, err = ParseConfig([]byte(zeroYAML))
	if err != nil {
		t.Fatalf("ParseConfig explicit zero: %v", err)
	}
	if cfg.MaxAnnouncePerDay != 0 {
		t.Errorf("explicit max_announce_per_day: 0 became %d, want 0", cfg.MaxAnnouncePerDay)
	}

	wrappedZeroYAML := `
fasti:
  max_announce_per_day: 0
`
	cfg, err = ParseConfig([]byte(wrappedZeroYAML))
	if err != nil {
		t.Fatalf("ParseConfig wrapped explicit zero: %v", err)
	}
	if cfg.MaxAnnouncePerDay != 0 {
		t.Errorf("wrapped max_announce_per_day: 0 became %d, want 0", cfg.MaxAnnouncePerDay)
	}

	// 6. Spec M7: explicit catchup_window: 0s must be rejected by Validate()
	zeroCatchupYAML := `
catchup_window: 0s
`
	_, err = ParseConfig([]byte(zeroCatchupYAML))
	if err == nil {
		t.Fatalf("ParseConfig explicit zero catchup_window expected error, got nil")
	}

	// 7. Spec M8: wrapped decode error must be returned verbatim, not swallow and say "field fasti not found"
	wrappedUnknownYAML := `
fasti:
  unknown_wrapped_field: "oops"
`
	_, err = ParseConfig([]byte(wrappedUnknownYAML))
	if err == nil {
		t.Fatalf("ParseConfig wrapped unknown key expected error, got nil")
	}
	if strings.Contains(err.Error(), "field fasti not found") {
		t.Fatalf("ParseConfig wrapped error swallowed and returned misleading %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown_wrapped_field") {
		t.Errorf("expected error to name unknown_wrapped_field, got: %v", err)
	}
}

func TestConfig_Validation(t *testing.T) {
	tests := []struct {
		name     string
		modify   func(*Config)
		wantFail bool
	}{
		{
			name:   "valid default",
			modify: func(c *Config) {},
		},
		{
			name: "invalid timezone",
			modify: func(c *Config) {
				c.LocalTimezone = "Invalid/Nonexistent_Zone"
			},
			wantFail: true,
		},
		{
			name: "invalid quiet hours format",
			modify: func(c *Config) {
				c.QuietHours = "25:00-07:00"
			},
			wantFail: true,
		},
		{
			name: "invalid quiet hours separator",
			modify: func(c *Config) {
				c.QuietHours = "22:00 to 07:00"
			},
			wantFail: true,
		},
		{
			name: "invalid heartbeat daily_at",
			modify: func(c *Config) {
				c.Heartbeat.DailyAt = "07:65"
			},
			wantFail: true,
		},
		{
			name: "negative max announce",
			modify: func(c *Config) {
				c.MaxAnnouncePerDay = -1
			},
			wantFail: true,
		},
		{
			name: "zero catchup window",
			modify: func(c *Config) {
				c.CatchupWindow = 0
			},
			wantFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(&cfg)
			err := cfg.Validate()
			if tt.wantFail && err == nil {
				t.Fatalf("Validate expected error, got nil")
			}
			if !tt.wantFail && err != nil {
				t.Fatalf("Validate unexpected error: %v", err)
			}
		})
	}
}
