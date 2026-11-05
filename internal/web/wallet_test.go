package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWallet_RoundTrip(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "https://example.com/test"

	activeDir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cookiePath := filepath.Join(activeDir, "cookies.db")
	cookieData := []byte("session_token=abc123xyz; Secure; HttpOnly")
	if err := os.WriteFile(cookiePath, cookieData, 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}

	subDir := filepath.Join(activeDir, "Default", "Storage")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatalf("mkdir subDir: %v", err)
	}
	subFile := filepath.Join(subDir, "state.txt")
	subData := []byte("profile_state=active")
	if err := os.WriteFile(subFile, subData, 0o600); err != nil {
		t.Fatalf("write subFile: %v", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	// Active dir should be removed
	if _, err := os.Stat(activeDir); !os.IsNotExist(err) {
		t.Fatalf("expected active dir to be removed, got err: %v", err)
	}

	if !w.Has(origin) {
		t.Fatalf("wallet should report having entry for %s", origin)
	}

	// Second load: roundtrip check
	activeDir2, release2, err := w.Load(origin)
	if err != nil {
		t.Fatalf("second Load failed: %v", err)
	}
	defer release2()

	readCookie, err := os.ReadFile(filepath.Join(activeDir2, "cookies.db"))
	if err != nil {
		t.Fatalf("read restored cookie: %v", err)
	}
	if string(readCookie) != string(cookieData) {
		t.Fatalf("cookie mismatch: got %q, want %q", readCookie, cookieData)
	}

	readSub, err := os.ReadFile(filepath.Join(activeDir2, "Default", "Storage", "state.txt"))
	if err != nil {
		t.Fatalf("read restored sub file: %v", err)
	}
	if string(readSub) != string(subData) {
		t.Fatalf("subfile mismatch: got %q, want %q", readSub, subData)
	}
}

func TestWallet_TamperDetected(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "example.com"

	dir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "data.txt"), []byte("payload"), 0o600)
	if err := release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	originKey, _ := OriginKey(origin)
	lrwsPath := filepath.Join(home, "sessions", originKey+".lrws")
	raw, err := os.ReadFile(lrwsPath)
	if err != nil {
		t.Fatalf("read lrws: %v", err)
	}

	// 1. Bit flip in ciphertext
	tampered := make([]byte, len(raw))
	copy(tampered, raw)
	tampered[len(tampered)-1] ^= 0x01
	if err := os.WriteFile(lrwsPath, tampered, 0o600); err != nil { //nolint:gosec // G703: test path derived from t.TempDir
		t.Fatalf("write tampered: %v", err)
	}

	_, _, err = w.Load(origin)
	if err == nil {
		t.Fatal("expected Load to fail on tampered ciphertext, got nil")
	}
	if !strings.Contains(err.Error(), "tampered or truncated") {
		t.Fatalf("expected clear tamper error, got: %v", err)
	}

	// Active dir must not be left behind
	activeDir := filepath.Join(home, "sessions", ".active", originKey)
	if _, err := os.Stat(activeDir); !os.IsNotExist(err) {
		t.Fatalf("active dir should be cleaned up on tamper error, got err: %v", err)
	}

	// 2. Truncated file
	if err := os.WriteFile(lrwsPath, raw[:10], 0o600); err != nil { //nolint:gosec // G703: test path derived from t.TempDir
		t.Fatalf("write truncated: %v", err)
	}
	_, _, err = w.Load(origin)
	if err == nil {
		t.Fatal("expected Load to fail on truncated file, got nil")
	}
	if !strings.Contains(err.Error(), "tampered or truncated") {
		t.Fatalf("expected clear truncation error, got: %v", err)
	}

	// 3. Corrupt magic
	corruptMagic := make([]byte, len(raw))
	copy(corruptMagic, raw)
	copy(corruptMagic[:5], "BADMG")
	if err := os.WriteFile(lrwsPath, corruptMagic, 0o600); err != nil { //nolint:gosec // G703: test path derived from t.TempDir
		t.Fatalf("write corrupt magic: %v", err)
	}
	_, _, err = w.Load(origin)
	if err == nil {
		t.Fatal("expected Load to fail on bad magic, got nil")
	}
	if !strings.Contains(err.Error(), "tampered or truncated") {
		t.Fatalf("expected clear magic error, got: %v", err)
	}
}

func TestWallet_ActiveLockRejectsSecondLoad(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "ups.com"

	_, release1, err := w.Load(origin)
	if err != nil {
		t.Fatalf("first Load failed: %v", err)
	}

	// Second concurrent load for the same origin must be rejected
	_, _, err2 := w.Load(origin)
	if err2 == nil {
		t.Fatal("expected second Load to be rejected while active, got nil")
	}
	if !strings.Contains(err2.Error(), "already active") {
		t.Fatalf("expected 'already active' error, got: %v", err2)
	}

	// Release first load
	if err := release1(); err != nil {
		t.Fatalf("release1 failed: %v", err)
	}

	// Now third load should succeed
	_, release3, err3 := w.Load(origin)
	if err3 != nil {
		t.Fatalf("subsequent Load after release failed: %v", err3)
	}
	if err := release3(); err != nil {
		t.Fatalf("release3 failed: %v", err)
	}
}

func TestWallet_OriginKeySanitizing(t *testing.T) {
	tests := []struct {
		input       string
		wantPrefix  string
		wantErr     bool
		validKey    bool
		mustContain string
	}{
		{input: "localhost", wantPrefix: "localhost", validKey: true},
		{input: "127.0.0.1", wantPrefix: "127.0.0.1", validKey: true},
		{input: "HTTPS://EXAMPLE.COM:8080/path", wantPrefix: "example.com", validKey: true},
		{input: "münchen.de", wantPrefix: "xn--mnchen-3ya.de", validKey: true},
		{input: "test_domain.com", mustContain: "_5f", validKey: true},
		{input: "[::1]", mustContain: "_3a", validKey: true},
		{input: "", wantErr: true},
		{input: "..", wantErr: true},
	}

	seen := make(map[string]string)
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			k, err := OriginKey(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("OriginKey(%q) expected error, got %q", tc.input, k)
				}
				return
			}
			if err != nil {
				t.Fatalf("OriginKey(%q) unexpected error: %v", tc.input, err)
			}
			if tc.wantPrefix != "" && k != tc.wantPrefix {
				t.Errorf("OriginKey(%q) = %q, want %q", tc.input, k, tc.wantPrefix)
			}
			if tc.mustContain != "" && !strings.Contains(k, tc.mustContain) {
				t.Errorf("OriginKey(%q) = %q, want it to contain %q", tc.input, k, tc.mustContain)
			}

			// Ensure collision-safe uniqueness
			if prev, exists := seen[k]; exists && prev != tc.input {
				t.Fatalf("collision detected: %q and %q both mapped to %q", prev, tc.input, k)
			}
			seen[k] = tc.input
		})
	}

	// Specific test: "my_site.com" vs "my_5fsite.com" must not collide
	k1, err1 := OriginKey("my_site.com")
	k2, err2 := OriginKey("my_5fsite.com")
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected error: %v, %v", err1, err2)
	}
	if k1 == k2 {
		t.Fatalf("collision between 'my_site.com' and 'my_5fsite.com': both %q", k1)
	}
}
