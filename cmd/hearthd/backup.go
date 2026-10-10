package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/surface"
)

const OperatorStepStaleSocket = "confirm the daemon process is dead, remove hearthd.sock, retry"

func backupCmd(cfgPath string, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printBackupUsage()
		if len(args) == 0 {
			os.Exit(2)
		}
		os.Exit(0)
	}

	switch args[0] {
	case "create":
		backupCreateCmd(cfgPath, args[1:])
	case "list":
		backupListCmd(args[1:])
	case "verify":
		backupVerifyCmd(args[1:])
	case "extract-config":
		backupExtractConfigCmd(args[1:])
	default:
		printBackupUsage()
		os.Exit(2)
	}
}

func printBackupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: hearthd backup create [--out <path>] [--no-config]
       hearthd backup list <file>
       hearthd backup verify <file>
       hearthd backup extract-config <file> <path>

%s
`, backup.SecretsHonestyLine)
}

func backupCreateCmd(cfgPath string, args []string) {
	fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
	outFlag := fs.String("out", "", "output bundle path")
	noConfigFlag := fs.Bool("no-config", false, "do not bundle lararium.yaml")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hearthd backup create [--out <path>] [--no-config]\n\n%s\n", backup.SecretsHonestyLine)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	home := cfg.Hearth.Home
	outPath := *outFlag
	if outPath == "" {
		outPath = fmt.Sprintf("./hearth-%s.lararium-backup", time.Now().UTC().Format("20060102T150405Z"))
	}

	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve out path: %v\n", err)
		os.Exit(1)
	}

	if err := backup.CheckSelfInclusion(home, outAbs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	sockPath := filepath.Join(home, "hearthd.sock")
	if _, err := os.Stat(sockPath); err == nil {
		// Socket file exists: ping to check if daemon is alive (§4.5.3.2)
		if err := surface.PingViaSocket(sockPath, 2*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "hearthd.sock exists but daemon is not responding: %s\n", OperatorStepStaleSocket)
			os.Exit(1)
		}
		// Daemon is live: invoke control-socket verb
		if err := surface.BackupViaSocket(sockPath, outAbs, *noConfigFlag, func(p string) {
			fmt.Fprintln(os.Stderr, p)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "backup failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(backup.SecretsHonestyLine)
		return
	}

	// Daemon stopped: run offline backup under flocks (§4.5.3.2)
	if err := backup.CreateOffline(home, cfgPath, outAbs, *noConfigFlag, func(p string) {
		fmt.Fprintln(os.Stderr, p)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "backup failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(backup.SecretsHonestyLine)
}

func backupListCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hearthd backup list <file>")
		os.Exit(2)
	}
	if err := backup.List(args[0], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func backupVerifyCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hearthd backup verify <file>")
		os.Exit(2)
	}
	findings, err := backup.Verify(args[0], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}

func backupExtractConfigCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hearthd backup extract-config <file> <path>")
		os.Exit(2)
	}
	if err := backup.ExtractConfig(args[0], args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
