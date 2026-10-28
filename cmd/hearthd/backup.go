package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

const OperatorStepStaleSocket = "confirm the daemon process is dead, remove hearthd.sock, retry"

var exitFunc = os.Exit

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(val string) error {
	*s = append(*s, val)
	return nil
}

func backupCmd(cfgPath string, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printBackupUsage()
		if len(args) == 0 {
			exitFunc(2)
		}
		exitFunc(0)
	}

	switch args[0] {
	case "create":
		backupCreateCmd(cfgPath, args[1:])
	case "list":
		backupListCmd(cfgPath, args[1:])
	case "verify":
		backupVerifyCmd(cfgPath, args[1:])
	case "extract-config":
		backupExtractConfigCmd(cfgPath, args[1:])
	case "keygen":
		backupKeygenCmd(args[1:])
	default:
		printBackupUsage()
		exitFunc(2)
	}
}

func printBackupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: hearthd backup create [--out <path>] [--no-config]
                             [--to <age1…>] [--passphrase] [--plaintext]
       hearthd backup list <file> [--identity <file>]
       hearthd backup verify <file> [--identity <file>]
       hearthd backup extract-config <file> <path> [--identity <file>]
       hearthd backup keygen [--out <identity-file>]

%s
`, backup.SecretsHonestyLine)
}

func backupCreateCmd(cfgPath string, args []string) {
	fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
	outFlag := fs.String("out", "", "output bundle path")
	noConfigFlag := fs.Bool("no-config", false, "do not bundle lararium.yaml")
	passphraseFlag := fs.Bool("passphrase", false, "encrypt with passphrase")
	plaintextFlag := fs.Bool("plaintext", false, "create unencrypted plaintext bundle")
	var toFlags stringSliceFlag
	fs.Var(&toFlags, "to", "recipient public key (repeatable)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup create [--out <path>] [--no-config] [--to <age1...>] [--passphrase] [--plaintext]\n\n%s\n", backup.SecretsHonestyLine)
	}
	if err := fs.Parse(args); err != nil {
		exitFunc(2)
		return
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}
	cfg.Hearth.Home = custos.ResolveHearthHome("", cfg.Hearth.Home)

	home := cfg.Hearth.Home
	outPath := *outFlag
	if outPath == "" {
		outPath = fmt.Sprintf("./hearth-%s.lararium-backup", time.Now().UTC().Format("20060102T150405Z"))
	}

	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve out path: %v\n", err)
		exitFunc(1)
		return
	}

	if err := backup.CheckSelfInclusion(home, outAbs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	var recipients []age.Recipient
	if *plaintextFlag {
		fmt.Fprintln(os.Stderr, backup.PlaintextBanner)
	} else {
		recipientStrings := append([]string{}, toFlags...)
		recipientStrings = append(recipientStrings, cfg.Backup.EncryptTo...)

		if len(recipientStrings) == 0 && !*passphraseFlag {
			fmt.Fprintln(os.Stderr, "refusing unsealed backup: specify --to <age1...>, --passphrase, configure backup.encrypt_to, or use --plaintext")
			exitFunc(1)
			return
		}

		if len(recipientStrings) > 0 {
			recips, err := backup.ParseRecipients(recipientStrings)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid recipient: %v\n", err)
				exitFunc(1)
				return
			}
			recipients = append(recipients, recips...)
		}

		if *passphraseFlag {
			pass, err := backup.PromptPassphrase("Enter passphrase: ", os.Stdin, os.Stderr)
			if err != nil || pass == "" {
				fmt.Fprintf(os.Stderr, "passphrase prompt failed: %v\n", err)
				exitFunc(1)
				return
			}
			scryptRecip, err := backup.NewScryptRecipient(pass)
			if err != nil {
				fmt.Fprintf(os.Stderr, "scrypt recipient error: %v\n", err)
				exitFunc(1)
				return
			}
			recipients = append(recipients, scryptRecip)
		}
	}

	sockPath := filepath.Join(home, "hearthd.sock")
	if _, err := os.Stat(sockPath); err == nil {
		// Socket file exists: ping to check if daemon is alive (§4.5.3.2)
		if err := surface.PingViaSocket(sockPath, 2*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "hearthd.sock exists but daemon is not responding: %s\n", OperatorStepStaleSocket)
			exitFunc(1)
			return
		}
		// Daemon is live: invoke control-socket verb
		if err := surface.BackupViaSocket(sockPath, outAbs, *noConfigFlag, func(p string) {
			fmt.Fprintln(os.Stderr, p)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "backup failed: %v\n", err)
			exitFunc(1)
			return
		}
		fmt.Println(backup.SecretsHonestyLine)
		return
	}

	// Daemon stopped: run offline backup under flocks (§4.5.3.2)
	if err := backup.CreateOffline(home, cfgPath, outAbs, *noConfigFlag, func(p string) {
		fmt.Fprintln(os.Stderr, p)
	}, recipients...); err != nil {
		fmt.Fprintf(os.Stderr, "backup failed: %v\n", err)
		exitFunc(1)
		return
	}
	fmt.Println(backup.SecretsHonestyLine)
}

func parseReaderArgs(args []string) (file string, extra string, identity string, err error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--identity":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("missing value for --identity")
			}
			i++
			identity = args[i]
		case strings.HasPrefix(arg, "--identity="):
			identity = strings.TrimPrefix(arg, "--identity=")
		case strings.HasPrefix(arg, "-"):
			return "", "", "", fmt.Errorf("unknown flag %s", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) > 0 {
		file = positional[0]
	}
	if len(positional) > 1 {
		extra = positional[1]
	}
	return file, extra, identity, nil
}

func loadReaderIdentities(cfgPath, identityPath string) []age.Identity {
	var identities []age.Identity
	if identityPath != "" {
		ids, err := backup.LoadIdentityFile(identityPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "identity file error: %v\n", err)
			exitFunc(1)
			return nil
		}
		identities = append(identities, ids...)
	} else if cfgPath != "" {
		if cfg, err := LoadConfig(cfgPath); err == nil && cfg.Backup.Identity != "" {
			ids, err := backup.LoadIdentityFile(cfg.Backup.Identity)
			if err != nil {
				fmt.Fprintf(os.Stderr, "config backup.identity error: %v\n", err)
				exitFunc(1)
				return nil
			}
			identities = append(identities, ids...)
		}
	}
	return identities
}

func backupListCmd(cfgPath string, args []string) {
	file, _, idPath, err := parseReaderArgs(args)
	if err != nil || file == "" {
		fmt.Fprintln(os.Stderr, "usage: hearthd backup list <file> [--identity <file>]")
		exitFunc(2)
		return
	}
	ids := loadReaderIdentities(cfgPath, idPath)
	if err := backup.List(file, os.Stdout, ids...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}
}

func backupVerifyCmd(cfgPath string, args []string) {
	file, _, idPath, err := parseReaderArgs(args)
	if err != nil || file == "" {
		fmt.Fprintln(os.Stderr, "usage: hearthd backup verify <file> [--identity <file>]")
		exitFunc(2)
		return
	}
	ids := loadReaderIdentities(cfgPath, idPath)
	findings, err := backup.Verify(file, os.Stdout, os.Stderr, ids...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}
	if len(findings) > 0 {
		exitFunc(1)
		return
	}
}

func backupExtractConfigCmd(cfgPath string, args []string) {
	file, destPath, idPath, err := parseReaderArgs(args)
	if err != nil || file == "" || destPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hearthd backup extract-config <file> <path> [--identity <file>]")
		exitFunc(2)
		return
	}
	ids := loadReaderIdentities(cfgPath, idPath)
	if err := backup.ExtractConfig(file, destPath, ids...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}
}

// backupKeygenCmd implements `hearthd backup keygen [--out <identity-file>]`
// (§4.5.2a, amendment A4b): writes a fresh X25519 identity (mode 0600) and
// prints ONLY the recipient to stdout; guidance goes to stderr.
func backupKeygenCmd(args []string) {
	fs := flag.NewFlagSet("backup keygen", flag.ContinueOnError)
	outFlag := fs.String("out", "lararium-backup-identity.key", "identity file to create (mode 0600)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup keygen [--out <identity-file>]\n\n%s\n", backup.SecretsHonestyLine)
	}
	if err := fs.Parse(args); err != nil {
		exitFunc(2)
		return
	}
	recipient, err := backup.Keygen(*outFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keygen failed: %v\n", err)
		exitFunc(1)
		return
	}
	fmt.Fprintf(os.Stderr, "identity written to %s (mode 0600) — keep it offline; losing it means losing the bundle\n", *outFlag)
	fmt.Println(recipient)
}
