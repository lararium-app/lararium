package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lararium-app/lararium/internal/backup"
)

func restoreCmd(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printRestoreUsage()
		if len(args) == 0 {
			os.Exit(2)
		}
		os.Exit(0)
	}

	var file string
	var toDir string
	var replaceDir string
	var yes bool
	var noSafety bool

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--to":
			if i+1 >= len(args) {
				printRestoreUsage()
				os.Exit(2)
			}
			i++
			toDir = args[i] //nolint:gosec // G602: bounds checked immediately above
		case strings.HasPrefix(arg, "--to="):
			toDir = strings.TrimPrefix(arg, "--to=")
		case arg == "--replace":
			if i+1 >= len(args) {
				printRestoreUsage()
				os.Exit(2)
			}
			i++
			replaceDir = args[i] //nolint:gosec // G602: bounds checked immediately above
		case strings.HasPrefix(arg, "--replace="):
			replaceDir = strings.TrimPrefix(arg, "--replace=")
		case arg == "--yes":
			yes = true
		case arg == "--no-safety":
			noSafety = true
		case arg == "-h" || arg == "--help":
			printRestoreUsage()
			os.Exit(0)
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(os.Stderr, "unknown flag %s\n", arg)
			printRestoreUsage()
			os.Exit(2)
		default:
			if file != "" {
				fmt.Fprintf(os.Stderr, "unexpected positional argument %s\n", arg)
				printRestoreUsage()
				os.Exit(2)
			}
			file = arg
		}
	}

	if file == "" {
		fmt.Fprintln(os.Stderr, "missing backup file argument")
		printRestoreUsage()
		os.Exit(2)
	}

	if toDir == "" && replaceDir == "" {
		fmt.Fprintln(os.Stderr, "must specify either --to <dir> or --replace <dir>")
		printRestoreUsage()
		os.Exit(2)
	}

	if toDir != "" && replaceDir != "" {
		fmt.Fprintln(os.Stderr, "cannot specify both --to and --replace")
		printRestoreUsage()
		os.Exit(2)
	}

	if toDir != "" {
		if yes || noSafety {
			fmt.Fprintln(os.Stderr, "--yes and --no-safety flags are only valid with --replace")
			printRestoreUsage()
			os.Exit(2)
		}

		if err := backup.RecoverSwap(toDir); err != nil {
			fmt.Fprintf(os.Stderr, "swap recovery failed: %v\n", err)
			os.Exit(1)
		}

		if err := backup.RestoreTo(file, toDir); err != nil {
			fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
			os.Exit(1)
		}

		// Fresh-root (--to) success stdout must contain the exact hearthd backup extract-config <file> <path> command text (BK16)
		configPath := filepath.Join(toDir, "lararium.yaml")
		fmt.Printf("hearthd backup extract-config %s %s\n", file, configPath)
		return
	}

	// replaceDir != ""
	if !yes {
		fmt.Fprintln(os.Stderr, "usage refusal: --replace requires --yes")
		os.Exit(2)
	}

	if err := backup.RecoverSwap(replaceDir); err != nil {
		fmt.Fprintf(os.Stderr, "swap recovery failed: %v\n", err)
		os.Exit(1)
	}

	if err := backup.RestoreReplace(file, replaceDir, yes, noSafety); err != nil {
		if !strings.Contains(err.Error(), "safety bundle failed") {
			fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
		}
		os.Exit(1)
	}
}

func printRestoreUsage() {
	fmt.Fprintf(os.Stderr, "Usage: hearthd restore <file> --to <dir> | --replace <dir> --yes [--no-safety]\n")
}
