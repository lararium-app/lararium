package devices

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

// Device represents one registered device record per DEVICE-RAILS-SPEC D3.
// Persisted at <hearth>/custos/devices.json.
type Device struct {
	DeviceID      string         `json:"device_id"`
	DisplayName   string         `json:"display_name"`
	RailsDeclared []DeclaredRail `json:"rails_declared"`
	RegisteredAt  time.Time      `json:"registered_at"`
	LastSeen      time.Time      `json:"last_seen"`
	SyncEpoch     int64          `json:"sync_epoch"`
	Tombstoned    bool           `json:"tombstoned,omitempty"`
}

// Delta is one incremental mutation pushed during rail sync per DEVICE-RAILS-SPEC D6.
type Delta struct {
	Op   string          `json:"op,omitempty"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ActionBroker dispatches an action tool call to the live paired phone stream per DEVICE-RAILS-SPEC D5/D6.
type ActionBroker interface {
	Dispatch(ctx context.Context, deviceID, tool string, args json.RawMessage) (json.RawMessage, error)
}

// PolicyDecider evaluates worker lane policy for device tools per DEVICE-RAILS-SPEC D5.
type PolicyDecider interface {
	Decide(connector, tool string) string
}

// Sentinels and typed errors for device rails.
var (
	// ErrDeviceOffline is returned when an action tool is called while the device is offline or unattached.
	ErrDeviceOffline = errors.New("device offline")

	// ErrInvalidDeviceID is returned when device_id fails the slug grammar ^[a-z0-9][a-z0-9_-]{0,31}$.
	ErrInvalidDeviceID = errors.New("invalid device_id slug")

	// ErrDeviceNotFound is returned when operating on a nonexistent device.
	ErrDeviceNotFound = errors.New("device not found")

	// ErrDeviceTombstoned is returned when operating on a tombstoned device.
	ErrDeviceTombstoned = errors.New("device tombstoned")

	// ErrSchemaDrift is returned when arguments do not match the frozen tool schema.
	ErrSchemaDrift = errors.New("schema drift: arguments do not match tool schema")

	// ErrRailDisabled is returned when attempting to sync or read a disabled rail.
	ErrRailDisabled = errors.New("rail disabled")

	// ErrRailNotFound is returned when requesting a rail that is not declared on the device.
	ErrRailNotFound = errors.New("rail not found")

	// ErrForbiddenAuditData is returned when audit payload contains canary tokens, phone numbers, addresses, or payload data.
	ErrForbiddenAuditData = errors.New("forbidden audit data: contains token, phone number, address, or payload data")
)

// ErrRailMismatch is returned by ValidateRails when declared rails violate the closed vocabulary or flags.
type ErrRailMismatch struct {
	Reason string
}

func (e ErrRailMismatch) Error() string {
	if e.Reason == "" {
		return "rail mismatch"
	}
	return fmt.Sprintf("rail mismatch: %s", e.Reason)
}

// Is supports errors.Is comparison for ErrRailMismatch.
func (e ErrRailMismatch) Is(target error) bool {
	var rme *ErrRailMismatch
	if errors.As(target, &rme) {
		return true
	}
	_, ok := target.(ErrRailMismatch)
	return ok
}

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

// Is supports errors.Is comparison for ErrStaleSeq.
func (e ErrStaleSeq) Is(target error) bool {
	var s *ErrStaleSeq
	if errors.As(target, &s) {
		return true
	}
	_, ok := target.(ErrStaleSeq)
	return ok
}

// Closed rail vocabulary constants per DEVICE-RAILS-SPEC D4.
const (
	RailCalendar      = "calendar"
	RailContacts      = "contacts"
	RailNotifications = "notifications"
	RailCallLog       = "calllog"
	RailHealth        = "health"
	RailDialer        = "dialer"
	RailSMS           = "sms"
)

// Frozen tool names per rail (one tool per rail) per DEVICE-RAILS-SPEC D4.
const (
	ToolCalendarList      = "calendar_list"
	ToolContactsSearch    = "contacts_search"
	ToolNotificationsList = "notifications_list"
	ToolCallLogSearch     = "calllog_search"
	ToolHealthLatest      = "health_latest"
	ToolDialerCall        = "dialer_call"
	ToolSMSSend           = "sms_send"
)
