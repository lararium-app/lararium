package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
)

// `hearthd pair create|revoke|status|unpair` (NUNTIUS-SPEC §3). The CLI talks
// directly to <hearth.home>/nuntius/owners.json; under the freshness
// doctrine (§3) the running daemon re-reads the file on every poll
// iteration, so no IPC, SIGHUP, or daemon restart is required.

const pairUsage = "usage: hearthd pair create|revoke --all|status|unpair <user_id>"

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
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: hearthd pair status")
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
	case "status":
		pairStatus(store)
	case "unpair":
		pairUnpair(store, args[1])
	}
}

// pairCreate mints a one-time pairing code valid for pair_code_ttl (§3, §4).
// If the bot is already paired, it refuses and exits 1 without minting (§3).
// When unpaired, the plaintext code is printed once to stdout alone; a
// one-line human hint is printed to stderr.
func pairCreate(cfg *Config, store *nuntius.PairStore) {
	o, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(o.OwnerIDs) > 0 {
		fmt.Fprintln(os.Stderr, "hearthd: already paired — run 'hearthd pair unpair <user_id>' first (owner migration: unpair, create, re-pair)")
		os.Exit(1)
	}

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
	fmt.Fprintf(os.Stderr, "send '/pair <code>' to the bot before it expires (%s from now)\n", cfg.Nuntius.PairCodeTTL)
}

// pairRevokeAll clears all pending pairing codes (§3).
func pairRevokeAll(store *nuntius.PairStore) {
	if err := store.RevokeAll(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("pending codes cleared")
}

// pairStatus reports owners and pending-code expiry without a daemon (§3).
// Missing owners.json reports unpaired state with exit code 0.
// No code material or hash is ever printed.
func pairStatus(store *nuntius.PairStore) {
	o, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if len(o.OwnerIDs) == 0 {
		fmt.Println("owners: none")
	} else {
		fmt.Printf("owners: %d\n", len(o.OwnerIDs))
		for _, id := range o.OwnerIDs {
			fmt.Printf("  %s\n", id)
		}
	}

	if len(o.Codes) == 0 {
		fmt.Println("pending codes: none")
	} else {
		codes := make([]nuntius.OwnerCode, len(o.Codes))
		copy(codes, o.Codes)
		sort.SliceStable(codes, func(i, j int) bool {
			return codes[i].Created < codes[j].Created
		})
		now := time.Now()
		for _, c := range codes {
			rem := time.Until(time.Unix(c.Expires, 0)).Round(time.Second)
			if rem <= 0 || now.Unix() >= c.Expires {
				fmt.Println("  expired")
			} else {
				fmt.Printf("  expires in %s\n", rem)
			}
		}
	}
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
