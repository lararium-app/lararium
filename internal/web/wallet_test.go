package web

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestWallet_Store_RoundTripByteEquality(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "https://carrier.example.com/account"

	stagingDir := filepath.Join(t.TempDir(), "staging-profile")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("mkdir stagingDir: %v", err)
	}

	files := map[string][]byte{
		"cookies.txt":                       []byte("session=abc123xyz; secure; httponly"),
		"nested/dir/level/deep.json":        []byte(`{"auth": true, "token": "xyz789"}`),
		"empty.dat":                         []byte(""),
		"binary.bin":                        {0x00, 0x01, 0xfe, 0xff, 0x5a, 0x6b},
		"Default/Network/Cookies":           []byte("sqlite format 3 \x00 mock database binary"),
		"Default/Local Storage/leveldb/LOG": []byte("leveldb log entries..."),
	}

	for relPath, content := range files {
		fullPath := filepath.Join(stagingDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", relPath, err)
		}
		if err := os.WriteFile(fullPath, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
	}

	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	if !w.Has(origin) {
		t.Fatalf("Has(%q) = false, want true after Store", origin)
	}

	activeDir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Fatalf("release failed: %v", err)
		}
	}()

	// Verify all source files exist in activeDir with identical bytes
	for relPath, expectedData := range files {
		destPath := filepath.Join(activeDir, relPath)
		actualData, err := os.ReadFile(destPath)
		if err != nil {
			t.Fatalf("read restored file %s: %v", relPath, err)
		}
		if !bytes.Equal(actualData, expectedData) {
			t.Fatalf("byte mismatch in %s:\ngot:  %q\nwant: %q", relPath, actualData, expectedData)
		}
	}

	// Verify no unexpected extra files in activeDir
	err = filepath.Walk(activeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(activeDir, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if _, ok := files[relSlash]; !ok {
			t.Fatalf("unexpected file found in restored activeDir: %s", relSlash)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk activeDir: %v", err)
	}
}

func TestWallet_Store_CorruptSealedFile(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "https://carrier.example.com"

	stagingDir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("tok123"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	originKey, err := OriginKey(origin)
	if err != nil {
		t.Fatalf("OriginKey: %v", err)
	}
	lrwsPath := filepath.Join(home, "sessions", originKey+".lrws")
	raw, err := os.ReadFile(lrwsPath)
	if err != nil {
		t.Fatalf("read lrws: %v", err)
	}

	corruptions := []struct {
		name    string
		payload []byte
	}{
		{name: "empty file", payload: []byte{}},
		{name: "truncated header", payload: raw[:4]},
		{name: "corrupt magic", payload: append([]byte("XXXXX"), raw[5:]...)},
		{name: "flipped bit", payload: func() []byte {
			c := make([]byte, len(raw))
			copy(c, raw)
			c[len(c)-1] ^= 0x42
			return c
		}()},
	}

	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(lrwsPath, tc.payload, 0o600); err != nil {
				t.Fatalf("write corrupt payload: %v", err)
			}

			// Ensure Load does not panic and returns a clean error
			var loadErr error
			didPanic := func() (panicked bool) {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
					}
				}()
				_, _, loadErr = w.Load(origin)
				return false
			}()

			if didPanic {
				t.Fatalf("Load panicked on %s", tc.name)
			}
			if loadErr == nil {
				t.Fatalf("Load expected error on %s, got nil", tc.name)
			}

			// Active dir must not be left behind
			activeDir := filepath.Join(home, "sessions", ".active", originKey)
			if _, err := os.Stat(activeDir); !os.IsNotExist(err) {
				t.Fatalf("active dir was left behind after error: %v", err)
			}
		})
	}
}

func TestWallet_Store_Overwrite(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "utility.example.com"

	staging1 := filepath.Join(t.TempDir(), "s1")
	_ = os.MkdirAll(staging1, 0o700)
	_ = os.WriteFile(filepath.Join(staging1, "data.txt"), []byte("v1-content"), 0o600)

	if err := w.Store(origin, staging1); err != nil {
		t.Fatalf("Store v1: %v", err)
	}

	dir1, release1, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load v1: %v", err)
	}
	content1, _ := os.ReadFile(filepath.Join(dir1, "data.txt"))
	if string(content1) != "v1-content" {
		t.Fatalf("got %q, want v1-content", content1)
	}
	if err := release1(); err != nil {
		t.Fatalf("release1: %v", err)
	}

	// Overwrite with v2
	staging2 := filepath.Join(t.TempDir(), "s2")
	_ = os.MkdirAll(staging2, 0o700)
	_ = os.WriteFile(filepath.Join(staging2, "data.txt"), []byte("v2-updated-content"), 0o600)
	_ = os.WriteFile(filepath.Join(staging2, "newfile.txt"), []byte("brand-new"), 0o600)

	if err := w.Store(origin, staging2); err != nil {
		t.Fatalf("Store v2: %v", err)
	}

	dir2, release2, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load v2: %v", err)
	}
	defer release2()

	content2, _ := os.ReadFile(filepath.Join(dir2, "data.txt"))
	if string(content2) != "v2-updated-content" {
		t.Fatalf("got %q, want v2-updated-content", content2)
	}
	newfile, _ := os.ReadFile(filepath.Join(dir2, "newfile.txt"))
	if string(newfile) != "brand-new" {
		t.Fatalf("got %q, want brand-new", newfile)
	}
}

func TestWallet_Store_DeleteInterleave(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "ups.com"

	for i := range 4 {
		staging := filepath.Join(t.TempDir(), fmt.Sprintf("stg-%d", i))
		_ = os.MkdirAll(staging, 0o700)
		_ = os.WriteFile(filepath.Join(staging, "run.txt"), []byte(fmt.Sprintf("run-%d", i)), 0o600)

		if err := w.Store(origin, staging); err != nil {
			t.Fatalf("iteration %d Store failed: %v", i, err)
		}
		if !w.Has(origin) {
			t.Fatalf("iteration %d Has = false after Store", i)
		}

		if err := w.Delete(origin); err != nil {
			t.Fatalf("iteration %d Delete failed: %v", i, err)
		}
		if w.Has(origin) {
			t.Fatalf("iteration %d Has = true after Delete", i)
		}
	}

	// Final store and load to verify cleanly usable after delete
	stagingFinal := filepath.Join(t.TempDir(), "stg-final")
	_ = os.MkdirAll(stagingFinal, 0o700)
	_ = os.WriteFile(filepath.Join(stagingFinal, "final.txt"), []byte("final-state"), 0o600)
	if err := w.Store(origin, stagingFinal); err != nil {
		t.Fatalf("final Store failed: %v", err)
	}
	if !w.Has(origin) {
		t.Fatal("final Has = false")
	}
	dir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("final Load failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "final.txt"))
	if string(data) != "final-state" {
		t.Fatalf("final data mismatch: got %q", data)
	}
	if err := release(); err != nil {
		t.Fatalf("final release failed: %v", err)
	}
}

func TestWallet_Store_ConcurrentDifferentOrigins(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)

	const numWorkers = 12
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := range numWorkers {
		idx := i
		origin := fmt.Sprintf("https://worker-%d.example.com", idx)
		go func() {
			defer wg.Done()

			stagingDir := filepath.Join(t.TempDir(), fmt.Sprintf("staging-%d", idx))
			_ = os.MkdirAll(stagingDir, 0o700)
			token := fmt.Sprintf("secret-token-%d", idx)
			_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte(token), 0o600)

			if err := w.Store(origin, stagingDir); err != nil {
				t.Errorf("worker %d Store failed: %v", idx, err)
				return
			}

			// Parallel Has calls
			for range 5 {
				if !w.Has(origin) {
					t.Errorf("worker %d Has(%q) = false", idx, origin)
				}
			}

			// Round-trip verification
			activeDir, release, err := w.Load(origin)
			if err != nil {
				t.Errorf("worker %d Load failed: %v", idx, err)
				return
			}
			defer release()

			gotToken, err := os.ReadFile(filepath.Join(activeDir, "token.txt"))
			if err != nil {
				t.Errorf("worker %d read token: %v", idx, err)
				return
			}
			if string(gotToken) != token {
				t.Errorf("worker %d token mismatch: got %q, want %q", idx, gotToken, token)
			}
		}()
	}

	wg.Wait()
}
