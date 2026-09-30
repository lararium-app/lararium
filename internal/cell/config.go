package cell

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Limits defines resource constraints for a cell.
type Limits struct {
	MemoryMB    int    `yaml:"memory_mb"`
	CPUQuota    string `yaml:"cpu_quota"`
	TasksMax    int    `yaml:"tasks_max"`
	DiskQuotaMB int    `yaml:"disk_quota_mb"`
	Ownership   string `yaml:"ownership"`
	ProxyURL    string `yaml:"proxy_url"`
}

// DefaultLimits returns the spec §5 defaults.
func DefaultLimits() Limits {
	return Limits{
		MemoryMB:    8192, // 8G
		CPUQuota:    "200",
		TasksMax:    512,
		DiskQuotaMB: 0, // 0 = unlimited
		Ownership:   "auto",
		ProxyURL:    "",
	}
}

// Config represents the lararium.yaml configuration.
type Config struct {
	SchemaVersion int    `yaml:"schema_version"`
	Cell          Limits `yaml:"cell"`
	Hearth        string `yaml:"hearth"`
}

// LoadConfig reads lararium.yaml and returns a Config with defaults applied.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults for unset values.
	def := DefaultLimits()
	if cfg.Cell.MemoryMB == 0 {
		cfg.Cell.MemoryMB = def.MemoryMB
	}
	if cfg.Cell.CPUQuota == "" {
		cfg.Cell.CPUQuota = def.CPUQuota
	}
	if cfg.Cell.TasksMax == 0 {
		cfg.Cell.TasksMax = def.TasksMax
	}
	if cfg.Cell.Ownership == "" {
		cfg.Cell.Ownership = def.Ownership
	}

	return &cfg, nil
}
