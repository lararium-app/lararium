package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

const (
	capsCacheVersion       = 1
	defaultRefreshInterval = 60 * time.Second
	defaultProbeTimeout    = 10 * time.Second
	maxProbeConcurrency    = 4
)

type capsCache struct {
	Version   int                        `json:"version"`
	Providers map[string]providerCapsRow `json:"providers"`
}

type providerCapsRow struct {
	ProbedAt string            `json:"probed_at"`
	OK       bool              `json:"ok"`
	Models   []cachedModelCaps `json:"models"`
}

type cachedModelCaps struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window"`
	Vision        bool   `json:"vision"`
	Tools         bool   `json:"tools"`
}

// capsCatalog implements surface.ModelCatalog backed by <hearth-home>/caps.json
// and a periodic background refresher. Catalog() never performs network I/O.
type capsCatalog struct {
	cachePath    string
	providers    []ProviderConf
	defaultChain []string
	compactChain []string
	resolveKey   func(ProviderConf) string
	rt           *router.Router
	byName       map[string]router.Provider

	interval     time.Duration
	probeTimeout time.Duration
	stderr       io.Writer

	mu    sync.RWMutex
	cache capsCache
}

var _ surface.ModelCatalog = (*capsCatalog)(nil)

func wireModelCatalogAndProfile(
	srv *surface.Server,
	home string,
	cfg *Config,
	kp *keyPlumbing,
	rt *router.Router,
	byName map[string]router.Provider,
) func() {
	cat := newCapsCatalog(home, cfg, kp, rt, byName)
	ctx, cancel := context.WithCancel(context.Background())
	done := cat.Start(ctx)
	srv.Models = cat
	srv.PersonaHome = home
	srv.ProfileAudit = newProfileAuditHook(home)
	srv.SetWebHome(home)
	return func() {
		cancel()
		<-done
	}
}

func newCapsCatalog(
	home string,
	cfg *Config,
	kp *keyPlumbing,
	rt *router.Router,
	byName map[string]router.Provider,
) *capsCatalog {
	resolveKey := func(p ProviderConf) string {
		if p.APIKeyEnv != "" {
			if k := os.Getenv(p.APIKeyEnv); strings.TrimSpace(k) != "" {
				return k
			}
		}
		if kp != nil && kp.Registry != nil {
			if k := kp.Registry.Key(p.Name); k != "" {
				return k
			}
		}
		return p.APIKey
	}

	c := &capsCatalog{
		cachePath:    filepath.Join(home, "caps.json"),
		providers:    append([]ProviderConf(nil), cfg.Providers...),
		defaultChain: append([]string(nil), cfg.Models.Default...),
		compactChain: append([]string(nil), cfg.Models.Compact...),
		resolveKey:   resolveKey,
		rt:           rt,
		byName:       byName,
		interval:     defaultRefreshInterval,
		probeTimeout: defaultProbeTimeout,
		stderr:       os.Stderr,
		cache: capsCache{
			Version:   capsCacheVersion,
			Providers: map[string]providerCapsRow{},
		},
	}
	c.loadCache()
	c.ApplyPinProfiles()
	return c
}

func (c *capsCatalog) loadCache() {
	data, err := os.ReadFile(c.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		fmt.Fprintf(c.stderr, "warning: model catalog cache %s unreadable (%v); starting empty\n", c.cachePath, err)
		return
	}
	var loaded capsCache
	if err := json.Unmarshal(data, &loaded); err != nil {
		fmt.Fprintf(c.stderr, "warning: model catalog cache %s corrupt (%v); starting empty\n", c.cachePath, err)
		return
	}
	if loaded.Version != capsCacheVersion {
		fmt.Fprintf(c.stderr, "warning: model catalog cache %s unsupported version %d; starting empty\n", c.cachePath, loaded.Version)
		return
	}
	if loaded.Providers == nil {
		loaded.Providers = map[string]providerCapsRow{}
	}
	c.mu.Lock()
	c.cache = loaded
	c.mu.Unlock()
}

func (c *capsCatalog) isCold() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache.Providers) == 0
}

// Start launches the background refresher goroutine and returns a channel
// closed when the goroutine exits cleanly after ctx cancellation.
func (c *capsCatalog) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c.isCold() {
			c.Refresh(ctx)
		}
		interval := c.interval
		if interval <= 0 {
			interval = defaultRefreshInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.Refresh(ctx)
			}
		}
	}()
	return done
}

// Refresh probes every configured provider (concurrency <= 4, 10s per-request
// timeout), updates the in-memory cache, atomically persists caps.json if
// anything changed, and refreshes pin:<ref> router profiles.
func (c *capsCatalog) Refresh(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	timeout := c.probeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	sem := make(chan struct{}, maxProbeConcurrency)
	var wg sync.WaitGroup
	var resMu sync.Mutex
	results := make(map[string]providerCapsRow, len(c.providers))

	for _, p := range c.providers {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(prov ProviderConf) {
			defer wg.Done()
			defer func() { <-sem }()

			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			key := ""
			if c.resolveKey != nil {
				key = c.resolveKey(prov)
			}
			now := time.Now().UTC().Format(time.RFC3339)
			probes, err := router.ListModels(probeCtx, prov.BaseURL, key)

			row := providerCapsRow{
				ProbedAt: now,
				OK:       err == nil,
				Models:   []cachedModelCaps{},
			}
			if err == nil {
				row.Models = make([]cachedModelCaps, 0, len(probes))
				for _, m := range probes {
					row.Models = append(row.Models, cachedModelCaps{
						ID:            m.ID,
						ContextWindow: m.Caps.ContextLength,
						Vision:        m.Caps.SupportsVision,
						Tools:         m.Caps.SupportsTools,
					})
				}
			}

			resMu.Lock()
			results[prov.Name] = row
			resMu.Unlock()
		}(p)
	}
	wg.Wait()

	if ctx.Err() != nil && len(results) == 0 {
		return
	}

	c.mu.Lock()
	oldJSON, oldErr := json.Marshal(c.cache)
	if c.cache.Providers == nil {
		c.cache.Providers = map[string]providerCapsRow{}
	}
	c.cache.Version = capsCacheVersion
	for name, row := range results {
		c.cache.Providers[name] = row
	}
	newJSON, newErr := json.Marshal(c.cache)
	changed := newErr == nil && (oldErr != nil || !bytes.Equal(oldJSON, newJSON))
	c.mu.Unlock()

	if changed {
		c.saveCache(newJSON)
	}
	c.ApplyPinProfiles()
}

func (c *capsCatalog) saveCache(data []byte) {
	if c.cachePath == "" {
		return
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err == nil {
		data = pretty.Bytes()
	}
	if err := os.MkdirAll(filepath.Dir(c.cachePath), 0o755); err != nil {
		fmt.Fprintf(c.stderr, "warning: model catalog cache mkdir failed: %v\n", err)
		return
	}
	tmpPath := c.cachePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		fmt.Fprintf(c.stderr, "warning: model catalog cache write failed: %v\n", err)
		return
	}
	if err := os.Rename(tmpPath, c.cachePath); err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(c.stderr, "warning: model catalog cache rename failed: %v\n", err)
	}
}

// ApplyPinProfiles registers a one-target "pin:<ref>" profile on the router
// for every model currently present in the catalog.
func (c *capsCatalog) ApplyPinProfiles() {
	if c.rt == nil || len(c.byName) == 0 {
		return
	}
	resp := c.Catalog()
	for _, entry := range resp.Models {
		prov, ok := c.byName[entry.Provider]
		if !ok {
			continue
		}
		c.rt.SetProfile("pin:"+entry.Ref, router.Profile{
			Chain: []router.Target{{Provider: prov, Model: entry.ModelID}},
		})
	}
}

// Catalog returns the cached surface.ModelsResponse without performing any
// network I/O.
func (c *capsCatalog) Catalog() surface.ModelsResponse {
	defaultSet := make(map[string]bool, len(c.defaultChain))
	for _, ref := range c.defaultChain {
		defaultSet[ref] = true
	}
	compactSet := make(map[string]bool, len(c.compactChain))
	for _, ref := range c.compactChain {
		compactSet[ref] = true
	}

	provByName := make(map[string]ProviderConf, len(c.providers))
	for _, p := range c.providers {
		provByName[p.Name] = p
	}

	c.mu.RLock()
	provsCache := make(map[string]providerCapsRow, len(c.cache.Providers))
	for k, v := range c.cache.Providers {
		provsCache[k] = v
	}
	c.mu.RUnlock()

	statusFor := func(p ProviderConf, row providerCapsRow, hasCache bool) string {
		if hasCache && row.OK {
			return "online"
		}
		key := ""
		if c.resolveKey != nil {
			key = c.resolveKey(p)
		}
		if key == "" {
			return "no_key"
		}
		return "offline"
	}

	probedAtPtr := func(row providerCapsRow, hasCache bool) *string {
		if !hasCache || row.ProbedAt == "" {
			return nil
		}
		ts := row.ProbedAt
		return &ts
	}

	models := []surface.ModelEntry{}
	seen := map[string]bool{}

	for _, p := range c.providers {
		row, hasCache := provsCache[p.Name]
		if !hasCache || !row.OK {
			continue
		}
		thinking := p.Think == nil || *p.Think
		st := statusFor(p, row, hasCache)
		probed := probedAtPtr(row, hasCache)
		for _, m := range row.Models {
			if m.ID == "" {
				continue
			}
			ref := p.Name + "/" + m.ID
			if seen[ref] {
				continue
			}
			seen[ref] = true
			models = append(models, surface.ModelEntry{
				Ref:         ref,
				Provider:    p.Name,
				ModelID:     m.ID,
				DisplayName: m.ID,
				Capabilities: surface.ModelCapabilities{
					Thinking:      thinking,
					Vision:        m.Vision,
					Tools:         m.Tools,
					Streaming:     true,
					ContextWindow: m.ContextWindow,
				},
				Status:    st,
				IsDefault: defaultSet[ref],
				IsCompact: compactSet[ref],
				ProbedAt:  probed,
			})
		}
	}

	// Chain refs ALWAYS get a row even when the provider is unreachable.
	chainRefs := make([]string, 0, len(c.defaultChain)+len(c.compactChain))
	chainRefs = append(chainRefs, c.defaultChain...)
	chainRefs = append(chainRefs, c.compactChain...)
	for _, ref := range chainRefs {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		provName := providerOf(ref)
		modelID := modelOf(ref)
		p := provByName[provName]
		row, hasCache := provsCache[provName]
		st := statusFor(p, row, hasCache)
		probed := probedAtPtr(row, hasCache)

		var caps surface.ModelCapabilities
		if st == "online" {
			caps = surface.ModelCapabilities{
				Thinking:  p.Think == nil || *p.Think,
				Streaming: true,
			}
		}

		models = append(models, surface.ModelEntry{
			Ref:          ref,
			Provider:     provName,
			ModelID:      modelID,
			DisplayName:  modelID,
			Capabilities: caps,
			Status:       st,
			IsDefault:    defaultSet[ref],
			IsCompact:    compactSet[ref],
			ProbedAt:     probed,
		})
	}

	chatChain := append([]string{}, c.defaultChain...)
	if chatChain == nil {
		chatChain = []string{}
	}
	compactChain := append([]string{}, c.compactChain...)
	if compactChain == nil {
		compactChain = []string{}
	}

	return surface.ModelsResponse{
		Models: models,
		Defaults: surface.ModelDefaults{
			ChatChain:    chatChain,
			CompactChain: compactChain,
		},
	}
}
