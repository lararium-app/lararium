package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lararium-app/lararium/internal/keystore"
	"github.com/lararium-app/lararium/internal/router"
)

// keyPlumbing is the KEYS-SPEC K5/K6 stack shared by serve and the
// REPL (audit 5.3): keys.json store, live registry, and one provider
// builder implementing the K2 resolution order — env -> keys.json
// (turn snapshot, else live registry) -> config literal. Keeping ONE
// implementation is the fix: the REPL once read only config literals
// and silently drifted from serve's auth behavior.
type keyPlumbing struct {
	Store     *keystore.Store
	Registry  *keystore.Registry
	providers []ProviderConf
}

func newKeyPlumbing(cfg *Config) (*keyPlumbing, error) {
	kp := &keyPlumbing{Store: keystore.New(cfg.Hearth.Home), providers: cfg.Providers}
	kp.Registry = keystore.NewRegistry(nil)
	if err := kp.Reload(); err != nil {
		return nil, err
	}
	return kp, nil
}

// ProviderInfos lists the config's provider key layers (K2 shape)
// for surface.AttachKeys and reload warnings.
func (kp *keyPlumbing) ProviderInfos() []keystore.ProviderInfo {
	infos := make([]keystore.ProviderInfo, 0, len(kp.providers))
	for _, p := range kp.providers {
		infos = append(infos, keystore.ProviderInfo{
			Name: p.Name, EnvVar: p.APIKeyEnv, Literal: p.APIKey,
		})
	}
	return infos
}

// Reload re-reads keys.json into the registry and prints K2 warnings;
// it is the control socket's reload hook (serve) and each surface's
// startup step.
func (kp *keyPlumbing) Reload() error {
	m, err := kp.Store.Read()
	if err != nil {
		return err
	}
	kp.Registry.Swap(m)
	if _, warns := keystore.ResolveAll(kp.Store, kp.ProviderInfos(), os.Getenv); len(warns) > 0 {
		for _, w := range warns {
			fmt.Fprintln(os.Stderr, w)
		}
	}
	return nil
}

// BuildProviders constructs the name->provider map with live per-call
// keys (K2 order). firstModel comes from the caller's chain refs so
// both surfaces pin the same default model per provider.
func (kp *keyPlumbing) BuildProviders(cfg *Config) map[string]router.Provider {
	firstModel := map[string]string{}
	for _, ref := range append(append([]string{}, cfg.Models.Default...), cfg.Models.Compact...) {
		p := providerOf(ref)
		if _, ok := firstModel[p]; !ok {
			firstModel[p] = modelOf(ref)
		}
	}
	byName := map[string]router.Provider{}
	for _, p := range kp.providers {
		provName, envVar, literal := p.Name, p.APIKeyEnv, p.APIKey
		keyFn := func(ctx context.Context) string {
			if envVar != "" {
				if k := os.Getenv(envVar); strings.TrimSpace(k) != "" {
					return k
				}
			}
			if snap, ok := keystore.SnapshotFrom(ctx); ok {
				if k := snap[provName]; k != "" {
					return k
				}
			} else if k := kp.Registry.Key(provName); k != "" {
				return k
			}
			return literal
		}
		disableThink := p.Think != nil && !*p.Think
		byName[provName] = router.NewOpenAIKeyed(provName, p.BaseURL, keyFn, firstModel[provName], disableThink)
	}
	return byName
}
