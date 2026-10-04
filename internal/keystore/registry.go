package keystore

import (
	"context"
	"sync/atomic"
)

// Registry is the daemon's live key map: hot-swapped on reload, read
// per turn. Turns pin a snapshot at start (KEYS-SPEC K6 turn
// atomicity — revoking a key never mutates a running turn).
type Registry struct {
	ptr atomic.Pointer[map[string]string]
}

// NewRegistry seeds the registry with a copy of initial.
func NewRegistry(initial map[string]string) *Registry {
	r := &Registry{}
	r.Swap(initial)
	return r
}

// Snapshot returns the current map. Treat it as immutable: it is
// shared with every other reader.
func (r *Registry) Snapshot() map[string]string {
	return *r.ptr.Load()
}

// Swap replaces the map with a copy of next.
func (r *Registry) Swap(next map[string]string) {
	cp := make(map[string]string, len(next))
	for k, v := range next {
		cp[k] = v
	}
	r.ptr.Store(&cp)
}

// Key returns the current value for name, "" when absent.
func (r *Registry) Key(name string) string {
	return r.Snapshot()[name]
}

type ctxKey struct{}

// WithSnapshot pins snap to ctx for the lifetime of one turn.
func WithSnapshot(ctx context.Context, snap map[string]string) context.Context {
	return context.WithValue(ctx, ctxKey{}, snap)
}

// SnapshotFrom returns the turn-pinned snapshot, if any.
func SnapshotFrom(ctx context.Context) (map[string]string, bool) {
	snap, ok := ctx.Value(ctxKey{}).(map[string]string)
	return snap, ok
}
