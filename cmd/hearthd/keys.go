package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/lararium-app/lararium/internal/keystore"
	"github.com/lararium-app/lararium/internal/surface"
)

// `hearthd keys list|set|rm` (KEYS-SPEC K3). The CLI never talks HTTP:
// it writes keys.json under the same flock the daemon uses, then pings
// the control socket. Any socket failure means "no daemon" — the write
// already succeeded, so the command still exits 0.

const cliReloadTimeout = 5 * time.Second

func keysCmd(cfgPath string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hearthd keys list|set <provider>|rm <provider>")
		os.Exit(2)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	store := keystore.New(cfg.Hearth.Home)
	socket := filepath.Join(cfg.Hearth.Home, "hearthd.sock")

	switch args[0] {
	case "list":
		keysList(cfg, store)
	case "set":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: hearthd keys set <provider>")
			os.Exit(2)
		}
		keysSet(store, socket, args[1])
	case "rm":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: hearthd keys rm <provider>")
			os.Exit(2)
		}
		keysRemove(store, socket, args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: hearthd keys list|set <provider>|rm <provider>")
		os.Exit(2)
	}
}

// providerInfosFromConfig maps config providers to resolution inputs.
func providerInfosFromConfig(cfg *Config) []keystore.ProviderInfo {
	infos := make([]keystore.ProviderInfo, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		infos = append(infos, keystore.ProviderInfo{
			Name: p.Name, EnvVar: p.APIKeyEnv, Literal: p.APIKey,
		})
	}
	return infos
}

func keysList(cfg *Config, store *keystore.Store) {
	infos := providerInfosFromConfig(cfg)
	// Union with keys.json-only names (force-added providers list too).
	names := map[string]bool{}
	for _, p := range infos {
		names[p.Name] = true
	}
	if m, err := store.Read(); err == nil {
		for n := range m {
			names[n] = true
		}
	}
	byName := map[string]keystore.ProviderInfo{}
	for _, p := range infos {
		byName[p.Name] = p
	}
	var extra []string
	for n := range names {
		if _, ok := byName[n]; !ok {
			extra = append(extra, n)
		}
	}
	// Deterministic order for the extras.
	sortStrings(extra)
	for _, n := range extra {
		infos = append(infos, keystore.ProviderInfo{Name: n})
	}

	res, warns := keystore.ResolveAll(store, infos, os.Getenv)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, w)
	}
	for _, p := range infos {
		r := res[p.Name]
		if r.Value == "" {
			fmt.Printf("%s	missing\n", p.Name)
			continue
		}
		fmt.Printf("%s	%s	%s\n", p.Name, displayKey(r.Value), string(r.Source))
	}
}

// displayKey renders a key for the terminal: last 4 chars only. The
// value itself never goes to stdout (spec K3).
func displayKey(v string) string {
	if len(v) <= 4 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}

func keysSet(store *keystore.Store, socket, name string) {
	if !keystore.ValidName(name) {
		fmt.Fprintln(os.Stderr, "invalid provider name")
		os.Exit(1)
	}
	// Cap check before prompting (spec K3): a full store must not make
	// the user type a secret they cannot save. Overwrites are exempt —
	// Set enforces the cap itself and allows replacing an existing name.
	m, err := store.Read()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, exists := m[name]; !exists && len(m) >= keystore.MaxKeys {
		fmt.Fprintln(os.Stderr, "key store full")
		os.Exit(1)
	}

	key, err := readSecret(os.Stdin, term.IsTerminal(int(os.Stdin.Fd())))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := store.Set(name, key); err != nil {
		fmt.Fprintln(os.Stderr, err) // ErrEmpty ("empty key") surfaces here
		os.Exit(1)
	}
	fmt.Printf("saved %s\n", name)
	notifyReload(socket)
}

func keysRemove(store *keystore.Store, socket, name string) {
	if !keystore.ValidName(name) {
		fmt.Fprintln(os.Stderr, "invalid provider name")
		os.Exit(1)
	}
	// Absent short-circuits BEFORE the socket (spec K6/V12): no write,
	// no reload ping, no bogus "will apply" line.
	ok, err := store.Remove(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !ok {
		fmt.Println("not set")
		return
	}
	fmt.Printf("removed %s\n", name)
	notifyReload(socket)
}

// readSecret reads the key: hidden prompt on a TTY, one stdin line
// otherwise (piping keeps scripts working). The value never touches
// argv, logs, or output.
func readSecret(stdin *os.File, isTTY bool) (string, error) {
	if !isTTY {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("empty key")
		}
		return line, nil
	}
	fmt.Fprint(os.Stderr, "key (input hidden): ")
	b, err := term.ReadPassword(int(stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// notifyReload pings the daemon. Every failure path exits 0 — the
// write is the contract (spec K6).
func notifyReload(socket string) {
	err := surface.ReloadViaSocket(socket, cliReloadTimeout)
	switch {
	case err == nil:
		// Daemon reloaded; next request uses the new key.
	case errors.Is(err, surface.ErrNotListening):
		fmt.Println("no daemon running — will apply at next start")
	default:
		fmt.Println("saved; reload ack failed — restart the daemon to apply now")
	}
}

// sortStrings is sort.Strings without importing sort for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
