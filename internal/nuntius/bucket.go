package nuntius

import (
	"context"
	"sync"
	"time"
)

// Bucket is a token bucket with an explicit pause (N9). A 429's
// retry_after pauses the bucket it came from; Wait blocks until a
// token is available and the pause has expired. now and sleep are
// injectable so tests assert pause durations without wall-clock cost.
// Safe for concurrent use.
type Bucket struct {
	rate  float64 // tokens per second
	burst float64

	mu          sync.Mutex
	tokens      float64
	updated     time.Time
	pausedUntil time.Time

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewBucket builds a bucket refilling at rate tokens/second with the
// given burst capacity (nil now = time.Now, nil sleep = ctx-aware
// time.Sleep).
func NewBucket(rate float64, burst int, now func() time.Time, sleep func(context.Context, time.Duration) error) *Bucket {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = realSleep
	}
	b := &Bucket{rate: rate, burst: float64(burst), now: now, sleep: sleep}
	b.tokens = float64(burst)
	b.updated = now()
	return b
}

// realSleep waits d unless ctx is cancelled first.
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait takes one token, blocking through refills and pauses.
func (b *Bucket) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		now := b.now()
		if d := now.Sub(b.updated); d > 0 {
			b.tokens += d.Seconds() * b.rate
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
			b.updated = now
		}
		var wait time.Duration
		switch {
		case now.Before(b.pausedUntil):
			wait = b.pausedUntil.Sub(now)
		case b.tokens >= 1:
			b.tokens--
			b.mu.Unlock()
			return nil
		default:
			wait = time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		}
		b.mu.Unlock()
		if err := b.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Pause stops the bucket for at least d from now (a 429's
// retry_after). Overlapping pauses extend, never shorten.
func (b *Bucket) Pause(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	until := b.now().Add(d)
	if until.After(b.pausedUntil) {
		b.pausedUntil = until
	}
}

// FloodControl is the two-bucket gate in front of every outbound send
// (N9): global ≤ 25 calls/s AND per-chat ≤ 1/s. Polling has its own
// bucket and is never blocked by a send pause.
type FloodControl struct {
	mu    sync.Mutex
	glob  *Bucket
	chats map[string]*Bucket

	chatFactory func() *Bucket
}

// NewFloodControl builds the gate at the spec rates (N9): global 25/s
// burst 25, per-chat 1/s burst 1. now and sleep are injectable for
// tests (nil = wall clock, ctx-aware time.Sleep).
func NewFloodControl(now func() time.Time, sleep func(context.Context, time.Duration) error) *FloodControl {
	return &FloodControl{
		glob:        NewBucket(25, 25, now, sleep),
		chats:       map[string]*Bucket{},
		chatFactory: func() *Bucket { return NewBucket(1, 1, now, sleep) },
	}
}

// chatBucketFor returns (creating on first use) the bucket for one
// chat.
func (fc *FloodControl) chatBucketFor(chatID string) *Bucket {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if b, ok := fc.chats[chatID]; ok {
		return b
	}
	b := fc.chatFactory()
	fc.chats[chatID] = b
	return b
}

// Send admits one outbound send to chatID: both the chat's bucket and
// the global bucket must grant a token. The chat bucket is waited on
// first so a global token is never burned while parked on a per-chat
// pause.
func (fc *FloodControl) Send(ctx context.Context, chatID string) error {
	if err := fc.chatBucketFor(chatID).Wait(ctx); err != nil {
		return err
	}
	return fc.glob.Wait(ctx)
}

// PauseGlobal pauses the global send bucket (a global 429).
func (fc *FloodControl) PauseGlobal(d time.Duration) { fc.glob.Pause(d) }

// PauseChat pauses one chat's bucket (a per-chat 429).
func (fc *FloodControl) PauseChat(chatID string, d time.Duration) {
	fc.chatBucketFor(chatID).Pause(d)
}
