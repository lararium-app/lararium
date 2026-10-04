package keystore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func seed(t *testing.T, values map[string]string) *Store {
	t.Helper()
	s := New(t.TempDir())
	for k, v := range values {
		// Bypass validation to plant whitespace-only values.
		if err := os.WriteFile(filepath.Join(s.dir, "keys.json"), mustJSON(t, values), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		_ = k
		_ = v
	}
	return s
}

func mustJSON(t *testing.T, m map[string]string) []byte {
	t.Helper()
	// Minimal JSON encoder (values are test fixtures, no exotic chars).
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, `"`+k+`":"`+v+`"`)
	}
	return []byte("{" + strings.Join(pairs, ",") + "}")
}

func TestResolutionPrecedence(t *testing.T) {
	provs := []ProviderInfo{{Name: "p", EnvVar: "P_KEY", Literal: "lit"}}

	cases := []struct {
		name    string
		env     map[string]string
		store   map[string]string
		wantSrc Source
		wantVal string
	}{
		{"env wins", map[string]string{"P_KEY": "envv"}, map[string]string{"p": "storev"}, SourceEnv, "envv"},
		{"store when env unset", nil, map[string]string{"p": "storev"}, SourceKeys, "storev"},
		{"literal when others miss", nil, nil, SourceConfig, "lit"},
		{"whitespace env is a miss", map[string]string{"P_KEY": "   "}, map[string]string{"p": "storev"}, SourceKeys, "storev"},
		{"whitespace store is a miss", nil, map[string]string{"p": " \t"}, SourceConfig, "lit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s *Store
			if c.store != nil {
				s = seed(t, c.store)
			}
			res, _ := ResolveAll(s, provs, fakeEnv(c.env))
			got := res["p"]
			if got.Source != c.wantSrc || got.Value != c.wantVal {
				t.Fatalf("got %+v, want {%s %s}", got, c.wantSrc, c.wantVal)
			}
		})
	}
}

func TestMissingGetsEntry(t *testing.T) {
	res, _ := ResolveAll(nil, []ProviderInfo{{Name: "p"}}, fakeEnv(nil))
	got, ok := res["p"]
	if !ok {
		t.Fatal("every provider must get an entry")
	}
	if got.Source != SourceMissing || got.Value != "" {
		t.Fatalf("got %+v, want missing/empty", got)
	}
}

func TestShadowWarning(t *testing.T) {
	provs := []ProviderInfo{{Name: "p", EnvVar: "P_KEY"}}

	// env shadows a DIFFERENT store value -> warning.
	_, w := ResolveAll(seed(t, map[string]string{"p": "other"}), provs, fakeEnv(map[string]string{"P_KEY": "envv"}))
	if !hasWarning(w, "shadowed by api_key_env") {
		t.Fatalf("different values: want shadow warning, got %v", w)
	}
	// env equals store value -> no warning.
	_, w = ResolveAll(seed(t, map[string]string{"p": "same"}), provs, fakeEnv(map[string]string{"P_KEY": "same"}))
	if hasWarning(w, "shadowed") {
		t.Fatalf("equal values: want no shadow warning, got %v", w)
	}
	// No store entry -> no warning.
	_, w = ResolveAll(nil, provs, fakeEnv(map[string]string{"P_KEY": "envv"}))
	if hasWarning(w, "shadowed") {
		t.Fatalf("no store entry: want no shadow warning, got %v", w)
	}
	// Whitespace-only env must not fire a shadow warning either.
	_, w = ResolveAll(seed(t, map[string]string{"p": "storev"}), provs, fakeEnv(map[string]string{"P_KEY": "  "}))
	if hasWarning(w, "shadowed") {
		t.Fatalf("whitespace env: want no shadow warning, got %v", w)
	}
}

func TestConfigWarningOnlyWhenResolved(t *testing.T) {
	provs := []ProviderInfo{{Name: "p", EnvVar: "P_KEY", Literal: "lit"}}
	_, w := ResolveAll(nil, provs, fakeEnv(map[string]string{"P_KEY": "envv"}))
	if hasWarning(w, "key in config file") {
		t.Fatalf("env resolved: want no config warning, got %v", w)
	}
	_, w = ResolveAll(nil, provs, fakeEnv(nil))
	if !hasWarning(w, "key in config file") {
		t.Fatalf("config resolved: want config warning, got %v", w)
	}
}

func TestCorruptStoreDegrades(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := os.WriteFile(filepath.Join(dir, "keys.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, w := ResolveAll(s, []ProviderInfo{{Name: "p", Literal: "lit"}}, fakeEnv(nil))
	if res["p"].Source != SourceConfig {
		t.Fatalf("corrupt store must degrade to empty, fell to %+v", res["p"])
	}
	if !hasWarning(w, "keys.json unreadable") {
		t.Fatalf("want unreadable warning, got %v", w)
	}
	// Mutations against a corrupt store must surface the error, not
	// silently truncate the file.
	if err := s.Set("p", "v"); err == nil {
		t.Fatal("Set on corrupt store: want error")
	}
}

func hasWarning(ws []string, substr string) bool {
	for _, w := range ws {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
