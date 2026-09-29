package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

type runtime struct {
	cfg    *Config
	rt     *router.Router
	sess   *loop.Session
	home   string
	closer []io.Closer
}

func buildRuntime(cfg *Config, chain []string) (*runtime, error) {
	// Build provider instances by name. defaultModel per provider = the
	// model from its first chain ref (targets override per-request).
	firstModel := map[string]string{}
	for _, ref := range append(append([]string{}, cfg.Models.Default...), cfg.Models.Compact...) {
		p := providerOf(ref)
		if _, ok := firstModel[p]; !ok {
			firstModel[p] = modelOf(ref)
		}
	}
	byName := map[string]router.Provider{}
	for _, p := range cfg.Providers {
		key := p.APIKey
		if p.APIKeyEnv != "" {
			key = os.Getenv(p.APIKeyEnv)
		}
		byName[p.Name] = router.NewOpenAI(p.BaseURL, key, firstModel[p.Name])
	}

	targets := func(refs []string) ([]router.Target, error) {
		var ts []router.Target
		for _, ref := range refs {
			prov, ok := byName[providerOf(ref)]
			if !ok {
				return nil, fmt.Errorf("unknown provider in ref %q", ref)
			}
			ts = append(ts, router.Target{Provider: prov, Model: modelOf(ref)})
		}
		return ts, nil
	}

	def, err := targets(chain)
	if err != nil {
		return nil, err
	}
	rt := router.NewRouter(router.Profile{Chain: def})
	if len(cfg.Models.Compact) > 0 {
		ct, err := targets(cfg.Models.Compact)
		if err != nil {
			return nil, err
		}
		rt.SetProfile("compact", router.Profile{Chain: ct})
	} else {
		rt.SetProfile("compact", router.Profile{Chain: def})
	}

	// Persona tree → system prompt.
	sys, warnings, err := loop.SystemPrompt(cfg.Hearth.Home)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "⚠ "+w)
	}

	// Open (or create) the main session.
	sessDir := filepath.Join(cfg.Hearth.Home, "sessions", "main")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return nil, err
	}
	log, err := penatus.OpenLog(sessDir)
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}

	return &runtime{
		cfg:  cfg,
		rt:   rt,
		home: cfg.Hearth.Home,
		sess: &loop.Session{
			Log:            log,
			Sys:            sys,
			Router:         rt,
			CompactProfile: "compact",
			TriggerPct:     cfg.Hearth.CompactionTriggerPct,
		},
	}, nil
}

func (r *runtime) Close() {
	for _, c := range r.closer {
		_ = c.Close()
	}
}

func repl(rt *runtime) {
	fmt.Printf("hearthd — %s (ctrl-C to interrupt a turn, /exit to quit)\n", rt.home)
	// Show probed window once, up front (detection, not assumption).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	caps, err := rt.rt.Probe(ctx, "chat")
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ probe failed (compaction trigger disabled): %v\n", err)
	} else {
		fmt.Printf("model window: %d tokens (source: %s)\n", caps.ContextLength, caps.Source)
	}

	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !sc.Scan() {
			return
		}
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case line == "/exit" || line == "/quit":
			return
		case line == "/compact":
			if err := rt.sess.Compact(context.Background()); err != nil {
				fmt.Fprintln(os.Stderr, "compact:", err)
			} else {
				fmt.Println("(compacted)")
			}
			continue
		case line == "/dump":
			for _, ev := range rt.sess.Log.Live() {
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
			}
			continue
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		t0 := time.Now()
		fmt.Print("\nlararium> ")
		text, err := rt.sess.RunTurn(ctx, line, func(d string) { fmt.Print(d) })
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nerror:", err)
			if text != "" {
				fmt.Fprintln(os.Stderr, "(partial reply was NOT logged)")
			}
			continue
		}
		fmt.Printf("\n  [%s]", time.Since(t0).Round(100*time.Millisecond))

		// Post-turn compaction check (spec §3: after completed turns only).
		if did, err := rt.sess.MaybeCompact(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "⚠ compact check:", err)
		} else if did {
			fmt.Print(" [compacted]")
		}
	}
}
