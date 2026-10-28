// Package main — hearthd: the Lararium hearth daemon (M1: config + session
// loop + REPL). See docs/ARCHITECTURE.md §2.1, docs/PENATUS-SPEC.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
	"github.com/lararium-app/lararium/web"
)

// Config is the whole of lararium.yaml. Unknown keys are an ERROR (fasti
// lesson: silent config drift is how daemons start doing surprise things).
type Config struct {
	Hearth    HearthConfig        `yaml:"hearth"`
	Models    ModelsConfig        `yaml:"models"`
	Providers []ProviderConf      `yaml:"providers"`
	Serve     surface.ServeConfig `yaml:"serve"`
	// Nuntius is the Telegram bridge configuration (NUNTIUS-SPEC §4).
	Nuntius nuntius.Config `yaml:"nuntius"`
	// Custos configures the custody door client (CUSTOS-SPEC §6.4b).
	Custos CustosConfig `yaml:"custos"`
	// Backup configures backup sealing and identity (PENATUS §4.5.2a).
	Backup BackupConfig `yaml:"backup"`
}

type BackupConfig struct {
	EncryptTo []string `yaml:"encrypt_to"`
	Identity  string   `yaml:"identity"`
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
	for _, recipientStr := range c.Backup.EncryptTo {
		if _, err := backup.ParseRecipient(recipientStr); err != nil {
			fmt.Fprintf(os.Stderr, "warning: config: invalid backup.encrypt_to recipient %q: %v\n", recipientStr, err)
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

func hoistGlobalFlags(args []string) ([]string, error) {
	var hoisted []string
	var rest []string
	stopped := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if stopped {
			rest = append(rest, arg)
			continue
		}
		if arg == "--" {
			stopped = true
			rest = append(rest, arg)
			continue
		}

		flagName := ""
		if strings.HasPrefix(arg, "--") {
			flagName = arg[2:]
		} else if strings.HasPrefix(arg, "-") {
			flagName = arg[1:]
		}

		if flagName != "" {
			name := flagName
			hasEq := false
			if eq := strings.IndexByte(flagName, '='); eq >= 0 {
				name = flagName[:eq]
				hasEq = true
			}
			if name == "config" || name == "model" {
				if hasEq {
					hoisted = append(hoisted, arg)
					continue
				}
				tail := args[i+1:]
				if len(tail) == 0 {
					return nil, fmt.Errorf("flag needs an argument: %s", arg)
				}
				i++
				hoisted = append(hoisted, arg, tail[0])
				continue
			}
		}

		rest = append(rest, arg)
	}

	return append(hoisted, rest...), nil
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
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: hearthd [options] [command]\n\nCommands:\n  serve\n  setup\n  token create|revoke <label>\n  keys list|set|rm\n  pair\n  backup create|list|verify|extract-config\n  restore <file> --to <dir> | --replace <dir> --yes [--no-safety]\n\n%s\n\nOptions:\n", backup.SecretsHonestyLine)
		flag.PrintDefaults()
	}

	hoisted, err := hoistGlobalFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(2)
	}
	os.Args = append(os.Args[:1], hoisted...)
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			if hasHelpFlag(args[1:]) {
				serveHelp()
				return
			}
			serve(*cfgPath)
			return
		case "setup":
			if hasHelpFlag(args[1:]) {
				setupHelp()
				return
			}
			setupCmd(*cfgPath, args[1:])
			return
		case "token":
			if hasHelpFlag(args[1:]) {
				tokenHelp(args[1:])
				return
			}
			tokenCmd(*cfgPath, args[1:])
			return
		case "keys":
			if hasHelpFlag(args[1:]) {
				keysHelp(args[1:])
				return
			}
			keysCmd(*cfgPath, args[1:])
			return
		case "pair":
			if hasHelpFlag(args[1:]) {
				pairHelp(args[1:])
				return
			}
			pairCmd(*cfgPath, args[1:])
			return
		case "backup":
			if hasHelpFlag(args[1:]) {
				backupHelp(args[1:])
				return
			}
			backupCmd(*cfgPath, args[1:])
			return
		case "restore":
			if hasHelpFlag(args[1:]) {
				restoreHelp()
				return
			}
			restoreCmd(*cfgPath, args[1:])
			return
		default:
			if hasHelpFlag(args) {
				flag.Usage()
				os.Exit(0)
			}
			if !strings.HasPrefix(args[0], "-") {
				fmt.Fprintf(os.Stderr, "hearthd: unknown command %q\n", args[0])
				flag.Usage()
				fmt.Fprintf(os.Stderr, "\nexample: hearthd -config ./lararium.yaml token create laptop\n")
				os.Exit(2)
			}
		}
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg.Hearth.Home = custos.ResolveHearthHome("", cfg.Hearth.Home)
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
	// "list" takes no label; create/revoke do.
	if len(args) == 0 || (args[0] != "list" && len(args) < 2) {
		fmt.Fprintln(os.Stderr, "usage: hearthd token create|revoke <label> | list")
		os.Exit(2)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg.Hearth.Home = custos.ResolveHearthHome("", cfg.Hearth.Home)
	switch args[0] {
	case "create":
		token, err := createDoorToken(cfg, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(token)
		fmt.Printf("Open: http://%s/#%s\n", serveHostPort(cfg), token)
	case "revoke":
		store, err := surface.OpenTokenStore(filepath.Join(cfg.Hearth.Home, "tokens.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
	case "list":
		// L-7: nothing may stay live invisibly. Display-only view —
		// hash prefixes, never plaintext.
		store, err := surface.OpenTokenStore(filepath.Join(cfg.Hearth.Home, "tokens.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		live, err := store.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(live) == 0 {
			fmt.Println("(no API tokens are live)")
			return
		}
		for _, t := range live {
			fmt.Printf("%-24s %s…  created %s\n", t.Label, t.Prefix, t.Created)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: hearthd token create|revoke <label> | list")
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
	cfg.Hearth.Home = custos.ResolveHearthHome("", cfg.Hearth.Home)
	// Fill spec defaults (port, approval/turn timeouts) BEFORE anything
	// reads cfg.Serve: a zero timeout would arm already-expired contexts.
	if err := cfg.Serve.Normalize(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	home := cfg.Hearth.Home
	if err := loop.ValidatePersona(home); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := backup.RecoverSwap(home); err != nil {
		fmt.Fprintf(os.Stderr, "swap recovery failed: %v\n", err)
		os.Exit(1)
	}

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
	logServeWarnings(warnings)

	// ApprovalHub and Hub
	ap := surface.NewApprovalHub(cfg.Serve.ApprovalTimeout)
	hub := surface.NewHub(cfg.Serve, home, cfg.Hearth.MaxTokens, cfg.Hearth.CompactionTriggerPct, tools, rt, ap)
	hub.AttachKeys(keyReg)

	sessions := surface.NewPenatusSource(home, hub)

	srv := &surface.Server{
		Cfg:       cfg.Serve,
		Store:     store,
		Sessions:  sessions,
		Hub:       hub,
		PairStore: newPairStoreAdapter(openPairStore(home)),
	}
	// Issue #128: turn-approval cards (web chat + MCP rail) become visible
	// and resolvable through the custody card surface.
	srv.SetApprovalHub(ap)
	srv.AttachKeys(&surface.KeysDeps{
		Store:     kstore,
		Registry:  keyReg,
		Providers: kp.ProviderInfos,
		Reload:    reloadKeys,
	})

	srv.BackupCheck = func() error {
		if len(cfg.Backup.EncryptTo) == 0 {
			return errors.New("no recipients configured")
		}
		return nil
	}
	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		if len(cfg.Backup.EncryptTo) == 0 {
			return errors.New("no recipients configured")
		}
		recipients, err := backup.ParseRecipients(cfg.Backup.EncryptTo)
		if err != nil {
			return fmt.Errorf("invalid recipient in backup.encrypt_to: %w", err)
		}
		return backup.CreateFromDaemon(home, cfgPath, outAbs, noConfig, hub.SingleWriterLock, progress, recipients...)
	}

	// Control socket (KEYS-SPEC K6): CLI writes, then pings here.
	sock, err := srv.ServeSocket(filepath.Join(home, "hearthd.sock"), reloadKeys)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	bridge, stopNuntius, err := startNuntius(context.Background(), cfg, home, hub, sessions, ap)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		sock.Close()
		os.Exit(1)
	}
	// Custody door client: nil stop (and nothing started) unless both
	// custos.doors_sock and custos.door_token are set.
	decider := wireCustosPolicy(srv, home, cfg.Custos)
	stopDoor := startCustosDoor(context.Background(), cfg.Custos, bridge, srv, hub, decider)
	stopBridge := func() {
		if stopDoor != nil {
			stopDoor()
		}
		if stopNuntius != nil {
			stopNuntius()
		}
	}
	if stopDoor != nil || stopNuntius != nil {
		srv.OnShutdown = stopBridge
	}

	fmt.Printf("hearthd serve: web chat + API on http://%s\n", serveHostPort(cfg))
	fmt.Printf("  open a door:  hearthd token create <label>   (prints a ready URL, shown once)\n")

	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		sock.Close()
		stopBridge()
		os.Exit(1)
	}
	sock.Close()
	stopBridge()
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}

func hasSubverb(args []string, verb string) bool {
	for _, a := range args {
		if a == verb {
			return true
		}
	}
	return false
}

func tokenHelp(args []string) {
	if hasSubverb(args, "create") {
		fmt.Fprintln(os.Stderr, "usage: hearthd token create <label>")
		os.Exit(0)
	}
	if hasSubverb(args, "revoke") {
		fmt.Fprintln(os.Stderr, "usage: hearthd token revoke <label>")
		os.Exit(0)
	}
	if hasSubverb(args, "list") {
		fmt.Fprintln(os.Stderr, "usage: hearthd token list")
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "usage: hearthd token create|revoke <label> | list")
	os.Exit(0)
}

func keysHelp(args []string) {
	if hasSubverb(args, "set") {
		fmt.Fprintln(os.Stderr, "usage: hearthd keys set <provider>")
		os.Exit(0)
	}
	if hasSubverb(args, "rm") {
		fmt.Fprintln(os.Stderr, "usage: hearthd keys rm <provider>")
		os.Exit(0)
	}
	if hasSubverb(args, "list") {
		fmt.Fprintln(os.Stderr, "usage: hearthd keys list")
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "usage: hearthd keys list|set <provider>|rm <provider>")
	os.Exit(0)
}

func pairHelp(args []string) {
	if hasSubverb(args, "create") {
		fmt.Fprintln(os.Stderr, "usage: hearthd pair create [--stdout]")
		os.Exit(0)
	}
	if hasSubverb(args, "revoke") {
		fmt.Fprintln(os.Stderr, "usage: hearthd pair revoke --all")
		os.Exit(0)
	}
	if hasSubverb(args, "status") {
		fmt.Fprintln(os.Stderr, "usage: hearthd pair status")
		os.Exit(0)
	}
	if hasSubverb(args, "unpair") {
		fmt.Fprintln(os.Stderr, "usage: hearthd pair unpair <user_id>")
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "usage: hearthd pair create|revoke --all|status|unpair <user_id>")
	os.Exit(0)
}

func backupHelp(args []string) {
	if hasSubverb(args, "create") {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup create [--out <path>] [--no-config] [--to <age1...>] [--passphrase] [--plaintext]\n\n%s\n", backup.SecretsHonestyLine)
		os.Exit(0)
	}
	if hasSubverb(args, "list") {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup list <file> [--identity <file>]\n")
		os.Exit(0)
	}
	if hasSubverb(args, "verify") {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup verify <file> [--identity <file>]\n")
		os.Exit(0)
	}
	if hasSubverb(args, "extract-config") {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup extract-config <file> <path> [--identity <file>]\n")
		os.Exit(0)
	}
	if hasSubverb(args, "keygen") {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup keygen [--out <identity-file>]\n")
		os.Exit(0)
	}
	printBackupUsage()
	os.Exit(0)
}

func restoreHelp() {
	printRestoreUsage()
	os.Exit(0)
}

func setupHelp() {
	fmt.Fprintf(os.Stderr, "Usage: hearthd setup [--config <path>] [--defaults]\n\nInteractive AI provider and model setup wizard.\n")
	os.Exit(0)
}

// logServeWarnings prints config/system-prompt warnings, then warns when
// the embedded web bundle carries no JS assets (a stub dist), so a source
// checkout without `npm run build` says so at boot instead of serving a
// blank chat UI.
func logServeWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "⚠ "+w)
	}
	if surface.IsStubWebFS(web.FS) {
		fmt.Fprintln(os.Stderr, "warning: web assets not built — run (cd web && npm ci && npm run build) and rebuild, or use the official Docker image; the chat UI will not load")
	}
}

func serveHelp() {
	fmt.Fprintf(os.Stderr, "Usage: hearthd serve [--config <path>] [--model <model>]\n")
	os.Exit(0)
}

// openPairStore opens the nuntius pairing store for the surface
// (issue #184). A pairing store that cannot open is a fatal serve-time
// fault, same policy as the token store next door.
func openPairStore(home string) *nuntius.PairStore {
	ps, err := nuntius.NewPairStore(filepath.Join(home, "nuntius"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return ps
}
