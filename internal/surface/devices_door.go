package surface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DeclaredRail represents one rail declared during device registration per DEVICE-RAILS-SPEC D4.
type DeclaredRail struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// IsEnabled reports whether the rail is currently enabled (defaults to true).
func (r DeclaredRail) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// DeviceDelta is one incremental mutation pushed during rail sync per DEVICE-RAILS-SPEC D6.
type DeviceDelta struct {
	Op   string          `json:"op,omitempty"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// DeviceCard is the app-facing device row representation per DEVICE-RAILS-SPEC D7 & Deliverable 3.
type DeviceCard struct {
	DeviceID     string         `json:"device_id"`
	DisplayName  string         `json:"display_name"`
	State        string         `json:"state"`
	Reason       string         `json:"reason"`
	Rails        []DeclaredRail `json:"rails"`
	LastSeen     *string        `json:"last_seen"`
	RegisteredAt string         `json:"registered_at"`
}

// DeviceAuditPayload contains fields allowed for device rails audit logging per DEVICE-RAILS-SPEC D8 / R5.
type DeviceAuditPayload struct {
	DeviceID    string
	DisplayName string
	Fingerprint string
	CredName    string
	RailNames   []string
	Actor       string
	Via         string
	Reason      string
}

// Device audit kind constants per DEVICE-RAILS-SPEC D8 / R5.
const (
	AuditKindDeviceRegistered         = "device_registered"
	AuditKindDeviceForgotten          = "device_forgotten"
	AuditKindDeviceRailToggled        = "device_rail_toggled"
	AuditKindDeviceRegistrationDenied = "device_registration_denied"
)

// Sentinels and typed errors for device rails on surface.
var (
	ErrDeviceOffline  = errors.New("device offline")
	ErrDeviceNotFound = errors.New("device not found")
	ErrRailNotFound   = errors.New("rail not found")
	ErrRailDisabled   = errors.New("rail disabled")
)

// ErrStaleSeq is returned when ApplySync receives a non-monotonic sequence number.
type ErrStaleSeq struct {
	DeviceID string
	Rail     string
	Current  int64
	Proposed int64
}

func (e ErrStaleSeq) Error() string {
	return fmt.Sprintf("stale seq for %s/%s: current %d, proposed %d", e.DeviceID, e.Rail, e.Current, e.Proposed)
}

// Is reports whether target matches ErrStaleSeq.
func (e ErrStaleSeq) Is(target error) bool {
	var s *ErrStaleSeq
	if errors.As(target, &s) {
		return true
	}
	_, ok := target.(ErrStaleSeq)
	return ok
}

// ErrRailMismatch is returned when declared rails violate the closed vocabulary or flags.
type ErrRailMismatch struct {
	Reason string
}

func (e ErrRailMismatch) Error() string {
	if e.Reason == "" {
		return "rail mismatch"
	}
	return fmt.Sprintf("rail mismatch: %s", e.Reason)
}

// Is reports whether target matches ErrRailMismatch.
func (e ErrRailMismatch) Is(target error) bool {
	var rme *ErrRailMismatch
	if errors.As(target, &rme) {
		return true
	}
	_, ok := target.(ErrRailMismatch)
	return ok
}

// ActionBroker dispatches an action tool call to the live paired phone stream per DEVICE-RAILS-SPEC D5/D6.
type ActionBroker interface {
	Dispatch(ctx context.Context, deviceID, tool string, args json.RawMessage) (json.RawMessage, error)
}

// DeviceDoor is the surface's view of the devices core plane, implemented in cmd/hearthd (surface never imports custody or devices core).
//
//nolint:interfacebloat // DeviceDoor mirrors all device manager and custody audit operations required by the surface layer
type DeviceDoor interface {
	ValidateID(deviceID string) error
	ValidateRails(rails []DeclaredRail) error
	Register(deviceID, displayName string, rails []DeclaredRail) error
	List() []DeviceCard
	Get(deviceID string) (DeviceCard, error)
	Forget(deviceID string) error
	SetRailEnabled(deviceID, rail string, enabled bool) error
	SetOnline(deviceID string, online bool)
	IsOnline(deviceID string) bool
	LastSeen(deviceID string) time.Time
	ApplySync(deviceID, rail string, snapshot []byte, deltas []DeviceDelta, seq int64) error
	SetActionBroker(broker ActionBroker)
	Audit(kind string, payload DeviceAuditPayload) error
	GetGracePeriod() time.Duration
	SetGracePeriod(d time.Duration)
}
