package devices

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockClock allows deterministic time advancement in tests.
type mockClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMockClock(t time.Time) *mockClock {
	return &mockClock{now: t.UTC()}
}

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mockClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestStoreAtomicityAnd0600Mode(t *testing.T) {
	dir := t.TempDir()
	clk := newMockClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID:    "pixel-8",
		DisplayName: "My Pixel 8",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailSMS, ReadOnly: false},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Verify <hearth>/custos/devices.json has 0600 permissions
	devicesPath := filepath.Join(dir, "custos", "devices.json")
	fi, err := os.Stat(devicesPath)
	if err != nil {
		t.Fatalf("stat devices.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("devices.json mode = %04o, want 0600", perm)
	}

	// Apply sync and verify <hearth>/devices/<device_id>/<rail>.json has 0600 mode
	events := `[{"title":"Meeting","start":"2026-11-01T14:00:00Z"}]`
	if err := mgr.ApplySync("pixel-8", RailCalendar, []byte(events), nil, 1); err != nil {
		t.Fatalf("ApplySync: %v", err)
	}

	calendarPath := filepath.Join(dir, "devices", "pixel-8", "calendar.json")
	cfi, err := os.Stat(calendarPath)
	if err != nil {
		t.Fatalf("stat calendar.json: %v", err)
	}
	if perm := cfi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("calendar.json mode = %04o, want 0600", perm)
	}

	// Ensure no temp files remained in directories
	entries, _ := os.ReadDir(filepath.Join(dir, "custos"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("stray tmp file in custos: %s", e.Name())
		}
	}
	devEntries, _ := os.ReadDir(filepath.Join(dir, "devices", "pixel-8"))
	for _, e := range devEntries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("stray tmp file in devices: %s", e.Name())
		}
	}
}

func TestTombstoneBootMerge(t *testing.T) {
	dir := t.TempDir()
	clk := newMockClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID:    "pixel-test",
		DisplayName: "Pixel Test",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Forget device
	if err := mgr.Forget("pixel-test"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !mgr.IsTombstoned("pixel-test") {
		t.Fatalf("expected device to be tombstoned")
	}

	// Simulate restart by creating a new Manager instance over the same dir
	mgr2, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager restart: %v", err)
	}

	if !mgr2.IsTombstoned("pixel-test") {
		t.Fatalf("expected tombstone to persist after restart")
	}
	if len(mgr2.List()) != 0 {
		t.Fatalf("tombstoned device must not appear in List(), got %d", len(mgr2.List()))
	}
	if _, gerr := mgr2.Get("pixel-test"); !errors.Is(gerr, ErrDeviceTombstoned) {
		t.Fatalf("Get tombstoned device = %v, want ErrDeviceTombstoned", gerr)
	}

	// Re-registration of tombstoned device clears tombstone
	devFresh := Device{
		DeviceID:    "pixel-test",
		DisplayName: "Pixel Re-registered",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
		},
	}
	if err := mgr2.Register(devFresh); err != nil {
		t.Fatalf("Re-register tombstoned: %v", err)
	}
	if mgr2.IsTombstoned("pixel-test") {
		t.Fatalf("re-registration must clear tombstone")
	}
	if len(mgr2.List()) != 1 {
		t.Fatalf("re-registered device must appear in List(), got %d", len(mgr2.List()))
	}

	// Test corrupt devices.json fails to load
	_ = os.WriteFile(filepath.Join(dir, "custos", "devices.json"), []byte("{corrupt-json"), 0o600)
	if _, err := NewManager(dir, WithClock(clk)); err == nil {
		t.Fatalf("corrupt devices.json must return error on load")
	}
}

func TestSeqMonotonicity(t *testing.T) {
	dir := t.TempDir()
	clk := newMockClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID: "pixel-seq",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Seq 1 -> success
	data1 := `[{"title":"First"}]`
	if err := mgr.ApplySync("pixel-seq", RailCalendar, []byte(data1), nil, 1); err != nil {
		t.Fatalf("ApplySync seq 1: %v", err)
	}

	// Seq 1 duplicate with a snapshot is an idempotent replay (#274);
	// delta-only at seq 1 is still stale.
	if err := mgr.ApplySync("pixel-seq", RailCalendar, []byte(`[{"title":"Duplicate"}]`), nil, 1); err != nil {
		t.Fatalf("snapshot replay at equal seq must be a no-op, got %v", err)
	}
	err = mgr.ApplySync("pixel-seq", RailCalendar, nil, nil, 1)
	if err == nil {
		t.Fatalf("expected ErrStaleSeq on equal seq without snapshot")
	}
	var staleErr *ErrStaleSeq
	if !errors.As(err, &staleErr) {
		t.Fatalf("expected *ErrStaleSeq, got %T: %v", err, err)
	}
	if staleErr.Current != 1 || staleErr.Proposed != 1 {
		t.Fatalf("staleErr = %+v, want current=1 proposed=1", staleErr)
	}

	// Seq 0 backwards -> ErrStaleSeq
	err = mgr.ApplySync("pixel-seq", RailCalendar, []byte(`[{"title":"Old"}]`), nil, 0)
	if err == nil {
		t.Fatalf("expected ErrStaleSeq on lower seq")
	}

	// Seq 10 -> success
	data10 := `[{"title":"Jump to 10"}]`
	if err := mgr.ApplySync("pixel-seq", RailCalendar, []byte(data10), nil, 10); err != nil {
		t.Fatalf("ApplySync seq 10: %v", err)
	}

	// Seq 9 -> ErrStaleSeq
	err = mgr.ApplySync("pixel-seq", RailCalendar, []byte(`[{"title":"Stale"}]`), nil, 9)
	if err == nil {
		t.Fatalf("expected ErrStaleSeq on lower seq 9")
	}

	// Verify file content was NOT overwritten on stale error
	readItems, _, err := mgr.Read("pixel-seq", RailCalendar)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(readItems), "Jump to 10") || strings.Contains(string(readItems), "Stale") {
		t.Fatalf("read items = %s, want content with 'Jump to 10' and without 'Stale'", readItems)
	}
}

func TestRetentionWindowsPerRail(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID: "retention-phone",
		RailsDeclared: []DeclaredRail{
			{Name: RailNotifications, ReadOnly: true},
			{Name: RailCallLog, ReadOnly: true},
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailContacts, ReadOnly: true},
			{Name: RailHealth, ReadOnly: true},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 1. Notifications: TTL 7d + FIFO cap 500
	t.Run("notifications retention", func(t *testing.T) {
		notifs := []map[string]any{
			{"id": "n-old", "title": "Old", "timestamp": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)},
			{"id": "n-kept", "title": "Kept", "timestamp": now.Add(-5 * 24 * time.Hour).Format(time.RFC3339)},
			{"id": "n-recent", "title": "Recent", "timestamp": now.Add(-1 * time.Hour).Format(time.RFC3339)},
		}
		raw, _ := json.Marshal(notifs)
		if err := mgr.ApplySync("retention-phone", RailNotifications, raw, nil, 1); err != nil {
			t.Fatalf("ApplySync: %v", err)
		}

		items, _, err := mgr.Read("retention-phone", RailNotifications)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		var parsed []map[string]any
		_ = json.Unmarshal(items, &parsed)
		if len(parsed) != 2 {
			t.Fatalf("notifications count = %d, want 2 (n-old should be pruned)", len(parsed))
		}

		// Test FIFO cap 500
		large := make([]map[string]any, 550)
		for i := range 550 {
			large[i] = map[string]any{
				"id":        fmt.Sprintf("n-%d", i),
				"timestamp": now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			}
		}
		largeRaw, _ := json.Marshal(large)
		if err := mgr.ApplySync("retention-phone", RailNotifications, largeRaw, nil, 2); err != nil {
			t.Fatalf("ApplySync large: %v", err)
		}
		items, _, _ = mgr.Read("retention-phone", RailNotifications)
		_ = json.Unmarshal(items, &parsed)
		if len(parsed) != 500 {
			t.Fatalf("notifications capped = %d, want 500", len(parsed))
		}
		// First item should be n-50 (oldest 50 dropped)
		if parsed[0]["id"] != "n-50" {
			t.Fatalf("first item = %v, want n-50", parsed[0]["id"])
		}
	})

	// 2. Calllog: TTL 30d + cap 200
	t.Run("calllog retention", func(t *testing.T) {
		calls := []map[string]any{
			{"id": "c-old", "date": now.Add(-31 * 24 * time.Hour).Format(time.RFC3339)},
			{"id": "c-kept", "date": now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)},
		}
		raw, _ := json.Marshal(calls)
		if err := mgr.ApplySync("retention-phone", RailCallLog, raw, nil, 1); err != nil {
			t.Fatalf("ApplySync: %v", err)
		}
		items, _, err := mgr.Read("retention-phone", RailCallLog)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		var parsed []map[string]any
		_ = json.Unmarshal(items, &parsed)
		if len(parsed) != 1 || parsed[0]["id"] != "c-kept" {
			t.Fatalf("calllog count = %d, want 1 (c-old pruned)", len(parsed))
		}

		// Cap 200
		large := make([]map[string]any, 250)
		for i := range 250 {
			large[i] = map[string]any{
				"id":   fmt.Sprintf("c-%d", i),
				"date": now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			}
		}
		largeRaw, _ := json.Marshal(large)
		if err := mgr.ApplySync("retention-phone", RailCallLog, largeRaw, nil, 2); err != nil {
			t.Fatalf("ApplySync cap: %v", err)
		}
		items, _, _ = mgr.Read("retention-phone", RailCallLog)
		_ = json.Unmarshal(items, &parsed)
		if len(parsed) != 200 {
			t.Fatalf("calllog capped = %d, want 200", len(parsed))
		}
		if parsed[0]["id"] != "c-50" {
			t.Fatalf("first call = %v, want c-50", parsed[0]["id"])
		}
	})

	// 3. Calendar: window [-30d, +90d]
	t.Run("calendar window retention", func(t *testing.T) {
		events := []map[string]any{
			{"id": "e-past-out", "start": now.Add(-31 * 24 * time.Hour).Format(time.RFC3339)},
			{"id": "e-past-in", "start": now.Add(-25 * 24 * time.Hour).Format(time.RFC3339)},
			{"id": "e-today", "start": now.Format(time.RFC3339)},
			{"id": "e-future-in", "start": now.Add(80 * 24 * time.Hour).Format(time.RFC3339)},
			{"id": "e-future-out", "start": now.Add(91 * 24 * time.Hour).Format(time.RFC3339)},
		}
		raw, _ := json.Marshal(events)
		if err := mgr.ApplySync("retention-phone", RailCalendar, raw, nil, 1); err != nil {
			t.Fatalf("ApplySync: %v", err)
		}
		items, _, err := mgr.Read("retention-phone", RailCalendar)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		var parsed []map[string]any
		_ = json.Unmarshal(items, &parsed)
		if len(parsed) != 3 {
			t.Fatalf("calendar count = %d, want 3", len(parsed))
		}
		expectedIDs := map[string]bool{"e-past-in": true, "e-today": true, "e-future-in": true}
		for _, e := range parsed {
			id := fmt.Sprintf("%v", e["id"])
			if !expectedIDs[id] {
				t.Fatalf("unexpected event kept: %s", id)
			}
		}
	})

	// 4. Contacts: snapshot + deltas
	t.Run("contacts snapshot and deltas", func(t *testing.T) {
		snap := []map[string]any{
			{"id": "c1", "name": "Alice"},
			{"id": "c2", "name": "Bob"},
		}
		raw, _ := json.Marshal(snap)
		if err := mgr.ApplySync("retention-phone", RailContacts, raw, nil, 1); err != nil {
			t.Fatalf("ApplySync: %v", err)
		}

		// Apply deltas: add Charlie, update Alice, delete Bob
		deltaAdd := Delta{
			Op:   "add",
			ID:   "c3",
			Data: json.RawMessage(`{"id":"c3","name":"Charlie"}`),
		}
		deltaUpdate := Delta{
			Op:   "update",
			ID:   "c1",
			Data: json.RawMessage(`{"id":"c1","name":"Alice Smith"}`),
		}
		deltaDel := Delta{
			Op: "delete",
			ID: "c2",
		}
		if err := mgr.ApplySync("retention-phone", RailContacts, nil, []Delta{deltaAdd, deltaUpdate, deltaDel}, 2); err != nil {
			t.Fatalf("ApplySync deltas: %v", err)
		}

		items, _, err := mgr.Read("retention-phone", RailContacts)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		var parsed []map[string]any
		_ = json.Unmarshal(items, &parsed)
		if len(parsed) != 2 {
			t.Fatalf("contacts count = %d, want 2", len(parsed))
		}
		names := make(map[string]string)
		for _, c := range parsed {
			names[fmt.Sprintf("%v", c["id"])] = fmt.Sprintf("%v", c["name"])
		}
		if names["c1"] != "Alice Smith" {
			t.Fatalf("c1 name = %q, want Alice Smith", names["c1"])
		}
		if names["c3"] != "Charlie" {
			t.Fatalf("c3 name = %q, want Charlie", names["c3"])
		}
		if _, hasBob := names["c2"]; hasBob {
			t.Fatalf("c2 Bob was not deleted")
		}
	})

	// 5. Health: last snapshot only
	t.Run("health last-snapshot-only", func(t *testing.T) {
		snap1 := `{"steps": 5000, "date": "2026-11-01"}`
		if err := mgr.ApplySync("retention-phone", RailHealth, []byte(snap1), nil, 1); err != nil {
			t.Fatalf("ApplySync health 1: %v", err)
		}

		snap2 := `{"steps": 10000, "heart_rate": 72, "date": "2026-11-02"}`
		if err := mgr.ApplySync("retention-phone", RailHealth, []byte(snap2), nil, 2); err != nil {
			t.Fatalf("ApplySync health 2: %v", err)
		}

		items, _, err := mgr.Read("retention-phone", RailHealth)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(items, &parsed)
		if parsed["steps"] != float64(10000) || parsed["heart_rate"] != float64(72) {
			t.Fatalf("health = %+v, want snapshot 2 contents", parsed)
		}
	})
}

func TestForgetWipes(t *testing.T) {
	dir := t.TempDir()
	clk := newMockClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID: "pixel-wipe",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailHealth, ReadOnly: true},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Sync data to create directory and files
	_ = mgr.ApplySync("pixel-wipe", RailCalendar, []byte(`[{"title":"Meeting"}]`), nil, 1)
	_ = mgr.ApplySync("pixel-wipe", RailHealth, []byte(`{"steps":5000}`), nil, 1)

	devDir := filepath.Join(dir, "devices", "pixel-wipe")
	if _, err := os.Stat(devDir); err != nil {
		t.Fatalf("expected device directory %s to exist before forget", devDir)
	}

	// Forget wipes
	if err := mgr.Forget("pixel-wipe"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// Directory tree must be wiped completely
	if _, err := os.Stat(devDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("device directory %s still exists after forget: %v", devDir, err)
	}

	// Row must be tombstoned
	if !mgr.IsTombstoned("pixel-wipe") {
		t.Fatalf("device not tombstoned after forget")
	}

	// Read after forget returns ErrDeviceTombstoned
	if _, _, err := mgr.Read("pixel-wipe", RailCalendar); !errors.Is(err, ErrDeviceTombstoned) {
		t.Fatalf("Read after forget = %v, want ErrDeviceTombstoned", err)
	}
}

func TestApplySync_SnapshotReplay(t *testing.T) {
	dir := t.TempDir()
	clk := newMockClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const id = "pixel-replay"
	if err := mgr.Register(Device{
		DeviceID:      id,
		RailsDeclared: []DeclaredRail{{Name: RailCalendar, ReadOnly: true}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	snap := []byte(`[{"title":"First"}]`)

	// (a) initial snapshot at seq 1
	if err := mgr.ApplySync(id, RailCalendar, snap, nil, 1); err != nil {
		t.Fatalf("seq 1: %v", err)
	}
	_, _, err = mgr.Read(id, RailCalendar)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	railFile := filepath.Join(dir, "devices", id, RailCalendar+".json")
	before, err := os.ReadFile(railFile)
	if err != nil {
		t.Fatalf("read rail file: %v", err)
	}

	// Advance the clock so any rewrite would change last_sync.
	clk.Advance(time.Hour)

	assertUnchanged := func(step string) {
		t.Helper()
		after, err := os.ReadFile(railFile)
		if err != nil {
			t.Fatalf("%s: read rail file: %v", step, err)
		}
		if string(after) != string(before) {
			t.Fatalf("%s: stored doc changed:\nbefore=%s\nafter=%s", step, before, after)
		}
	}

	// (b) identical snapshot at seq 1 -> ok, doc unchanged
	if err := mgr.ApplySync(id, RailCalendar, snap, nil, 1); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	assertUnchanged("identical replay")

	// (c) different payload at same seq -> still a no-op; replay never rewrites
	if err := mgr.ApplySync(id, RailCalendar, []byte(`[{"title":"Different"}]`), nil, 1); err != nil {
		t.Fatalf("different-payload replay: %v", err)
	}
	assertUnchanged("different-payload replay")

	// (d) seq 0 after 1 -> stale
	var staleErr *ErrStaleSeq
	err = mgr.ApplySync(id, RailCalendar, snap, nil, 0)
	if !errors.As(err, &staleErr) {
		t.Fatalf("seq 0: want *ErrStaleSeq, got %v", err)
	}

	// (e) delta-only (nil snapshot) at seq 1 -> stale
	staleErr = nil
	err = mgr.ApplySync(id, RailCalendar, nil, nil, 1)
	if !errors.As(err, &staleErr) {
		t.Fatalf("delta-only seq 1: want *ErrStaleSeq, got %v", err)
	}
	if staleErr.Current != 1 || staleErr.Proposed != 1 {
		t.Fatalf("staleErr = %+v, want current=1 proposed=1", staleErr)
	}
	assertUnchanged("rejected pushes")

	// (f) after replays, seq 2 advances
	if err := mgr.ApplySync(id, RailCalendar, []byte(`[{"title":"Second"}]`), nil, 2); err != nil {
		t.Fatalf("seq 2: %v", err)
	}
	items, _, err := mgr.Read(id, RailCalendar)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(items), "Second") {
		t.Fatalf("items = %s, want Second", items)
	}
}
