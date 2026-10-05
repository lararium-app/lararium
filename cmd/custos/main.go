// Package main implements the custos CLI tool per CUSTOS-SPEC §11.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/lararium-app/lararium/internal/custos"
)

type ConfigFile struct {
	Hearth struct {
		Home string `yaml:"home"`
	} `yaml:"hearth"`
	Custos custos.Config `yaml:"custos"`
}

func loadConfig(cfgPath, hearthOverride string) (string, *custos.Config) {
	var c ConfigFile
	if raw, err := os.ReadFile(cfgPath); err == nil { //nolint:gosec // operator-provided config path
		_ = yaml.Unmarshal(raw, &c)
	}

	home := c.Hearth.Home
	if hearthOverride != "" {
		home = hearthOverride
	}
	if home == "" {
		if env := os.Getenv("HEARTH_HOME"); env != "" {
			home = env
		} else {
			home = "."
		}
	}

	stateDir := filepath.Join(home, "custos")
	c.Custos.Normalize()
	return stateDir, &c.Custos
}

var stdinReader = bufio.NewReader(os.Stdin)

func readPassphrase(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := stdinReader.ReadString('\n')
		if err != nil && line == "" {
			return "", custos.ErrEmpty
		}
		pass := strings.TrimSpace(line)
		if pass == "" {
			return "", custos.ErrEmpty
		}
		return pass, nil
	}

	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	pass := strings.TrimSpace(string(b))
	if pass == "" {
		return "", custos.ErrEmpty
	}
	return pass, nil
}

func runInit(v *custos.Vault, keyfilePath string) int {
	var passphrase string
	if keyfilePath != "" {
		b, err := os.ReadFile(keyfilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read keyfile: %v\n", err)
			return 1
		}
		passphrase = strings.TrimSpace(string(b))
		if passphrase == "" {
			fmt.Fprintln(os.Stderr, custos.ErrEmpty.Error())
			return 1
		}
	} else {
		p, err := readPassphrase("passphrase (input hidden): ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		passphrase = p
	}

	if err := v.Init(passphrase); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

func runUnlock(v *custos.Vault, stateDir, keyfilePath string, cfg *custos.Config) int {
	var passphrase string
	isKeyfile := false
	if keyfilePath != "" {
		b, err := os.ReadFile(keyfilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read keyfile: %v\n", err)
			return 1
		}
		passphrase = strings.TrimSpace(string(b))
		if passphrase == "" {
			fmt.Fprintln(os.Stderr, custos.ErrEmpty.Error())
			return 1
		}
		isKeyfile = true
	} else {
		p, err := readPassphrase("passphrase (input hidden): ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		passphrase = p
	}

	// Try daemon over ctl.sock first
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTripWithArg("UNLOCK", passphrase)
		if dialErr == nil {
			fmt.Println(resp)
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			// Daemon answered with an error
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	// File-direct unlock per CUSTOS §10 V17
	if err := v.Unlock(passphrase, isKeyfile); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Println("unlocked")
	return 0
}

func runLock(stateDir, keyfilePath string, cfg *custos.Config) int {
	if keyfilePath != "" {
		// CUSTOS §C3: custos lock refuses in keyfile mode
		fmt.Fprintln(os.Stderr, custos.ErrKeyfileModeAlwaysUnlocked.Error())
		return 1
	}

	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTrip("LOCK")
		if dialErr == nil {
			fmt.Println(resp)
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}
	// Daemon not running: already locked
	fmt.Println("locked")
	return 0
}

func runStatus(v *custos.Vault, stateDir string, cfg *custos.Config) int {
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTrip("STATUS")
		if dialErr == nil {
			fmt.Println(resp)
			return 0
		}
	}

	// File-direct status
	st := v.Status(false, 0)
	b, err := json.Marshal(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Println(string(b))
	return 0
}

func runAudit(v *custos.Vault, subArgs []string) int {
	if len(subArgs) == 0 || subArgs[0] != "verify" {
		fmt.Fprintf(os.Stderr, "usage: custos audit verify\n")
		return 2
	}

	key, _ := v.LoadInstanceKey()
	res, err := v.Audit().Verify(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	out := res.Format()
	if res.IsBroken || res.IsDangling {
		// Frozen error output to stderr, exit 1
		fmt.Fprintln(os.Stderr, out)
		return 1
	}
	// Frozen data output to stdout, exit 0
	fmt.Println(out)
	return 0
}

func runSnapshots(v *custos.Vault, subArgs []string) int {
	if len(subArgs) == 0 || subArgs[0] != "list" {
		fmt.Fprintf(os.Stderr, "usage: custos snapshots list\n")
		return 2
	}

	gens, err := v.Snapshots().ListGenerations()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	// Line-oriented, tab-separated output per CUSTOS §11
	fmt.Println("generation")
	for _, g := range gens {
		fmt.Println(g)
	}
	return 0
}

func runRestore(v *custos.Vault, stateDir string, cfg *custos.Config, subArgs []string, yes bool) int {
	if len(subArgs) == 0 {
		fmt.Fprintf(os.Stderr, "usage: custos restore <gen> --yes\n")
		return 2
	}

	gen, err := strconv.ParseInt(subArgs[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid generation %q\n", subArgs[0])
		return 1
	}

	if !yes {
		fmt.Fprintf(os.Stderr, "restore requires --yes\n")
		return 1
	}

	unlock, err := custos.NewLockFile(stateDir, cfg.LockWaitTimeout).Lock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	defer unlock()

	key, err := v.LoadInstanceKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load instance key: %v\n", err)
		return 1
	}

	if err := v.Snapshots().Restore(gen, key, v.Audit()); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

func runChangePassphrase(v *custos.Vault, stateDir string, cfg *custos.Config) int {
	oldPass, err := readPassphrase("current passphrase (input hidden): ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	newPass, err := readPassphrase("new passphrase (input hidden): ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	// Try daemon first
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTripWithArg("CHANGE-PASSPHRASE", oldPass+"\x00"+newPass)
		if dialErr == nil {
			fmt.Println(resp)
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	// File-direct
	if err := v.ChangePassphrase(oldPass, newPass); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Println("passphrase changed")
	return 0
}

func main() {
	defaultCfg := "lararium.yaml"
	if e := os.Getenv("LARARIUM_CONFIG"); e != "" {
		defaultCfg = e
	}

	fs := flag.NewFlagSet("custos", flag.ContinueOnError)
	cfgFlag := fs.String("config", defaultCfg, "path to lararium.yaml")
	hearthFlag := fs.String("hearth", "", "override hearth root directory")
	keyfileFlag := fs.String("keyfile", "", "path to unlock keyfile")
	yesFlag := fs.Bool("yes", false, "confirm destructive action")

	// Allow flags anywhere before or after subcommand
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	args := fs.Args()
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: custos <verb> [options]\n")
		os.Exit(2)
	}

	verb := args[0]
	subArgs := args[1:]

	stateDir, cfg := loadConfig(*cfgFlag, *hearthFlag)

	keyfilePath := *keyfileFlag
	if keyfilePath == "" {
		keyfilePath = cfg.UnlockKeyfile
	}

	v := custos.NewVault(stateDir, cfg.LockWaitTimeout)

	var exitCode int
	switch verb {
	case "init":
		exitCode = runInit(v, keyfilePath)
	case "unlock":
		exitCode = runUnlock(v, stateDir, keyfilePath, cfg)
	case "lock":
		exitCode = runLock(stateDir, keyfilePath, cfg)
	case "status":
		exitCode = runStatus(v, stateDir, cfg)
	case "audit":
		exitCode = runAudit(v, subArgs)
	case "snapshots":
		exitCode = runSnapshots(v, subArgs)
	case "restore":
		exitCode = runRestore(v, stateDir, cfg, subArgs, *yesFlag)
	case "change-passphrase":
		exitCode = runChangePassphrase(v, stateDir, cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown verb %q\n", verb)
		exitCode = 2
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
