package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/surface"
)

var errFastiAnnouncerUnset = errors.New("fasti: announcer bridge not configured")

// headlessTurnDoor is the narrow turn-door interface satisfied by *surface.Hub (spec D2).
type headlessTurnDoor interface {
	RunHeadlessTurn(
		ctx context.Context,
		sessionID, text, channel string,
		updateID int64,
		onDelta func(delta string),
	) error
}

// fastiRealClock implements loop.Clock against the system wall clock for daemon serve mode.
type fastiRealClock struct{}

func (fastiRealClock) Now() time.Time {
	return time.Now().UTC()
}

func (fastiRealClock) AfterFunc(d time.Duration, f func()) loop.Timer {
	return time.AfterFunc(d, f)
}

// fastiAnnouncerHolder is a concurrent-safe indirection holder for fasti.Announcer
// so the fasti engine can be wired before ServeSocket while *nuntius.Bridge is
// populated once startNuntius returns (spec D7).
type fastiAnnouncerHolder struct {
	mu  sync.RWMutex
	ann fasti.Announcer
}

func newFastiAnnouncerHolder() *fastiAnnouncerHolder {
	return &fastiAnnouncerHolder{}
}

// Set attaches the active announcer (such as *nuntius.Bridge). Safe to call on a nil receiver.
func (h *fastiAnnouncerHolder) Set(ann fasti.Announcer) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := ann.(*nuntius.Bridge); ok && b == nil {
		h.ann = nil
		return
	}
	h.ann = ann
}

// Announce delegates announcement delivery to the underlying bridge if configured.
func (h *fastiAnnouncerHolder) Announce(ctx context.Context, text string) error {
	if h == nil {
		return errFastiAnnouncerUnset
	}
	h.mu.RLock()
	ann := h.ann
	h.mu.RUnlock()
	if ann == nil {
		return errFastiAnnouncerUnset
	}
	return ann.Announce(ctx, text)
}

func mapTurnDoorErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, surface.ErrTurnInFlight) {
		return fmt.Errorf("%w: %w", fasti.ErrTurnInFlight, err)
	}
	return err
}

func newFastiRunner(door headlessTurnDoor) fasti.TurnRunner {
	return func(ctx context.Context, sessionID, prompt string) error {
		if door == nil {
			return nil
		}
		channel := fasti.TaskKindFromContext(ctx)
		return mapTurnDoorErr(door.RunHeadlessTurn(ctx, sessionID, prompt, channel, 0, nil))
	}
}

func newFastiTextRunner(door headlessTurnDoor) fasti.TextTurnRunner {
	return func(ctx context.Context, sessionID, prompt string) (string, error) {
		if door == nil {
			return "", nil
		}
		channel := fasti.TaskKindFromContext(ctx)
		var b strings.Builder
		err := door.RunHeadlessTurn(ctx, sessionID, prompt, channel, 0, func(delta string) {
			b.WriteString(delta)
		})
		if err != nil {
			return "", mapTurnDoorErr(err)
		}
		return strings.TrimSpace(b.String()), nil
	}
}

// startFasti constructs and starts the fasti proactivity engine when cfg.Enabled is true.
// When cfg.Enabled is false, no engine is constructed and all return values are nil.
func startFasti(
	ctx context.Context,
	cfg fasti.Config,
	home string,
	door headlessTurnDoor,
	clk loop.Clock,
	extraOpts ...fasti.EngineOption,
) (*fasti.Engine, *fastiAnnouncerHolder, func(), error) {
	if !cfg.Enabled {
		return nil, nil, nil, nil
	}
	if clk == nil {
		clk = fastiRealClock{}
	}
	holder := newFastiAnnouncerHolder()
	opts := []fasti.EngineOption{
		fasti.WithClock(clk),
		fasti.WithRunner(newFastiRunner(door)),
		fasti.WithTextRunner(newFastiTextRunner(door)),
		fasti.WithAnnouncer(holder),
	}
	opts = append(opts, extraOpts...)

	eng, err := fasti.New(cfg, home, opts...)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := eng.Start(ctx); err != nil {
		eng.Stop()
		return nil, nil, nil, err
	}
	return eng, holder, eng.Stop, nil
}

func mustWireFasti(
	ctx context.Context,
	cfg fasti.Config,
	home string,
	srv *surface.Server,
	door headlessTurnDoor,
) (*fastiAnnouncerHolder, func()) {
	eng, holder, stopFasti, err := startFasti(ctx, cfg, home, door, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv.Fasti = eng
	return holder, stopFasti
}
