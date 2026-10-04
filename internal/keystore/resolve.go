package keystore

import (
	"fmt"
	"os"
	"strings"
)

// ProviderInfo is the config-level view of one provider's key
// locations (name, env var name, literal). Values come from lararium.yaml.
type ProviderInfo struct {
	Name    string
	EnvVar  string // api_key_env (name of env var, may be "")
	Literal string // api_key     (may be "")
}

// Source names where a resolved key came from, for status display.
type Source string

// Resolution sources, in precedence order.
const (
	SourceEnv     Source = "env"
	SourceKeys    Source = "keys.json"
	SourceConfig  Source = "config"
	SourceMissing Source = "missing"
)

// Resolution is the winning key for one provider plus its source.
type Resolution struct {
	Value  string
	Source Source
}

// ResolveAll resolves every provider against env, the store, and the
// config literal, in that order. A layer counts only when non-empty
// AFTER trimming (a whitespace-only env var is a miss, never a
// "set (env)" lie). Returns per-provider resolutions (every provider
// gets an entry; SourceMissing when nothing resolves) and warning
// lines for stderr — the K2 visibility contract. A corrupt keys.json
// is not fatal here: it degrades to an empty store with a warning.
func ResolveAll(store *Store, provs []ProviderInfo, getenv func(string) string) (map[string]Resolution, []string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var warnings []string

	keys := map[string]string{}
	if store != nil {
		m, err := store.Read()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("keys.json unreadable: %v", err))
		} else {
			keys = m
		}
	}

	out := make(map[string]Resolution, len(provs))
	for _, p := range provs {
		var envVal, keysVal string
		if p.EnvVar != "" {
			envVal = strings.TrimSpace(getenv(p.EnvVar))
		}
		keysVal = strings.TrimSpace(keys[p.Name])
		litVal := strings.TrimSpace(p.Literal)

		// Shadow warning: ONLY when env wins over a DIFFERENT
		// keys.json value — the editor would be editing the shadowed
		// copy. Equal values are not a shadow.
		if envVal != "" && keysVal != "" && envVal != keysVal {
			warnings = append(warnings, fmt.Sprintf("provider %q: keys.json entry shadowed by api_key_env — the web/CLI editor updates the shadowed copy", p.Name))
		}

		switch {
		case envVal != "":
			out[p.Name] = Resolution{Value: envVal, Source: SourceEnv}
		case keysVal != "":
			out[p.Name] = Resolution{Value: keysVal, Source: SourceKeys}
		case litVal != "":
			out[p.Name] = Resolution{Value: litVal, Source: SourceConfig}
			warnings = append(warnings, fmt.Sprintf("provider %q: key in config file — prefer 'hearthd keys set %s'", p.Name, p.Name))
		default:
			out[p.Name] = Resolution{Source: SourceMissing}
		}
	}
	return out, warnings
}
