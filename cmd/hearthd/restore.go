package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/lararium-app/lararium/internal/backup"
)

type restoreOptions struct {
	file         string
	toDir        string
	replaceDir   string
	identityPath string
	yes          bool
	noSafety     bool
}

func parseRestoreArgs(args []string) (*restoreOptions, int, error) {
	opts := &restoreOptions{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--to":
			if i+1 >= len(args) {
				return nil, 2, errors.New("missing value for --to")
			}
			i++
			opts.toDir = args[i]
		case strings.HasPrefix(arg, "--to="):
			opts.toDir = strings.TrimPrefix(arg, "--to=")
		case arg == "--replace":
			if i+1 >= len(args) {
				return nil, 2, errors.New("missing value for --replace")
			}
			i++
			opts.replaceDir = args[i]
		case strings.HasPrefix(arg, "--replace="):
			opts.replaceDir = strings.TrimPrefix(arg, "--replace=")
		case arg == "--identity":
			if i+1 >= len(args) {
				return nil, 2, errors.New("missing value for --identity")
			}
			i++
			opts.identityPath = args[i]
		case strings.HasPrefix(arg, "--identity="):
			opts.identityPath = strings.TrimPrefix(arg, "--identity=")
		case arg == "--yes":
			opts.yes = true
		case arg == "--no-safety":
			opts.noSafety = true
		case arg == "-h" || arg == "--help":
			return nil, 0, nil
		case strings.HasPrefix(arg, "-"):
			return nil, 2, fmt.Errorf("unknown flag %s", arg)
		default:
			if opts.file != "" {
				return nil, 2, fmt.Errorf("unexpected positional argument %s", arg)
			}
			opts.file = arg
		}
	}

	if opts.file == "" {
		return nil, 2, errors.New("missing backup file argument")
	}
	if opts.toDir == "" && opts.replaceDir == "" {
		return nil, 2, errors.New("must specify either --to <dir> or --replace <dir>")
	}
	if opts.toDir != "" && opts.replaceDir != "" {
		return nil, 2, errors.New("cannot specify both --to and --replace")
	}
	return opts, 0, nil
}

func restoreCmd(cfgPath string, args []string) {
	if len(args) == 0 {
		printRestoreUsage()
		exitFunc(2)
		return
	}

	opts, exitCode, err := parseRestoreArgs(args)
	if err != nil || opts == nil {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		printRestoreUsage()
		exitFunc(exitCode)
		return
	}

	// Safety-bundle sealing law (§4.5.2a): the §4.5.4 rule 3 safety bundle
	// carries the config backup.encrypt_to recipients, so --replace never
	// writes a live root's secrets out unsealed on a sealed-policy hearth.
	// No config file at all = no sealing policy (pre-A4b law, and fresh
	// roots restore configless); a malformed config is a hard error.
	var recipients []age.Recipient
	cfg, cerr := LoadConfig(cfgPath)
	switch {
	case cerr == nil && len(cfg.Backup.EncryptTo) > 0:
		recs, perr := backup.ParseRecipients(cfg.Backup.EncryptTo)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "invalid recipient in backup.encrypt_to: %v\n", perr)
			exitFunc(1)
			return
		}
		recipients = recs
	case cerr != nil && !errors.Is(cerr, os.ErrNotExist):
		fmt.Fprintf(os.Stderr, "load config: %v\n", cerr)
		exitFunc(1)
		return
	}

	var ids []age.Identity
	if opts.identityPath != "" {
		loaded, err := backup.LoadIdentityFile(opts.identityPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "identity file error: %v\n", err)
			exitFunc(1)
			return
		}
		ids = append(ids, loaded...)
	}

	if opts.toDir != "" {
		if opts.yes || opts.noSafety {
			fmt.Fprintln(os.Stderr, "--yes and --no-safety flags are only valid with --replace")
			printRestoreUsage()
			exitFunc(2)
			return
		}
		if err := backup.RecoverSwap(opts.toDir); err != nil {
			fmt.Fprintf(os.Stderr, "swap recovery failed: %v\n", err)
			exitFunc(1)
			return
		}
		if err := backup.RestoreTo(opts.file, opts.toDir, ids...); err != nil {
			fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
			exitFunc(1)
			return
		}
		// Fresh-root (--to) success stdout must contain the exact hearthd backup extract-config <file> <path> command text (BK16)
		configPath := filepath.Join(opts.toDir, "lararium.yaml")
		fmt.Printf("hearthd backup extract-config %s %s\n", opts.file, configPath)
		return
	}

	// replaceDir != ""
	if !opts.yes {
		fmt.Fprintln(os.Stderr, "usage refusal: --replace requires --yes")
		exitFunc(2)
		return
	}

	if err := backup.RecoverSwap(opts.replaceDir); err != nil {
		fmt.Fprintf(os.Stderr, "swap recovery failed: %v\n", err)
		exitFunc(1)
		return
	}

	if err := backup.RestoreReplace(opts.file, opts.replaceDir, opts.yes, opts.noSafety, recipients, ids...); err != nil {
		if !strings.Contains(err.Error(), "safety bundle failed") {
			fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
		}
		exitFunc(1)
		return
	}
}

func printRestoreUsage() {
	fmt.Fprintf(os.Stderr, "Usage: hearthd restore <file> --to <dir> [--identity <file>]\n       hearthd restore <file> --replace <dir> --yes [--no-safety] [--identity <file>]\n")
}
