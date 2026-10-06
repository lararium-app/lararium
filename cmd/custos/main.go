// Package main implements the custos CLI tool per CUSTOS-SPEC §11.
package main

import (
	"bufio"
	"context"
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

// runLogin implements `custos login gmail --client-id <id> [--manual]` per
// CUSTOS-SPEC §4.5, §11: the client secret is prompted (hidden echo) and
// never taken from argv; the consent URL is printed; on success the kind
// oauth2 credential is stored through the Mutate path (audit
// credential_added, actor cli). A refused consent (state mismatch/missing/
// denied/timeout) audits login_denied and stores nothing.
func runLogin(v *custos.Vault, keyfilePath string, subArgs []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	clientID := fs.String("client-id", "", "oauth2 client id")
	manual := fs.Bool("manual", false, "headless paste-back mode")
	credName := fs.String("cred", "", "vault credential name (default: connector name)")
	tokenURI := fs.String("token-uri", "", "token endpoint override (tests)")
	authURI := fs.String("auth-uri", "", "authorization endpoint override (tests)")
	connector := "gmail"
	rest := subArgs
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		connector = rest[0]
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if connector != "gmail" {
		fmt.Fprintf(os.Stderr, "unknown connector %q\n", connector)
		return 2
	}
	if *clientID == "" {
		fmt.Fprintln(os.Stderr, "usage: custos login gmail --client-id <id> [--manual]")
		return 2
	}

	// Client secret: prompted, never a flag/argv (§4.5). Zero the local
	// copy on every exit path once the login flow is done (§C3 hygiene;
	// LoginOptions may still hand the pointer to the exchange until then).
	secret, err := readPassphrase("client secret (input hidden): ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	defer custos.ZeroString(secret)

	// The vault must be available: unlock the direct handle under custos.lock.
	pass := getPassphrase(keyfilePath)
	defer custos.ZeroString(pass)
	if pass == "" {
		fmt.Fprintln(os.Stderr, custos.ErrCustosLocked.Error())
		return 1
	}
	if !v.IsUnlocked() {
		if err := v.Unlock(pass, keyfilePath != ""); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
	}

	ctx := context.Background()
	opts := &custos.LoginOptions{
		Connector:    connector,
		ClientID:     *clientID,
		ClientSecret: secret,
		Manual:       *manual,
		Out:          os.Stdout,
	}
	if *credName != "" {
		opts.CredName = *credName
	}
	if *tokenURI != "" {
		opts.TokenURI = *tokenURI
	}
	if *authURI != "" {
		opts.AuthURI = *authURI
	}
	if *manual {
		opts.PasteBack = func() (string, error) {
			fmt.Fprint(os.Stderr, "paste redirect URL or code: ")
			line, rerr := stdinReader.ReadString('\n')
			if rerr != nil && strings.TrimSpace(line) == "" {
				return "", rerr
			}
			return line, nil
		}
	}

	res, err := custos.OAuthLogin(ctx, opts)
	if err != nil {
		if reason := custos.LoginDeniedReason(err); reason != "" {
			_ = v.Audit().Append(custos.AuditRecord{
				Kind:   custos.AuditKindLoginDenied,
				Actor:  "cli",
				Reason: reason,
			})
		}
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	if err := v.StoreOAuthGrant("", res.Name, res.Credential, "cli"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Printf("stored oauth2 credential %q\n", res.Name)
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

func getPassphrase(keyfilePath string) string {
	if keyfilePath != "" {
		if b, err := os.ReadFile(keyfilePath); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := stdinReader.ReadString('\n')
		if err == nil {
			return strings.TrimSpace(line)
		}
	} else {
		fmt.Fprint(os.Stderr, "passphrase (input hidden): ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func runSurrogateAdd(v *custos.Vault, stateDir, keyfilePath string, cfg *custos.Config, subArgs []string) int {
	var posArgs []string
	var flagArgs []string
	for i := 0; i < len(subArgs[1:]); i++ {
		arg := subArgs[1+i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			eqIdx := strings.Index(arg, "=")
			if eqIdx == -1 {
				flagName := strings.TrimLeft(arg, "-")
				if (flagName == "host" || flagName == "path" || flagName == "port") && i+1 < len(subArgs[1:]) {
					i++
					flagArgs = append(flagArgs, subArgs[1+i])
				}
			}
		} else {
			posArgs = append(posArgs, arg)
		}
	}

	fs := flag.NewFlagSet("surrogate add", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "bound destination host")
	pathFlag := fs.String("path", "/", "bound path prefix")
	portFlag := fs.Int("port", 80, "bound port")
	allowBinaryFlag := fs.Bool("allow-binary", false, "allow binary response pass-through")

	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(posArgs) == 0 || *hostFlag == "" {
		fmt.Fprintf(os.Stderr, "usage: custos surrogate add <credential> --host <host> [--path <path>] [--port <port>] [--allow-binary]\n")
		return 2
	}
	credName := posArgs[0]

	// Try daemon first
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		req := struct {
			Credential  string `json:"credential"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			PathPrefix  string `json:"path_prefix"`
			AllowBinary bool   `json:"allow_binary"`
		}{
			Credential:  credName,
			Host:        *hostFlag,
			Port:        *portFlag,
			PathPrefix:  *pathFlag,
			AllowBinary: *allowBinaryFlag,
		}
		b, err := json.Marshal(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		resp, dialErr := client.RoundTripWithArg("SURROGATE-ADD", string(b))
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
	pass := getPassphrase(keyfilePath)
	if pass == "" {
		fmt.Fprintln(os.Stderr, custos.ErrCustosLocked.Error())
		return 1
	}
	if !v.IsUnlocked() {
		if err := v.Unlock(pass, keyfilePath != ""); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
	}
	tok, err := v.AddSurrogate(pass, credName, *hostFlag, *portFlag, *pathFlag, *allowBinaryFlag, "cli")
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Println(tok)
	return 0
}

func runSurrogateList(v *custos.Vault, stateDir, keyfilePath string, cfg *custos.Config) int {
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTrip("SURROGATE-LIST")
		if dialErr == nil {
			var list []custos.SurrogateRecord
			if err := json.Unmarshal([]byte(resp), &list); err == nil {
				fmt.Println("name\tfingerprint\tbinding")
				for _, r := range list {
					fmt.Printf("%s\t%s\t%s\n", r.Credential, r.Fingerprint(), r.BindingString())
				}
				return 0
			}
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	// File-direct
	pass := getPassphrase(keyfilePath)
	if pass == "" {
		fmt.Fprintln(os.Stderr, custos.ErrCustosLocked.Error())
		return 1
	}
	if !v.IsUnlocked() {
		if err := v.Unlock(pass, keyfilePath != ""); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
	}
	list, err := v.ListSurrogates()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Println("name\tfingerprint\tbinding")
	for _, r := range list {
		fmt.Printf("%s\t%s\t%s\n", r.Credential, r.Fingerprint(), r.BindingString())
	}
	return 0
}

func runSurrogateRevoke(v *custos.Vault, stateDir, keyfilePath string, cfg *custos.Config, subArgs []string) int {
	if len(subArgs) < 2 {
		fmt.Fprintf(os.Stderr, "usage: custos surrogate revoke <id8>\n")
		return 2
	}
	id8 := subArgs[1]
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTripWithArg("SURROGATE-REVOKE", id8)
		if dialErr == nil {
			_ = resp
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	// File-direct
	pass := getPassphrase(keyfilePath)
	if pass == "" {
		fmt.Fprintln(os.Stderr, custos.ErrCustosLocked.Error())
		return 1
	}
	if !v.IsUnlocked() {
		if err := v.Unlock(pass, keyfilePath != ""); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
	}
	if err := v.RevokeSurrogate(pass, id8, "cli"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

func runSurrogate(v *custos.Vault, stateDir, keyfilePath string, cfg *custos.Config, subArgs []string) int {
	if len(subArgs) == 0 {
		fmt.Fprintf(os.Stderr, "usage: custos surrogate add|list|revoke [options]\n")
		return 2
	}

	verb := subArgs[0]
	switch verb {
	case "add":
		return runSurrogateAdd(v, stateDir, keyfilePath, cfg, subArgs)
	case "list":
		return runSurrogateList(v, stateDir, keyfilePath, cfg)
	case "revoke":
		return runSurrogateRevoke(v, stateDir, keyfilePath, cfg, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown surrogate subcommand %q\n", verb)
		return 2
	}
}

func parsePolicyAddArgs(args []string) (lane, pattern, verdict string, err error) {
	if len(args) == 3 {
		if custos.ValidVerdict(args[2]) {
			return args[0], args[1], args[2], nil
		}
		if custos.ValidVerdict(args[0]) {
			return args[1], args[2], args[0], nil
		}
	} else if len(args) == 2 {
		switch {
		case custos.ValidVerdict(args[1]):
			pattern = args[0]
			verdict = args[1]
		case custos.ValidVerdict(args[0]):
			verdict = args[0]
			pattern = args[1]
		default:
			return "", "", "", errors.New("missing valid verdict")
		}

		// Infer lane from pattern
		if strings.Contains(pattern, ".") || strings.HasPrefix(pattern, "[") || (strings.HasSuffix(pattern, "/*") && strings.Contains(pattern[:len(pattern)-2], ".")) {
			lane = "egress"
		} else {
			hostPart := pattern
			if idx := strings.Index(pattern, ":"); idx != -1 {
				hostPart = pattern[:idx]
			}
			if _, isIP, _ := custos.NormalizeHost(hostPart); isIP {
				lane = "egress"
			} else {
				lane = "credential"
			}
		}
		return lane, pattern, verdict, nil
	}
	return "", "", "", errors.New("invalid arguments for policy add")
}

func runPolicyAdd(v *custos.Vault, stateDir string, cfg *custos.Config, subArgs []string) int {
	lane, pattern, verdict, err := parsePolicyAddArgs(subArgs[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: custos policy add [<lane>] <pattern> <verdict>\n")
		return 2
	}

	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		req := struct {
			Lane    string `json:"lane"`
			Pattern string `json:"pattern"`
			Verdict string `json:"verdict"`
		}{Lane: lane, Pattern: pattern, Verdict: verdict}
		b, err := json.Marshal(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		resp, dialErr := client.RoundTripWithArg("POLICY-ADD", string(b))
		if dialErr == nil {
			if resp != "" {
				notes := strings.Split(resp, "\x00")
				for _, n := range notes {
					if strings.TrimSpace(n) != "" {
						fmt.Fprintln(os.Stderr, n)
					}
				}
			}
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	// File-direct
	_ = v.Policy().LoadStrict()
	var notes []string
	var addErr error
	if lane == "egress" {
		notes, addErr = v.Policy().AddEgressRule(pattern, verdict, false, "cli", "cli")
	} else {
		notes, addErr = v.Policy().AddCredentialRule(pattern, verdict, false, "cli", "cli")
	}
	if addErr != nil {
		fmt.Fprintln(os.Stderr, addErr.Error())
		return 1
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, n)
	}
	return 0
}

func runPolicyList(v *custos.Vault, stateDir string, cfg *custos.Config) int {
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTrip("POLICY-LIST")
		if dialErr == nil {
			var rows []custos.PolicyRuleRow
			if err := json.Unmarshal([]byte(resp), &rows); err == nil {
				fmt.Println("lane\tpattern\tverdict\talways\tsource")
				for _, r := range rows {
					fmt.Printf("%s\t%s\t%s\t%t\t%s\n", r.Lane, r.Pattern, r.Verdict, r.Always, r.Source)
				}
				return 0
			}
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	_ = v.Policy().LoadStrict()
	rows := v.Policy().ListRules()
	fmt.Println("lane\tpattern\tverdict\talways\tsource")
	for _, r := range rows {
		fmt.Printf("%s\t%s\t%s\t%t\t%s\n", r.Lane, r.Pattern, r.Verdict, r.Always, r.Source)
	}
	return 0
}

func runPolicyRm(v *custos.Vault, stateDir string, cfg *custos.Config, subArgs []string) int {
	if len(subArgs) < 2 {
		fmt.Fprintf(os.Stderr, "usage: custos policy rm [<lane>] <pattern>\n")
		return 2
	}
	lane := ""
	pat := subArgs[1]
	if len(subArgs) >= 3 {
		lane = subArgs[1]
		pat = subArgs[2]
	}

	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		arg := pat
		if lane != "" {
			arg = lane + "\x00" + pat
		}
		_, dialErr := client.RoundTripWithArg("POLICY-RM", arg)
		if dialErr == nil {
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	_ = v.Policy().LoadStrict()
	removed, rmErr := v.Policy().RemoveRule(lane, pat, "cli")
	if rmErr != nil {
		fmt.Fprintln(os.Stderr, rmErr.Error())
		return 1
	}
	if !removed {
		fmt.Fprintln(os.Stderr, "rule not found")
		return 1
	}
	return 0
}

func runPolicyReset(v *custos.Vault, stateDir string, cfg *custos.Config) int {
	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTrip("POLICY-RESET")
		if dialErr == nil {
			fmt.Println(resp)
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	_ = v.Policy().LoadStrict()
	if err := v.Policy().Reset("cli"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Println("always-rules cleared")
	return 0
}

func runPolicy(v *custos.Vault, stateDir string, cfg *custos.Config, subArgs []string) int {
	if len(subArgs) == 0 {
		fmt.Fprintf(os.Stderr, "usage: custos policy add|list|rm|reset [options]\n")
		return 2
	}

	verb := subArgs[0]
	switch verb {
	case "add":
		return runPolicyAdd(v, stateDir, cfg, subArgs)
	case "list":
		return runPolicyList(v, stateDir, cfg)
	case "rm":
		return runPolicyRm(v, stateDir, cfg, subArgs)
	case "reset":
		return runPolicyReset(v, stateDir, cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown policy subcommand %q\n", verb)
		return 2
	}
}

func runEgress(v *custos.Vault, stateDir string, cfg *custos.Config, subArgs []string) int {
	if len(subArgs) < 2 || subArgs[0] != "strict" {
		fmt.Fprintf(os.Stderr, "usage: custos egress strict on|off\n")
		return 2
	}
	val := subArgs[1]
	if val != "on" && val != "off" {
		fmt.Fprintf(os.Stderr, "usage: custos egress strict on|off\n")
		return 2
	}

	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		resp, dialErr := client.RoundTripWithArg("EGRESS-STRICT", val)
		if dialErr == nil {
			fmt.Println(resp)
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	_ = v.Policy().LoadStrict()
	if err := v.Policy().SetEgressStrict(val == "on", "cli"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Printf("egress strict: %s\n", val)
	return 0
}

func runRevokeCredential(v *custos.Vault, stateDir, keyfilePath string, cfg *custos.Config, subArgs []string) int {
	if len(subArgs) == 0 {
		fmt.Fprintf(os.Stderr, "usage: custos revoke <name>\n")
		return 2
	}
	credName := subArgs[0]

	client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
	if err == nil {
		_, dialErr := client.RoundTripWithArg("REVOKE-CREDENTIAL", credName)
		if dialErr == nil {
			return 0
		}
		if !errors.Is(dialErr, custos.ErrCtlNotListening) {
			fmt.Fprintln(os.Stderr, dialErr.Error())
			return 1
		}
	}

	pass := getPassphrase(keyfilePath)
	if pass == "" {
		fmt.Fprintln(os.Stderr, custos.ErrCustosLocked.Error())
		return 1
	}
	if !v.IsUnlocked() {
		if err := v.Unlock(pass, keyfilePath != ""); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
	}
	if err := v.RevokeCredential(pass, credName, "cli"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
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
	case "surrogate":
		exitCode = runSurrogate(v, stateDir, keyfilePath, cfg, subArgs)
	case "policy":
		exitCode = runPolicy(v, stateDir, cfg, subArgs)
	case "egress":
		exitCode = runEgress(v, stateDir, cfg, subArgs)
	case "revoke":
		exitCode = runRevokeCredential(v, stateDir, keyfilePath, cfg, subArgs)
	case "login":
		exitCode = runLogin(v, keyfilePath, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown verb %q\n", verb)
		exitCode = 2
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
