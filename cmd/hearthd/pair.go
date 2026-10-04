package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
)

// `hearthd pair create|revoke|unpair` (NUNTIUS-SPEC §3). The CLI talks
// directly to <hearth.home>/nuntius/owners.json; under the freshness
// doctrine (§3) the running daemon re-reads the file on every poll
// iteration, so no IPC, SIGHUP, or daemon restart is required.

const pairUsage = "usage: hearthd pair create|revoke --all|unpair <user_id>"

func pairCmd(cfgPath string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, pairUsage)
		os.Exit(2)
	}

	switch args[0] {
	case "create":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: hearthd pair create")
			os.Exit(2)
		}
	case "revoke":
		if len(args) != 2 || args[1] != "--all" {
			fmt.Fprintln(os.Stderr, "usage: hearthd pair revoke --all")
			os.Exit(2)
		}
	case "unpair":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: hearthd pair unpair <user_id>")
			os.Exit(2)
		}
	default:
		fmt.Fprintln(os.Stderr, pairUsage)
		os.Exit(2)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	store, err := nuntius.NewPairStore(filepath.Join(cfg.Hearth.Home, "nuntius"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		pairCreate(cfg, store)
	case "revoke":
		pairRevokeAll(store)
	case "unpair":
		pairUnpair(store, args[1])
	}
}

// pairCreate mints a one-time pairing code valid for pair_code_ttl (§3).
// The plaintext code is printed once to stdout so it pipes cleanly;
// the on-disk record stores only the SHA-256 hash.
func pairCreate(cfg *Config, store *nuntius.PairStore) {
	// Normalize fills spec defaults (15m PairCodeTTL) and floors (§4);
	// warnings are ignored here as serve logs them.
	if _, err := cfg.Nuntius.Normalize(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	code, err := store.Mint(time.Now(), cfg.Nuntius.PairCodeTTL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(code)
}

// pairRevokeAll clears all pending pairing codes (§3).
func pairRevokeAll(store *nuntius.PairStore) {
	if err := store.RevokeAll(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("pending codes cleared")
}

// pairUnpair removes an owner from the allowlist (§3).
// PairStore.Unpair returns error only, without distinguishing whether
// the user ID was present; idempotent removal is spec-acceptable (§3).
func pairUnpair(store *nuntius.PairStore, userID string) {
	if err := store.Unpair(userID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("owner %s removed\n", userID)
}
