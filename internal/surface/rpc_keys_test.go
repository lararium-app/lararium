package surface

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/keystore"
)

type rpcKeysFixture struct {
	srv      *Server
	store    *keystore.Store
	reg      *keystore.Registry
	dir      string
	reloaded int
	reloadFn func() error
	client   *testWSClient
}

func newRPCKeysFixture(t *testing.T, provs []keystore.ProviderInfo) *rpcKeysFixture {
	t.Helper()
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	dir := t.TempDir()
	fx := &rpcKeysFixture{
		srv:   srv,
		store: keystore.New(dir),
		reg:   keystore.NewRegistry(nil),
		dir:   dir,
	}
	srv.AttachKeys(&KeysDeps{
		Store:     fx.store,
		Registry:  fx.reg,
		Providers: func() []keystore.ProviderInfo { return provs },
		Reload: func() error {
			if fx.reloadFn != nil {
				return fx.reloadFn()
			}
			m, err := fx.store.Read()
			if err != nil {
				return err
			}
			fx.reg.Swap(m)
			fx.reloaded++
			return nil
		},
	})
	tok, err := tokStore.Create("rpc-keys-test")
	if err != nil {
		t.Fatal(err)
	}
	fx.client = dialAuthenticatedWS(t, ts.URL, tok)
	return fx
}

func TestRPCKeysUnattachedReturnsMethodNotFound(t *testing.T) {
	_, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	tok, err := tokStore.Create("no-keys")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	assertRPCErrorReason(t, client.callRPC(t, 1, "keys.list", map[string]any{}), "method_not_found")
	assertRPCErrorReason(t, client.callRPC(t, 2, "keys.set", map[string]any{"name": "openai", "key": "test-val"}), "method_not_found")
	assertRPCErrorReason(t, client.callRPC(t, 3, "keys.remove", map[string]any{"name": "openai"}), "method_not_found")
}

func TestRPCKeysList(t *testing.T) {
	t.Run("empty list is bare JSON array", func(t *testing.T) {
		fx := newRPCKeysFixture(t, nil)
		resp := fx.client.callRPC(t, 10, "keys.list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		if strings.TrimSpace(string(resp.Result)) != "[]" {
			t.Fatalf("result = %s, want []", string(resp.Result))
		}
	})

	t.Run("union status and sha256_8 without leaking values", func(t *testing.T) {
		t.Setenv("LARARIUM_RPC_TEST_ENV_KEY", "test-env-val")
		provs := []keystore.ProviderInfo{
			{Name: "envprov", EnvVar: "LARARIUM_RPC_TEST_ENV_KEY"},
			{Name: "cfgprov", Literal: "test-cfg-val"},
			{Name: "emptyprov"},
		}
		fx := newRPCKeysFixture(t, provs)
		if err := fx.store.Set("stored", "test-stored-val"); err != nil {
			t.Fatal(err)
		}

		resp := fx.client.callRPC(t, 11, "keys.list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		rawResult := string(resp.Result)
		for _, secret := range []string{"test-env-val", "test-cfg-val", "test-stored-val"} {
			if strings.Contains(rawResult, secret) {
				t.Fatalf("keys.list leaked value %q: %s", secret, rawResult)
			}
		}

		var rows []keyRow
		if err := json.Unmarshal(resp.Result, &rows); err != nil {
			t.Fatalf("unmarshal keyRow slice: %v", err)
		}
		if len(rows) != 4 {
			t.Fatalf("rows len = %d (%v), want 4", len(rows), rows)
		}
		byName := make(map[string]keyRow, len(rows))
		for _, r := range rows {
			byName[r.Name] = r
		}
		if byName["envprov"].Status != "set (env)" {
			t.Errorf("envprov status = %q", byName["envprov"].Status)
		}
		if byName["cfgprov"].Status != "set (config)" {
			t.Errorf("cfgprov status = %q", byName["cfgprov"].Status)
		}
		if byName["stored"].Status != "set (keys.json)" {
			t.Errorf("stored status = %q", byName["stored"].Status)
		}
		if byName["emptyprov"].Status != "missing" || byName["emptyprov"].SHA256 != "" {
			t.Errorf("emptyprov = %+v", byName["emptyprov"])
		}
		sum := sha256.Sum256([]byte("test-stored-val"))
		wantHex := hex.EncodeToString(sum[:4])
		if byName["stored"].SHA256 != wantHex {
			t.Errorf("stored sha256_8 = %q, want %q", byName["stored"].SHA256, wantHex)
		}
	})
}

func TestRPCKeysSetAndGateOrder(t *testing.T) {
	fx := newRPCKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})

	t.Run("happy path saves and reloads", func(t *testing.T) {
		resp := fx.client.callRPC(t, 20, "keys.set", map[string]any{
			"name": "known",
			"key":  "test-kval-1",
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK {
			t.Fatalf("result = %s, want {\"ok\":true}", string(resp.Result))
		}
		if fx.reloaded != 1 {
			t.Errorf("reloaded = %d, want 1", fx.reloaded)
		}
		if got := fx.reg.Key("known"); got != "test-kval-1" {
			t.Errorf("registry[known] = %q, want test-kval-1", got)
		}
	})

	t.Run("unknown provider requires force bool", func(t *testing.T) {
		assertRPCErrorReason(t, fx.client.callRPC(t, 21, "keys.set", map[string]any{
			"name": "novel",
			"key":  "test-kval-2",
		}), "unknown_provider")

		assertRPCErrorReason(t, fx.client.callRPC(t, 22, "keys.set", map[string]any{
			"name":  "novel",
			"key":   "test-kval-2",
			"force": false,
		}), "unknown_provider")

		// Non-bool force rejected as invalid_request.
		assertRPCErrorReason(t, fx.client.callRPC(t, 23, "keys.set", map[string]any{
			"name":  "novel",
			"key":   "test-kval-2",
			"force": "true",
		}), "invalid_request")

		resp := fx.client.callRPC(t, 24, "keys.set", map[string]any{
			"name":  "novel",
			"key":   "test-kval-2",
			"force": true,
		})
		if resp.Error != nil {
			t.Fatalf("force=true error: %+v", resp.Error)
		}
	})

	t.Run("gate order: name before unknown before size before unknown fields", func(t *testing.T) {
		bigKey := strings.Repeat("a", maxKeyBody+1)

		// Gate 1: invalid provider name beats unknown_provider, key_too_large, and unknown field.
		assertRPCErrorReason(t, fx.client.callRPC(t, 30, "keys.set", map[string]any{
			"name":  "BAD_NAME!",
			"key":   bigKey,
			"extra": true,
		}), "invalid_provider_name")

		// Gate 2: unknown provider beats key_too_large and unknown field.
		assertRPCErrorReason(t, fx.client.callRPC(t, 31, "keys.set", map[string]any{
			"name":  "unseen",
			"key":   bigKey,
			"extra": true,
		}), "unknown_provider")

		// Gate 3: key_too_large beats unknown field.
		assertRPCErrorReason(t, fx.client.callRPC(t, 32, "keys.set", map[string]any{
			"name":  "known",
			"key":   bigKey,
			"extra": true,
		}), "key_too_large")

		// Exact 4 KiB cap succeeds.
		exactKey := strings.Repeat("b", maxKeyBody)
		if resp := fx.client.callRPC(t, 33, "keys.set", map[string]any{
			"name": "known",
			"key":  exactKey,
		}); resp.Error != nil {
			t.Fatalf("exact 4KiB key failed: %+v", resp.Error)
		}

		// Gate 4: unknown param rejected (DisallowUnknownFields).
		assertRPCErrorReason(t, fx.client.callRPC(t, 34, "keys.set", map[string]any{
			"name":  "known",
			"key":   "test-kval-ok",
			"extra": 1,
		}), "invalid_request")

		// Non-string key rejected as invalid_request.
		assertRPCErrorReason(t, fx.client.callRPC(t, 35, "keys.set", map[string]any{
			"name": "known",
			"key":  12345,
		}), "invalid_request")

		// Gate 5: empty / whitespace key rejected as empty_key.
		assertRPCErrorReason(t, fx.client.callRPC(t, 36, "keys.set", map[string]any{
			"name": "known",
			"key":  "   ",
		}), "empty_key")
	})

	t.Run("key_store_full at MaxKeys", func(t *testing.T) {
		fullFx := newRPCKeysFixture(t, nil)
		for i := range keystore.MaxKeys {
			if err := fullFx.store.Set(fmt.Sprintf("p%02d", i), "test-fill"); err != nil {
				t.Fatal(err)
			}
		}
		assertRPCErrorReason(t, fullFx.client.callRPC(t, 40, "keys.set", map[string]any{
			"name":  "overflow",
			"key":   "test-new",
			"force": true,
		}), "key_store_full")
	})

	t.Run("key_store_write_failed and key_reload_failed", func(t *testing.T) {
		errFx := newRPCKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})
		// Corrupt keys.json so Store.Set fails during readLocked.
		if err := os.WriteFile(filepath.Join(errFx.dir, "keys.json"), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertRPCErrorReason(t, errFx.client.callRPC(t, 41, "keys.set", map[string]any{
			"name": "known",
			"key":  "test-kval-wfail",
		}), "key_store_write_failed")

		// Restore valid store, fail Reload.
		if err := os.Remove(filepath.Join(errFx.dir, "keys.json")); err != nil {
			t.Fatal(err)
		}
		errFx.reloadFn = func() error { return errors.New("reload boom") }
		assertRPCErrorReason(t, errFx.client.callRPC(t, 42, "keys.set", map[string]any{
			"name": "known",
			"key":  "test-kval-rfail",
		}), "key_reload_failed")
	})
}

func TestRPCKeysSetNeverLeaksKeyValue(t *testing.T) {
	const canary = "test-canary-kval-9182"
	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	fx := newRPCKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})

	checkNoLeak := func(name string, resp *RPCResponse) {
		t.Helper()
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("%s marshal resp: %v", name, err)
		}
		if strings.Contains(string(raw), canary) {
			t.Errorf("%s leaked canary in error frame: %s", name, string(raw))
		}
		if strings.Contains(logBuf.String(), canary) {
			t.Errorf("%s leaked canary in logs: %s", name, logBuf.String())
		}
	}

	checkNoLeak("invalid_provider_name", fx.client.callRPC(t, 50, "keys.set", map[string]any{
		"name": "BAD!",
		"key":  canary,
	}))
	checkNoLeak("unknown_provider", fx.client.callRPC(t, 51, "keys.set", map[string]any{
		"name": "unknownprov",
		"key":  canary,
	}))
	checkNoLeak("key_too_large", fx.client.callRPC(t, 52, "keys.set", map[string]any{
		"name": "known",
		"key":  canary + strings.Repeat("x", maxKeyBody),
	}))
	checkNoLeak("invalid_request", fx.client.callRPC(t, 53, "keys.set", map[string]any{
		"name":  "known",
		"key":   canary,
		"extra": "nope",
	}))

	// Corrupt store for write failure.
	if err := os.WriteFile(filepath.Join(fx.dir, "keys.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkNoLeak("key_store_write_failed", fx.client.callRPC(t, 54, "keys.set", map[string]any{
		"name": "known",
		"key":  canary,
	}))

	// Reload failure.
	if err := os.Remove(filepath.Join(fx.dir, "keys.json")); err != nil {
		t.Fatal(err)
	}
	fx.reloadFn = func() error { return errors.New("reload failed") }
	checkNoLeak("key_reload_failed", fx.client.callRPC(t, 55, "keys.set", map[string]any{
		"name": "known",
		"key":  canary,
	}))
}

func TestRPCKeysRemove(t *testing.T) {
	fx := newRPCKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})
	if err := fx.store.Set("known", "test-kval-rm"); err != nil {
		t.Fatal(err)
	}

	t.Run("happy path removes and reloads", func(t *testing.T) {
		resp := fx.client.callRPC(t, 60, "keys.remove", map[string]any{"name": "known"})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK {
			t.Fatalf("result = %s, want {\"ok\":true}", string(resp.Result))
		}
		if fx.reloaded != 1 {
			t.Errorf("reloaded = %d, want 1", fx.reloaded)
		}
	})

	t.Run("idempotent on absent valid name", func(t *testing.T) {
		resp := fx.client.callRPC(t, 61, "keys.remove", map[string]any{"name": "never-existed"})
		if resp.Error != nil {
			t.Fatalf("unexpected error on absent name: %+v", resp.Error)
		}
	})

	t.Run("invalid name returns invalid_provider_name", func(t *testing.T) {
		assertRPCErrorReason(t, fx.client.callRPC(t, 62, "keys.remove", map[string]any{"name": "../bad"}), "invalid_provider_name")
	})

	t.Run("store and reload errors", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(fx.dir, "keys.json"), []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertRPCErrorReason(t, fx.client.callRPC(t, 63, "keys.remove", map[string]any{"name": "known"}), "key_store_write_failed")

		if err := os.Remove(filepath.Join(fx.dir, "keys.json")); err != nil {
			t.Fatal(err)
		}
		fx.reloadFn = func() error { return errors.New("reload err") }
		assertRPCErrorReason(t, fx.client.callRPC(t, 64, "keys.remove", map[string]any{"name": "known"}), "key_reload_failed")
	})
}
