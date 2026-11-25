package devices

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnrollStoreMintRedeemLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custos")
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	store, err := NewEnrollStore(dir, clk)
	if err != nil {
		t.Fatalf("NewEnrollStore: %v", err)
	}

	// Mint code and verify grammar + on-disk security properties.
	code, err := store.MintCode(now, DefaultEnrollTTL)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}
	re := regexp.MustCompile(`^dev1_[0-9A-Za-z]{20}$`)
	if !re.MatchString(code) {
		t.Fatalf("code %q does not match %s", code, re.String())
	}

	// Verify 0600 file mode on enroll.json and enroll.lock, 0700 on dir, and no plaintext at rest.
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %04o, want 0700", perm)
	}
	fileInfo, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat enroll.json: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("enroll.json mode = %04o, want 0600", perm)
	}
	lockInfo, err := os.Stat(filepath.Join(dir, "enroll.lock"))
	if err != nil {
		t.Fatalf("stat enroll.lock: %v", err)
	}
	if perm := lockInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("enroll.lock mode = %04o, want 0600", perm)
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read enroll.json: %v", err)
	}
	if strings.Contains(string(raw), code) || strings.Contains(string(raw), "dev1_") {
		t.Fatalf("enroll.json leaked plaintext code: %s", raw)
	}

	// Pending reports one expiry matching now + 24h.
	pending, err := store.Pending(now)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Unix() != now.Add(DefaultEnrollTTL).Unix() {
		t.Fatalf("Pending = %v, want 1 entry at %v", pending, now.Add(DefaultEnrollTTL))
	}

	// First redeem succeeds.
	ok, err := store.Redeem(code, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("first Redeem = (%v, %v), want (true, nil)", ok, err)
	}

	// Second redeem of same code fails (one-time consume).
	ok, err = store.Redeem(code, now.Add(2*time.Minute))
	if err != nil || ok {
		t.Fatalf("second Redeem = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestEnrollStoreExpiredFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custos")
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	store, err := NewEnrollStore(dir, newMockClock(now))
	if err != nil {
		t.Fatalf("NewEnrollStore: %v", err)
	}

	code, err := store.MintCode(now, DefaultEnrollTTL)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	// At exact expiry boundary -> fails.
	ok, err := store.Redeem(code, now.Add(DefaultEnrollTTL))
	if err != nil || ok {
		t.Fatalf("Redeem at expiry = (%v, %v), want (false, nil)", ok, err)
	}

	// Past expiry -> fails.
	ok, err = store.Redeem(code, now.Add(DefaultEnrollTTL+time.Second))
	if err != nil || ok {
		t.Fatalf("Redeem after expiry = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestEnrollStoreRevokeAllCount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custos")
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	store, err := NewEnrollStore(dir, newMockClock(now))
	if err != nil {
		t.Fatalf("NewEnrollStore: %v", err)
	}

	code1, err := store.MintCode(now, DefaultEnrollTTL)
	if err != nil {
		t.Fatalf("MintCode 1: %v", err)
	}
	if _, err := store.MintCode(now, DefaultEnrollTTL); err != nil {
		t.Fatalf("MintCode 2: %v", err)
	}
	// Also mint one already-expired code; RevokeAll counts live codes while clearing all.
	if _, err := store.MintCode(now.Add(-48*time.Hour), DefaultEnrollTTL); err != nil {
		t.Fatalf("MintCode expired: %v", err)
	}

	revoked, err := store.RevokeAll(now)
	if err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if revoked != 2 {
		t.Fatalf("RevokeAll count = %d, want 2", revoked)
	}

	ok, err := store.Redeem(code1, now.Add(time.Minute))
	if err != nil || ok {
		t.Fatalf("Redeem after RevokeAll = (%v, %v), want (false, nil)", ok, err)
	}

	pending, err := store.Pending(now)
	if err != nil {
		t.Fatalf("Pending after RevokeAll: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("Pending after RevokeAll = %d, want 0", len(pending))
	}
}

func TestEnrollStoreConcurrentRedeemSingleWinner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custos")
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	store, err := NewEnrollStore(dir, newMockClock(now))
	if err != nil {
		t.Fatalf("NewEnrollStore: %v", err)
	}

	code, err := store.MintCode(now, DefaultEnrollTTL)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = store.Redeem(code, now.Add(time.Minute))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d error: %v", i, e)
		}
	}
	winners := 0
	for _, ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent Redeem winners = %d, want exactly 1 (results=%v)", winners, results)
	}
}

func TestEnrollStoreCorruptJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custos")
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	store, err := NewEnrollStore(dir, newMockClock(now))
	if err != nil {
		t.Fatalf("NewEnrollStore: %v", err)
	}
	if err := os.WriteFile(store.Path(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := store.Redeem("dev1_12345678901234567890", now); err == nil {
		t.Fatal("Redeem on corrupt enroll.json returned nil error, want error")
	}
	if _, err := store.Pending(now); err == nil {
		t.Fatal("Pending on corrupt enroll.json returned nil error, want error")
	}
}
