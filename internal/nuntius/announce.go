package nuntius

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	globalFallbackMu       sync.RWMutex
	globalFallbackAnnounce func(ctx context.Context, text string) error
)

// SetAnnounceFallback registers the global fallback callback invoked when
// nuntius is absent or disabled (D8 fallback).
// A nil callback safely clears the fallback (nil-safe).
func SetAnnounceFallback(fn func(ctx context.Context, text string) error) {
	globalFallbackMu.Lock()
	defer globalFallbackMu.Unlock()
	globalFallbackAnnounce = fn
}

// SetAnnounceFallback sets the instance fallback callback on Bridge when
// nuntius is disabled (D8 fallback). If b is nil, it sets the global fallback.
func (b *Bridge) SetAnnounceFallback(fn func(ctx context.Context, text string) error) {
	if b == nil {
		SetAnnounceFallback(fn)
		return
	}
	b.announceFallbackMu.Lock()
	defer b.announceFallbackMu.Unlock()
	b.announceFallback = fn
}

// invokeFallback invokes either the instance or global fallback seam exactly once.
func (b *Bridge) invokeFallback(ctx context.Context, text string) error {
	var fn func(ctx context.Context, text string) error
	if b != nil {
		b.announceFallbackMu.RLock()
		fn = b.announceFallback
		b.announceFallbackMu.RUnlock()
	}
	if fn == nil {
		globalFallbackMu.RLock()
		fn = globalFallbackAnnounce
		globalFallbackMu.RUnlock()
	}
	if fn != nil {
		return fn(ctx, text)
	}
	return nil
}

// Announce delivers a proactive announcement text to recorded owner chats
// through the flood-gated send path (spec decision D7).
//
// When nuntius is not enabled or absent (nil receiver), it falls back to
// the registered fallback seam (spec decision D8).
//
// Fan-out sends to every recorded owner chat exactly once. If any chat send
// fails, it logs the error, records it, and continues fan-out to remaining chats.
// An empty owner set is a no-op returning nil.
func (b *Bridge) Announce(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if b == nil || !b.cfg.Enabled {
		return b.invokeFallback(ctx, text)
	}

	chats := b.ownerChats()
	if len(chats) == 0 {
		return nil
	}

	var errs []error
	for _, chatID := range chats {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if _, err := b.tg.SendMessage(ctx, chatID, text, nil); err != nil {
			b.log.Printf(LogReplyFailed, err)
			errs = append(errs, fmt.Errorf("nuntius: announce to chat %s: %w", chatID, err))
		}
	}

	return errors.Join(errs...)
}
