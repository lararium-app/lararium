// Package main — hearthd: the Lararium hearth daemon (M1: config + session
// loop + REPL). See docs/ARCHITECTURE.md §2.1, docs/PENATUS-SPEC.md.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

// Config is the whole of lararium.yaml. Unknown keys are an ERROR (fasti
// lesson: silent config drift is how daemons start doing surprise things).
type Config struct {
	Hearth    HearthConfig        `yaml:"hearth"`
	Models    ModelsConfig        `yaml:"models"`
	Providers []ProviderConf      `yaml:"providers"`
	Serve     surface.ServeConfig `yaml:"serve"`
}

type HearthConfig struct {
	// Home is the persona root directory (contains SOUL.md, MEMORY.md, sessions/).
	Home string `yaml:"home"`
	// CompactionTriggerPct: when live context exceeds this share of the
	// probed window, compact before the next completion. Spec default 80.
	CompactionTriggerPct int `yaml:"compaction_trigger_pct"`
	// MaxTokens caps each generation (0 = provider default).
	MaxTokens int `yaml:"max_tokens"`
}

type ModelsConfig struct {
	// Default is the chain for normal chat turns.
	Default []string `yaml:"default"`
	// Compact is the chain for summarization turns (cheap model OK).
	Compact []string `yaml:"compact"`
}

type ProviderConf struct {
	// Ref format used by chains: "<name>/<model>" e.g. "local/llama-model".
	Name      string `yaml:"name"`
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"`     // literal or env:VAR
	APIKeyEnv string `yaml:"api_key_env"` // preferred: name of env var
	// Think: false disables model-side reasoning chains on servers whose
	// chat template supports it (llama.cpp: enable_thinking=false).
	// Detection: template support is probed per provider at startup.
	Think *bool `yaml:"think"`
}

// LoadConfig reads and validates lararium.yaml.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the --config flag or operator-set LARARIUM_CONFIG env: same trust as any CLI arg
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
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[:i]
	}
	return ref
}

func modelOf(ref string) string {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

func main() {
	// LARARIUM_CONFIG lets container images set the config path once;
	// the flag still wins when given.
	defaultCfg := "lararium.yaml"
	if e := os.Getenv("LARARIUM_CONFIG"); e != "" {
		defaultCfg = e
	}
	cfgPath := flag.String("config", defaultCfg, "path to lararium.yaml")
	modelFlag := flag.String("model", "", "override: model ref for this run")
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			serve(*cfgPath)
			return
		case "token":
			tokenCmd(*cfgPath, args[1:])
			return
		case "keys":
			keysCmd(*cfgPath, args[1:])
			return
		}
	}

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

// tokenCmd implements `hearthd token create|revoke <label>` (spec §3).
// Create prints the plaintext exactly once, plus the ready URL whose
// fragment never reaches a proxy log.
func tokenCmd(cfgPath string, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hearthd token create|revoke <label>")
		os.Exit(2)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	store, err := surface.OpenTokenStore(filepath.Join(cfg.Hearth.Home, "tokens.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch args[0] {
	case "create":
		token, err := store.Create(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(token)
		fmt.Printf("Open: http://%s/#%s\n", serveHostPort(cfg), token)
	case "revoke":
		ok, err := store.Revoke(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "no token with label %q\n", args[1])
			os.Exit(1)
		}
		fmt.Printf("revoked %q (effective on the daemon's next request)\n", args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: hearthd token create|revoke <label>")
		os.Exit(2)
	}
}

// serveHostPort is the host:port the ready URL points at; a wildcard or
// empty bind is reported as loopback (the operator opens this browser).
// The port always comes from the config, never a hardcoded default.
func serveHostPort(cfg *Config) string {
	host, port, err := net.SplitHostPort(cfg.Serve.Listen)
	if err != nil {
		host, port = cfg.Serve.Listen, "7717"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func serve(cfgPath string) {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Fill spec defaults (port, approval/turn timeouts) BEFORE anything
	// reads cfg.Serve: a zero timeout would arm already-expired contexts.
	if err := cfg.Serve.Normalize(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	home := cfg.Hearth.Home
	tokensPath := filepath.Join(home, "tokens.json")
	store, err := surface.OpenTokenStore(tokensPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Build router (shared plumbing with the REPL — audit 5.3/5.7:
	// one K2 resolver, one provider builder, provider id = config name).
	kp, err := newKeyPlumbing(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	byName := kp.BuildProviders(cfg)
	keyReg := kp.Registry
	kstore := kp.Store
	reloadKeys := kp.Reload

	targets := func(refs []string) ([]router.Target, error) {
		var ts []router.Target
		for _, ref := range refs {
			prov, ok := byName[providerOf(ref)]
			if !ok {
				return nil, fmt.Errorf("unknown provider in ref %q", ref)
			}
			ts = append(ts, router.Target{Provider: prov, Model: modelOf(ref)})
		}
		return ts, nil
	}

	def, err := targets(cfg.Models.Default)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rt := router.NewRouter(router.Profile{Chain: def})
	if len(cfg.Models.Compact) > 0 {
		ct, err := targets(cfg.Models.Compact)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		rt.SetProfile("compact", router.Profile{Chain: ct})
	} else {
		rt.SetProfile("compact", router.Profile{Chain: def})
	}

	// Build tools
	tools := buildTools(home)

	// System prompt (validated by Hub per-session)
	_, warnings, err := loop.SystemPrompt(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "⚠ "+w)
	}

	// ApprovalHub and Hub
	ap := surface.NewApprovalHub(cfg.Serve.ApprovalTimeout)
	hub := surface.NewHub(cfg.Serve, home, cfg.Hearth.MaxTokens, cfg.Hearth.CompactionTriggerPct, tools, rt, ap)
	hub.AttachKeys(keyReg)

	sessions := surface.NewPenatusSource(home, hub)

	srv := &surface.Server{
		Cfg:      cfg.Serve,
		Store:    store,
		Sessions: sessions,
		Hub:      hub,
	}
	srv.AttachKeys(&surface.KeysDeps{
		Store:     kstore,
		Registry:  keyReg,
		Providers: kp.ProviderInfos,
		Reload:    reloadKeys,
	})

	// Control socket (KEYS-SPEC K6): CLI writes, then pings here.
	sock, err := srv.ServeSocket(filepath.Join(home, "hearthd.sock"), reloadKeys)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("hearthd serve: web chat + API on http://%s\n", serveHostPort(cfg))
	fmt.Printf("  open a door:  hearthd token create <label>   (prints a ready URL, shown once)\n")

	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		sock.Close()
		os.Exit(1)
	}
	sock.Close()
}
