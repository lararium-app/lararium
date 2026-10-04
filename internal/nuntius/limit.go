package nuntius

import (
	"sync"
	"time"
)

// Muter implements the pairing lockout (spec §3): five failed code
// attempts from one user ID within pair_fail_window mutes that ID's
// replies for pair_mute. Logging continues while muted — this type
// only answers "may we reply?". In-memory by design: a mute is a
// flood guard, not durable state (N10: no in-memory-only state is
// load-bearing for delivery).
//
// Not safe for concurrent use: it is owned by the single supervisor
// goroutine (spec §2), like the Inbox.
type Muter struct {
	window time.Duration
	mute   time.Duration
	fail   int

	fails  map[string][]time.Time // user id -> fail times in window
	mutedU map[string]time.Time   // user id -> mute expiry
	now    func() time.Time
}

// NewMuter builds the tracker; failThreshold is the strike count (5,
// §3). now is injectable for tests (nil = time.Now).
func NewMuter(window, mute time.Duration, failThreshold int, now func() time.Time) *Muter {
	if now == nil {
		now = time.Now
	}
	return &Muter{
		window: window,
		mute:   mute,
		fail:   failThreshold,
		fails:  map[string][]time.Time{},
		mutedU: map[string]time.Time{},
		now:    now,
	}
}

// Muted reports whether replies to this user id are currently muted.
func (m *Muter) Muted(userID string) bool {
	exp, ok := m.mutedU[userID]
	if !ok {
		return false
	}
	if m.now().Before(exp) {
		return true
	}
	delete(m.mutedU, userID)
	return false
}

// RecordFail logs one failed redemption; the fifth strike inside the
// window starts the mute.
func (m *Muter) RecordFail(userID string) {
	now := m.now()
	hits := m.fails[userID]
	hits = append(hits, now)
	kept := make([]time.Time, 0, len(hits))
	for _, t := range hits {
		if !t.Before(now.Add(-m.window)) {
			kept = append(kept, t)
		}
	}
	m.fails[userID] = kept
	if len(kept) >= m.fail {
		m.mutedU[userID] = now.Add(m.mute)
		delete(m.fails, userID)
	}
}

// Limiter is the pairing-path reply throttle: at most one reply per
// user per gap (spec §3; gap 0 disables, used by CI). Safe for
// concurrent use.
type Limiter struct {
	gap time.Duration

	lock sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// NewLimiter builds the throttle (nil now = time.Now).
func NewLimiter(gap time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{gap: gap, last: map[string]time.Time{}, now: now}
}

// Allow reports whether a reply may go out to this user now, and
// records the send when it does.
func (l *Limiter) Allow(userID string) bool {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.gap <= 0 {
		return true
	}
	last, ok := l.last[userID]
	if ok && l.now().Before(last.Add(l.gap)) {
		return false
	}
	l.last[userID] = l.now()
	return true
}
