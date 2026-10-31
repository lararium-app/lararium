package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

const credAdapterValue = "fx-DISTINCTIVE-adapter-4e8d1b7a90c2"

// TestCredentialAdapter_RoundTrip drives the real vault through the adapter:
// create (write-only), list, revoke, then ResolveHandle denies; no state file
// or audit line contains the secret (CREDHANDLE-SPEC V-T1b, R4).
func TestCredentialAdapter_RoundTrip(t *testing.T) {
	custos.SetTestAgeWorkFactor(10)
	dir := shortTmp(t)
	stateDir := filepath.Join(dir, "custos")
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init("test-passphrase"); err != nil {
		t.Fatal(err)
	}
	a := &credentialSourceAdapter{vault: v}

	// Locked (Init leaves the vault locked until Unlock).
	if _, err := a.List(); !errors.Is(err, surface.ErrLocked) {
		t.Fatalf("locked list: %v", err)
	}
	if err := v.Unlock("test-passphrase", false); err != nil {
		t.Fatal(err)
	}
	info, err := a.Create(surface.CredentialCreate{
		Name: "svc", Secret: credAdapterValue, TTLSeconds: 3600,
		Rate: &surface.CredentialRate{N: 2, Window: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Last4 != "90c2" || info.Kind != "api_key" || info.Expires == nil || info.Rate == nil {
		t.Fatalf("info %+v", info)
	}
	rows, err := a.List()
	if err != nil || len(rows) != 1 || rows[0].Handle != info.Handle || rows[0].Last4 != "90c2" {
		t.Fatalf("list %+v %v", rows, err)
	}
	var fe *surface.CredentialFieldError
	if _, err := a.Create(surface.CredentialCreate{Name: "svc2", Secret: credAdapterValue, Scope: "other"}); !errors.As(err, &fe) || fe.Field != "scope" {
		t.Fatalf("bad scope: %v", err)
	}
	if _, err := a.Create(surface.CredentialCreate{Name: "svc", Secret: credAdapterValue}); !errors.Is(err, surface.ErrCredentialExists) {
		t.Fatalf("dup: %v", err)
	}
	if err := a.Revoke(info.Handle); err != nil {
		t.Fatal(err)
	}
	if err := a.Revoke(info.Handle); !errors.Is(err, surface.ErrCredentialNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if rows, _ := a.List(); len(rows) != 0 {
		t.Fatalf("list after revoke: %+v", rows)
	}
	if g := v.ResolveHandle(info.Handle, "", "svc", 443); g.Deny == "" || g.Secret != "" {
		t.Fatalf("revoked handle must deny: %+v", g)
	}

	err = filepath.Walk(dir, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil || fi.IsDir() {
			return werr
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // test-owned temp tree
		if rerr == nil && strings.Contains(string(b), credAdapterValue) {
			t.Errorf("secret found in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
