package devices

import (
	"sync"
	"time"
)

// Clock provides current time for testing without wall-clock sleeps.
type Clock interface {
	Now() time.Time
}

// realClock uses the system wall clock.
type realClock struct{}

// Now returns the current UTC time.
func (realClock) Now() time.Time {
	return time.Now().UTC()
}

// DefaultGracePeriod is the 30-second mobile handoff grace window per DEVICE-RAILS-SPEC D7.
const DefaultGracePeriod = 30 * time.Second

// livenessTracker manages online/offline state and the 30s grace window.
type livenessTracker struct {
	mu          sync.Mutex
	clock       Clock
	gracePeriod time.Duration
	online      map[string]bool
	graceTimers map[string]*time.Timer
	lastSeen    map[string]time.Time
	onOffline   func(deviceID string)
}

func newLivenessTracker(clock Clock, grace time.Duration, onOffline func(deviceID string)) *livenessTracker {
	if clock == nil {
		clock = realClock{}
	}
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	return &livenessTracker{
		clock:       clock,
		gracePeriod: grace,
		online:      make(map[string]bool),
		graceTimers: make(map[string]*time.Timer),
		lastSeen:    make(map[string]time.Time),
		onOffline:   onOffline,
	}
}

// SetOnline updates online status. A drop signal (online=false) starts a 30s grace window timer;
// the device stays marked online until the grace window elapses. Reconnecting cancels the timer.
func (lt *livenessTracker) SetOnline(deviceID string, online bool) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	if online {
		if t, ok := lt.graceTimers[deviceID]; ok {
			t.Stop()
			delete(lt.graceTimers, deviceID)
		}
		lt.online[deviceID] = true
		lt.lastSeen[deviceID] = lt.clock.Now()
		return
	}

	// Drop signal
	if !lt.online[deviceID] {
		return
	}
	if _, pending := lt.graceTimers[deviceID]; pending {
		return
	}

	// Schedule offline transition after gracePeriod
	lt.graceTimers[deviceID] = time.AfterFunc(lt.gracePeriod, func() {
		lt.mu.Lock()
		defer lt.mu.Unlock()
		delete(lt.graceTimers, deviceID)
		lt.online[deviceID] = false
		if lt.onOffline != nil {
			lt.onOffline(deviceID)
		}
	})
}

// IsOnline reports whether device is currently online (including during active grace window).
func (lt *livenessTracker) IsOnline(deviceID string) bool {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.online[deviceID]
}

// SetLastSeen updates the last_seen stamp for a device.
func (lt *livenessTracker) SetLastSeen(deviceID string, t time.Time) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.lastSeen[deviceID] = t
}

// LastSeen returns the last seen timestamp for a device.
func (lt *livenessTracker) LastSeen(deviceID string) time.Time {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.lastSeen[deviceID]
}

// Cancel cancels any pending grace timer and clears liveness for deviceID.
func (lt *livenessTracker) Cancel(deviceID string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if t, ok := lt.graceTimers[deviceID]; ok {
		t.Stop()
		delete(lt.graceTimers, deviceID)
	}
	delete(lt.online, deviceID)
	delete(lt.lastSeen, deviceID)
}

// SetGracePeriod dynamically updates the grace duration (used in tests).
func (lt *livenessTracker) SetGracePeriod(d time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.gracePeriod = d
}
