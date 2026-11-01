package devices

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DeviceStoreDoc is the serialized format of <hearth>/custos/devices.json.
type DeviceStoreDoc struct {
	Devices    []Device `json:"devices"`
	Tombstones []string `json:"tombstones,omitempty"`
}

// RailDataDoc is the serialized format of <hearth>/devices/<device_id>/<rail>.json.
type RailDataDoc struct {
	Seq      int64           `json:"seq"`
	LastSync time.Time       `json:"last_sync"`
	Items    json.RawMessage `json:"items"`
}

// Manager manages device registration, rail stores, liveness, and dynamic tool sources.
type Manager struct {
	mu          sync.RWMutex
	hearthDir   string
	custosDir   string
	devicesDir  string
	devicesFile string

	clock      Clock
	devices    map[string]Device
	tombstones map[string]bool
	railSeqs   map[string]map[string]int64
	railSyncs  map[string]map[string]time.Time
	liveness   *livenessTracker
	broker     ActionBroker
}

// Option configures Manager.
type Option func(*Manager)

// WithClock injects a custom clock for testing without sleeps.
func WithClock(clk Clock) Option {
	return func(m *Manager) {
		m.clock = clk
	}
}

// WithGracePeriod sets the offline grace window duration.
func WithGracePeriod(d time.Duration) Option {
	return func(m *Manager) {
		if m.liveness != nil {
			m.liveness.SetGracePeriod(d)
		}
	}
}

// NewManager initializes a device Manager rooted at hearthDir.
// It loads <hearth>/custos/devices.json applying boot merge semantics per CONNECTOR-LIFE-SPEC D2.
func NewManager(hearthDir string, opts ...Option) (*Manager, error) {
	custosDir := filepath.Join(hearthDir, "custos")
	devicesDir := filepath.Join(hearthDir, "devices")
	devicesFile := filepath.Join(custosDir, "devices.json")

	m := &Manager{
		hearthDir:   hearthDir,
		custosDir:   custosDir,
		devicesDir:  devicesDir,
		devicesFile: devicesFile,
		clock:       realClock{},
		devices:     make(map[string]Device),
		tombstones:  make(map[string]bool),
		railSeqs:    make(map[string]map[string]int64),
		railSyncs:   make(map[string]map[string]time.Time),
	}

	for _, opt := range opts {
		opt(m)
	}

	m.liveness = newLivenessTracker(m.clock, DefaultGracePeriod, nil)

	// Boot merge from <hearth>/custos/devices.json
	if err := m.loadStore(); err != nil {
		return nil, fmt.Errorf("load device store: %w", err)
	}

	return m, nil
}

// loadStore loads <hearth>/custos/devices.json if present.
func (m *Manager) loadStore() error {
	raw, err := os.ReadFile(m.devicesFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}

	var doc DeviceStoreDoc
	if trimmed[0] == '[' {
		// Array format support
		var devList []Device
		if err := json.Unmarshal(trimmed, &devList); err != nil {
			return fmt.Errorf("unmarshal devices array: %w", err)
		}
		doc.Devices = devList
	} else {
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return fmt.Errorf("unmarshal devices doc: %w", err)
		}
	}

	for _, id := range doc.Tombstones {
		if ValidateDeviceID(id) == nil {
			m.tombstones[id] = true
		}
	}

	for _, dev := range doc.Devices {
		// Pending rows without cards / invalid rows never load (CONNECTOR-LIFE-SPEC D2 mirror law)
		if ValidateDeviceID(dev.DeviceID) != nil {
			continue
		}
		if dev.Tombstoned || m.tombstones[dev.DeviceID] {
			m.tombstones[dev.DeviceID] = true
			continue
		}
		if ValidateRails(dev.RailsDeclared) != nil {
			continue
		}
		m.devices[dev.DeviceID] = dev
		if !dev.LastSeen.IsZero() {
			m.liveness.SetLastSeen(dev.DeviceID, dev.LastSeen)
		}
	}

	return nil
}

// saveStoreLocked writes the store atomically via temp+rename+fsync, mode 0600.
func (m *Manager) saveStoreLocked() error {
	var devList []Device
	for _, dev := range m.devices {
		// Fold in latest last_seen if known
		if ls := m.liveness.LastSeen(dev.DeviceID); !ls.IsZero() {
			dev.LastSeen = ls
		}
		devList = append(devList, dev)
	}
	sort.Slice(devList, func(i, j int) bool {
		return devList[i].DeviceID < devList[j].DeviceID
	})

	var tombs []string
	for id := range m.tombstones {
		tombs = append(tombs, id)
	}
	sort.Strings(tombs)

	doc := DeviceStoreDoc{
		Devices:    devList,
		Tombstones: tombs,
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal devices doc: %w", err)
	}
	data = append(data, '\n')

	return writeAtomic(m.custosDir, "devices.json", data)
}

// Register persists a newly approved device. Validates slug and rails first.
// Re-registration of a tombstoned device_id clears the tombstone.
func (m *Manager) Register(dev Device) error {
	if err := ValidateDeviceID(dev.DeviceID); err != nil {
		return err
	}
	if err := ValidateRails(dev.RailsDeclared); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock.Now()
	if dev.RegisteredAt.IsZero() {
		dev.RegisteredAt = now
	}
	if dev.LastSeen.IsZero() {
		dev.LastSeen = now
	}
	if dev.SyncEpoch == 0 {
		dev.SyncEpoch = 1
	}
	dev.Tombstoned = false

	// Clear tombstone on fresh registration per DEVICE-RAILS-SPEC D3
	delete(m.tombstones, dev.DeviceID)
	m.devices[dev.DeviceID] = dev
	m.liveness.SetLastSeen(dev.DeviceID, dev.LastSeen)

	return m.saveStoreLocked()
}

// List returns all registered, non-tombstoned devices.
func (m *Manager) List() []Device {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Device, 0, len(m.devices))
	for _, dev := range m.devices {
		if !dev.Tombstoned && !m.tombstones[dev.DeviceID] {
			if ls := m.liveness.LastSeen(dev.DeviceID); !ls.IsZero() {
				dev.LastSeen = ls
			}
			out = append(out, dev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

// Get returns the device record for deviceID.
func (m *Manager) Get(deviceID string) (Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.tombstones[deviceID] {
		return Device{}, ErrDeviceTombstoned
	}
	dev, ok := m.devices[deviceID]
	if !ok || dev.Tombstoned {
		return Device{}, ErrDeviceNotFound
	}
	if ls := m.liveness.LastSeen(deviceID); !ls.IsZero() {
		dev.LastSeen = ls
	}
	return dev, nil
}

// IsTombstoned reports whether deviceID is tombstoned.
func (m *Manager) IsTombstoned(deviceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tombstones[deviceID]
}

// safeToken guards every on-disk path construction from caller-controlled
// identifiers: only the device-slug grammar (rails.go) may ever be joined
// into a filesystem path, which also excludes rails the vocabulary accepts
// nowhere else, since all vocabulary names match it.
func safeToken(kind, tok string) error {
	if !deviceIDRegexp.MatchString(tok) {
		return fmt.Errorf("%w: unsafe %s %q for path construction", ErrInvalidDeviceID, kind, tok)
	}
	return nil
}

// Forget removes the device directory tree completely and tombstones the row per DEVICE-RAILS-SPEC Wipe-on-forget.
func (m *Manager) Forget(deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := safeToken("device id", deviceID); err != nil {
		return err
	}

	// 1. Remove directory tree completely
	devTree := filepath.Join(m.devicesDir, deviceID)
	if err := os.RemoveAll(devTree); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wipe device tree %s: %w", devTree, err)
	}

	// 2. Mark tombstoned
	delete(m.devices, deviceID)
	m.tombstones[deviceID] = true
	delete(m.railSeqs, deviceID)
	delete(m.railSyncs, deviceID)

	// 3. Clear liveness and timers
	m.liveness.Cancel(deviceID)

	// 4. Persist store
	return m.saveStoreLocked()
}

// SetRailEnabled card-toggles a rail's enabled state per DEVICE-RAILS-SPEC D4 / V-T3e.
func (m *Manager) SetRailEnabled(deviceID, rail string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dev, ok := m.devices[deviceID]
	if !ok || dev.Tombstoned || m.tombstones[deviceID] {
		return ErrDeviceNotFound
	}

	found := false
	for i, r := range dev.RailsDeclared {
		if r.Name == rail {
			en := enabled
			dev.RailsDeclared[i].Enabled = &en
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: rail %q not declared on device %s", ErrRailNotFound, rail, deviceID)
	}

	m.devices[deviceID] = dev
	return m.saveStoreLocked()
}

// SetOnline updates the device online state.
func (m *Manager) SetOnline(deviceID string, online bool) {
	m.liveness.SetOnline(deviceID, online)
	if online {
		m.liveness.SetLastSeen(deviceID, m.clock.Now())
	}
}

// IsOnline reports whether device is online (including during active 30s grace window).
func (m *Manager) IsOnline(deviceID string) bool {
	return m.liveness.IsOnline(deviceID)
}

// SetLastSeen updates last seen timestamp for a device.
func (m *Manager) SetLastSeen(deviceID string, t time.Time) {
	m.liveness.SetLastSeen(deviceID, t)
}

// LastSeen returns last seen timestamp for a device.
func (m *Manager) LastSeen(deviceID string) time.Time {
	return m.liveness.LastSeen(deviceID)
}

// SetActionBroker attaches the ActionBroker for live dispatch.
func (m *Manager) SetActionBroker(broker ActionBroker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broker = broker
}

// ActionBroker returns the current ActionBroker.
func (m *Manager) ActionBroker() ActionBroker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.broker
}

// SetGracePeriod adjusts the grace window duration (used in tests).
func (m *Manager) SetGracePeriod(d time.Duration) {
	m.liveness.SetGracePeriod(d)
}

// Clock returns the Manager's clock.
func (m *Manager) Clock() Clock {
	return m.clock
}

// ApplySync processes an incremental or snapshot sync push per DEVICE-RAILS-SPEC D6 & Rail Data At Rest.
// Enforces monotonic per-rail seq (stale seq returns ErrStaleSeq with no write).
func (m *Manager) ApplySync(deviceID, rail string, snapshot []byte, deltas []Delta, seq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dev, ok := m.devices[deviceID]
	if !ok || dev.Tombstoned || m.tombstones[deviceID] {
		return ErrDeviceNotFound
	}

	// Verify rail declared and enabled
	var targetRail *DeclaredRail
	for _, r := range dev.RailsDeclared {
		if r.Name == rail {
			targetRail = &r
			break
		}
	}
	if targetRail == nil {
		return fmt.Errorf("%w: rail %q not declared for device %s", ErrRailNotFound, rail, deviceID)
	}
	if !targetRail.IsEnabled() {
		return fmt.Errorf("%w: rail %q is disabled", ErrRailDisabled, rail)
	}

	// Read existing doc to enforce monotonic seq across restarts
	if err := safeToken("device id", deviceID); err != nil {
		return err
	}
	if err := safeToken("rail", rail); err != nil {
		return err
	}
	railFile := filepath.Join(m.devicesDir, deviceID, rail+".json")
	var existingDoc RailDataDoc
	if raw, err := os.ReadFile(railFile); err == nil {
		_ = json.Unmarshal(raw, &existingDoc)
	}

	curSeq := existingDoc.Seq
	if mSeqs, ok := m.railSeqs[deviceID]; ok {
		if s, ok := mSeqs[rail]; ok && s > curSeq {
			curSeq = s
		}
	}

	if seq <= curSeq {
		return &ErrStaleSeq{
			DeviceID: deviceID,
			Rail:     rail,
			Current:  curSeq,
			Proposed: seq,
		}
	}

	// Apply retention and merge deltas
	now := m.clock.Now()
	prunedItems, err := applyRailRetention(rail, existingDoc.Items, snapshot, deltas, now)
	if err != nil {
		return fmt.Errorf("apply retention for %s/%s: %w", deviceID, rail, err)
	}

	// Persist atomically to <hearth>/devices/<device_id>/<rail>.json (mode 0600)
	doc := RailDataDoc{
		Seq:      seq,
		LastSync: now,
		Items:    prunedItems,
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal rail doc: %w", err)
	}
	data = append(data, '\n')

	if err := safeToken("device id", deviceID); err != nil {
		return err
	}
	if err := safeToken("rail", rail); err != nil {
		return err
	}
	devDir := filepath.Join(m.devicesDir, deviceID)
	if err := writeAtomic(devDir, rail+".json", data); err != nil {
		return fmt.Errorf("write rail store %s/%s: %w", deviceID, rail, err)
	}

	// Update cached state
	if m.railSeqs[deviceID] == nil {
		m.railSeqs[deviceID] = make(map[string]int64)
	}
	m.railSeqs[deviceID][rail] = seq

	if m.railSyncs[deviceID] == nil {
		m.railSyncs[deviceID] = make(map[string]time.Time)
	}
	m.railSyncs[deviceID][rail] = now

	m.liveness.SetLastSeen(deviceID, now)
	return nil
}

// Read returns stored rail items + last_sync lag per DEVICE-RAILS-SPEC D6.
func (m *Manager) Read(deviceID, rail string) ([]byte, time.Duration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.tombstones[deviceID] {
		return nil, 0, ErrDeviceTombstoned
	}
	dev, ok := m.devices[deviceID]
	if !ok || dev.Tombstoned {
		return nil, 0, ErrDeviceNotFound
	}

	var targetRail *DeclaredRail
	for _, r := range dev.RailsDeclared {
		if r.Name == rail {
			targetRail = &r
			break
		}
	}
	if targetRail == nil {
		return nil, 0, ErrRailNotFound
	}
	if !targetRail.IsEnabled() {
		return nil, 0, ErrRailDisabled
	}

	if err := safeToken("device id", deviceID); err != nil {
		return nil, 0, err
	}
	if err := safeToken("rail", rail); err != nil {
		return nil, 0, err
	}
	railFile := filepath.Join(m.devicesDir, deviceID, rail+".json")
	raw, err := os.ReadFile(railFile)
	if err != nil {
		return nil, 0, err
	}

	var doc RailDataDoc
	if err := json.Unmarshal(raw, &doc); err == nil && len(doc.Items) > 0 {
		lag := m.clock.Now().Sub(doc.LastSync)
		if lag < 0 {
			lag = 0
		}
		return doc.Items, lag, nil
	}

	// Raw JSON fallback if file was written without envelope
	fi, _ := os.Stat(railFile)
	var lag time.Duration
	if fi != nil {
		lag = m.clock.Now().Sub(fi.ModTime())
		if lag < 0 {
			lag = 0
		}
	}
	return raw, lag, nil
}

// writeAtomic writes data to dir/filename atomically via temp+rename+fsync, mode 0600.
// Follows writeSyncTempNamed pattern (internal/custos/vault.go:36-68).
func writeAtomic(dir, filename string, data []byte) error {
	// Path-injection barrier: filenames are constructed from validated
	// vocabulary tokens, but enforce single-segment here as well.
	if filepath.Base(filename) != filename || filename == "." || filename == ".." {
		return fmt.Errorf("unsafe filename %q", filename)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	pattern := filename + "-*.tmp"
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}

	target := filepath.Join(dir, filename)
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename temp to %s: %w", target, err)
	}
	cleanup = false

	df, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %s for fsync: %w", dir, err)
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}
