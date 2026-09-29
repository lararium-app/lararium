// Package main — hearthd: the Lararium hearth daemon (M1: config + session
// loop + REPL). See docs/ARCHITECTURE.md §2.1, docs/PENATUS-SPEC.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole of lararium.yaml. Unknown keys are an ERROR (fasti
// lesson: silent config drift is how daemons start doing surprise things).
type Config struct {
	Hearth    HearthConfig   `yaml:"hearth"`
	Models    ModelsConfig   `yaml:"models"`
	Providers []ProviderConf `yaml:"providers"`
}

type HearthConfig struct {
	// Home is the persona root directory (contains SOUL.md, MEMORY.md, sessions/).
	Home string `yaml:"home"`
	// CompactionTriggerPct: when live context exceeds this share of the
	// probed window, compact before the next completion. Spec default 80.
	CompactionTriggerPct int `yaml:"compaction_trigger_pct"`
}

type ModelsConfig struct {
	// Default is the chain for normal chat turns.
	Default []string `yaml:"default"`
	// Compact is the chain for summarization turns (cheap model OK).
	Compact []string `yaml:"compact"`
}

type ProviderConf struct {
	// Ref format used by chains: "<name>/<model>" e.g. "p40/qwen3.6-27b".
	Name      string `yaml:"name"`
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"`     // literal or env:VAR
	APIKeyEnv string `yaml:"api_key_env"` // preferred: name of env var
}

// LoadConfig reads and validates lararium.yaml.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Hearth.Home == "" {
		return nil, fmt.Errorf("config: hearth.home is required")
	}
	if len(c.Models.Default) == 0 {
		return nil, fmt.Errorf("config: models.default chain is empty")
	}
	if c.Hearth.CompactionTriggerPct <= 0 || c.Hearth.CompactionTriggerPct > 100 {
		c.Hearth.CompactionTriggerPct = 80
	}
	byName := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || p.BaseURL == "" {
			return nil, fmt.Errorf("config: provider missing name or base_url")
		}
		if byName[p.Name] {
			return nil, fmt.Errorf("config: duplicate provider %q", p.Name)
		}
		byName[p.Name] = true
	}
	for _, chain := range [][]string{c.Models.Default, c.Models.Compact} {
		for _, ref := range chain {
			if !byName[providerOf(ref)] {
				return nil, fmt.Errorf("config: model ref %q names unknown provider", ref)
			}
		}
	}
	return &c, nil
}

func providerOf(ref string) string {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[:i]
		}
	}
	return ref
}

func modelOf(ref string) string {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[i+1:]
		}
	}
	return ""
}

func main() {
	cfgPath := flag.String("config", "lararium.yaml", "path to lararium.yaml")
	modelFlag := flag.String("model", "", "override: model ref for this run")
	flag.Parse()

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	chain := cfg.Models.Default
	if *modelFlag != "" {
		chain = []string{*modelFlag}
	}

	rt, err := buildRuntime(cfg, chain)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rt.Close()

	repl(rt)
}
