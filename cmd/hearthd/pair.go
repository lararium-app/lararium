package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/surface"
)

// `hearthd pair create|revoke|status|unpair` (NUNTIUS-SPEC §3). The CLI talks
// directly to <hearth.home>/nuntius/owners.json; under the freshness
// doctrine (§3) the running daemon re-reads the file on every poll
// iteration, so no IPC, SIGHUP, or daemon restart is required.

const pairUsage = "usage: hearthd pair create|revoke --all|status|unpair <user_id>"

func pairCmd(cfgPath string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, pairUsage)
		os.Exit(2)
	}

	var stdoutFlag bool
	switch args[0] {
	case "create":
		if len(args) == 2 && args[1] == "--stdout" {
			stdoutFlag = true
		} else if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: hearthd pair create [--stdout]")
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
	cfg.Hearth.Home = custos.ResolveHearthHome("", cfg.Hearth.Home)

	store, err := nuntius.NewPairStore(filepath.Join(cfg.Hearth.Home, "nuntius"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		pairCreate(cfg, store, stdoutFlag)
	case "revoke":
		pairRevokeAll(store)
	case "status":
		pairStatus(store)
	case "unpair":
		pairUnpair(cfg, store, args[1])
	}
}

// pairCreate mints a one-time pairing code valid for pair_code_ttl (§3, §4).
// If the bot is already paired, it refuses and exits 1 without minting (§3).
// When unpaired, the plaintext code is printed once to stdout alone; a
// one-line human hint is printed to stderr.
func pairCreate(cfg *Config, store *nuntius.PairStore, stdoutOnly bool) {
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
	if stdoutOnly {
		fmt.Fprintf(os.Stderr, "pairing code written to stdout; treat it as sensitive — terminal output may end up in shell history. Prefer 'hearthd pair redeem --token-stdin' in scripts (expires in %s)\n", cfg.Nuntius.PairCodeTTL)
	} else {
		fmt.Fprintf(os.Stderr, "send '/pair <code>' to the bot before it expires (%s from now)\n", cfg.Nuntius.PairCodeTTL)
	}
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
func pairUnpair(cfg *Config, store *nuntius.PairStore, userID string) {
	if err := store.Unpair(userID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("owner %s removed\n", userID)
	// L-7 (audit 2026-10-23): unpair must not leave API credentials live
	// silently. A token labeled with the unpaired owner ID (the pairing
	// convention `hearthd token create <user_id>`) is revoked outright,
	// and every surviving token is surfaced with its display prefix so
	// the operator sees exactly what still authenticates.
	sweepTokensAfterUnpair(cfg, userID)
}

// sweepTokensAfterUnpair prints the bearer-token inventory left after an
// unpair, revoking the entry whose label matches the unpaired owner ID.
func sweepTokensAfterUnpair(cfg *Config, userID string) {
	ts, err := surface.OpenTokenStore(filepath.Join(cfg.Hearth.Home, "tokens.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearthd: cannot inspect API tokens: %v\n", err)
		return
	}
	for _, l := range []string{userID, "paired:" + userID} {
		revoked, err := ts.RevokeLabel(l)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearthd: cannot revoke token %q: %v\n", l, err)
		} else if revoked > 0 {
			fmt.Printf("revoked API token labeled %q\n", l)
		}
	}
	live, err := ts.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearthd: cannot list API tokens: %v\n", err)
		return
	}
	if len(live) == 0 {
		fmt.Println("no API tokens are live")
		return
	}
	fmt.Println("live API tokens (survive unpair; revoke leftovers with 'hearthd token revoke <label>'):")
	for _, t := range live {
		fmt.Printf("  %-24s %s…  created %s\n", t.Label, t.Prefix, t.Created)
	}
}

// pairStoreAdapter adapts *nuntius.PairStore to surface.PairStore (issue #184).
type pairStoreAdapter struct {
	store *nuntius.PairStore
}

func newPairStoreAdapter(store *nuntius.PairStore) surface.PairStore {
	if store == nil {
		return nil
	}
	return &pairStoreAdapter{store: store}
}

func (a *pairStoreAdapter) Redeem(userID, code string, now time.Time) (surface.RedeemResult, error) {
	res, err := a.store.Redeem(userID, code, now)
	switch res {
	case nuntius.RedeemPaired:
		return surface.RedeemPaired, err
	case nuntius.RedeemInvalid:
		return surface.RedeemInvalid, err
	case nuntius.RedeemAlready:
		return surface.RedeemAlready, err
	default:
		return surface.RedeemInvalid, err
	}
}
