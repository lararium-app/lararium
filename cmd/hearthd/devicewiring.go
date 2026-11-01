package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/devices"
	"github.com/lararium-app/lararium/internal/surface"
)

// deviceDoorAdapter adapts *devices.Manager to surface.DeviceDoor per DEVICE-RAILS-SPEC D1-D8.
type deviceDoorAdapter struct {
	mgr   *devices.Manager
	audit *custos.AuditLogger
}

func newDeviceDoorAdapter(mgr *devices.Manager, audit *custos.AuditLogger) surface.DeviceDoor {
	if mgr == nil {
		return nil
	}
	return &deviceDoorAdapter{
		mgr:   mgr,
		audit: audit,
	}
}

var _ surface.DeviceDoor = (*deviceDoorAdapter)(nil)

func (a *deviceDoorAdapter) ValidateID(deviceID string) error {
	return devices.ValidateDeviceID(deviceID)
}

func (a *deviceDoorAdapter) ValidateRails(rails []surface.DeclaredRail) error {
	devRails := make([]devices.DeclaredRail, len(rails))
	for i, r := range rails {
		devRails[i] = devices.DeclaredRail{
			Name:     r.Name,
			ReadOnly: r.ReadOnly,
			Enabled:  r.Enabled,
		}
	}
	err := devices.ValidateRails(devRails)
	if err != nil {
		return &surface.ErrRailMismatch{Reason: err.Error()}
	}
	return nil
}

func (a *deviceDoorAdapter) Register(deviceID, displayName string, rails []surface.DeclaredRail) error {
	devRails := make([]devices.DeclaredRail, len(rails))
	for i, r := range rails {
		devRails[i] = devices.DeclaredRail{
			Name:     r.Name,
			ReadOnly: r.ReadOnly,
			Enabled:  r.Enabled,
		}
	}
	return a.mgr.Register(devices.Device{
		DeviceID:      deviceID,
		DisplayName:   displayName,
		RailsDeclared: devRails,
	})
}

func (a *deviceDoorAdapter) List() []surface.DeviceCard {
	devs := a.mgr.List()
	out := make([]surface.DeviceCard, 0, len(devs))
	for _, dev := range devs {
		out = append(out, toSurfaceDeviceCard(a.mgr, dev))
	}
	return out
}

func (a *deviceDoorAdapter) Get(deviceID string) (surface.DeviceCard, error) {
	dev, err := a.mgr.Get(deviceID)
	if err != nil {
		if errors.Is(err, devices.ErrDeviceNotFound) || errors.Is(err, devices.ErrDeviceTombstoned) {
			return surface.DeviceCard{}, surface.ErrDeviceNotFound
		}
		return surface.DeviceCard{}, err
	}
	return toSurfaceDeviceCard(a.mgr, dev), nil
}

func toSurfaceDeviceCard(mgr *devices.Manager, dev devices.Device) surface.DeviceCard {
	rails := make([]surface.DeclaredRail, len(dev.RailsDeclared))
	hasAction := false
	for i, r := range dev.RailsDeclared {
		rails[i] = surface.DeclaredRail{
			Name:     r.Name,
			ReadOnly: r.ReadOnly,
			Enabled:  r.Enabled,
		}
		if r.IsEnabled() && devices.IsActionRail(r.Name) {
			hasAction = true
		}
	}

	online := mgr.IsOnline(dev.DeviceID)
	var state, reason string
	if online {
		if hasAction {
			state = surface.AppStateReadAct
		} else {
			state = surface.AppStateReadOnly
		}
		reason = ""
	} else {
		state = surface.AppStateNotConnected
		reason = "offline"
	}

	var lastSeen *string
	ls := mgr.LastSeen(dev.DeviceID)
	if ls.IsZero() {
		ls = dev.LastSeen
	}
	if !ls.IsZero() {
		lsStr := ls.UTC().Format(time.RFC3339)
		lastSeen = &lsStr
	}

	regAt := dev.RegisteredAt
	if regAt.IsZero() {
		regAt = time.Now().UTC()
	}

	return surface.DeviceCard{
		DeviceID:     dev.DeviceID,
		DisplayName:  dev.DisplayName,
		State:        state,
		Reason:       reason,
		Rails:        rails,
		LastSeen:     lastSeen,
		RegisteredAt: regAt.UTC().Format(time.RFC3339),
	}
}

func (a *deviceDoorAdapter) Forget(deviceID string) error {
	return a.mgr.Forget(deviceID)
}

func (a *deviceDoorAdapter) SetRailEnabled(deviceID, rail string, enabled bool) error {
	err := a.mgr.SetRailEnabled(deviceID, rail, enabled)
	if errors.Is(err, devices.ErrRailNotFound) {
		return surface.ErrRailNotFound
	}
	return err
}

func (a *deviceDoorAdapter) SetOnline(deviceID string, online bool) {
	a.mgr.SetOnline(deviceID, online)
}

func (a *deviceDoorAdapter) IsOnline(deviceID string) bool {
	return a.mgr.IsOnline(deviceID)
}

func (a *deviceDoorAdapter) LastSeen(deviceID string) time.Time {
	return a.mgr.LastSeen(deviceID)
}

func (a *deviceDoorAdapter) ApplySync(deviceID, rail string, snapshot []byte, deltas []surface.DeviceDelta, seq int64) error {
	devDeltas := make([]devices.Delta, len(deltas))
	for i, d := range deltas {
		devDeltas[i] = devices.Delta{
			Op:   d.Op,
			ID:   d.ID,
			Data: d.Data,
		}
	}
	err := a.mgr.ApplySync(deviceID, rail, snapshot, devDeltas, seq)
	if err != nil {
		var staleSeq *devices.ErrStaleSeq
		switch {
		case errors.Is(err, devices.ErrDeviceNotFound), errors.Is(err, devices.ErrDeviceTombstoned):
			return surface.ErrDeviceNotFound
		case errors.Is(err, devices.ErrRailNotFound):
			return surface.ErrRailNotFound
		case errors.Is(err, devices.ErrRailDisabled):
			return surface.ErrRailDisabled
		case errors.As(err, &staleSeq):
			return &surface.ErrStaleSeq{
				DeviceID: staleSeq.DeviceID,
				Rail:     staleSeq.Rail,
				Current:  staleSeq.Current,
				Proposed: staleSeq.Proposed,
			}
		default:
			return err
		}
	}
	return nil
}

func (a *deviceDoorAdapter) SetActionBroker(broker surface.ActionBroker) {
	if broker == nil {
		a.mgr.SetActionBroker(nil)
		return
	}
	a.mgr.SetActionBroker(&devicesActionBrokerAdapter{broker: broker})
}

type devicesActionBrokerAdapter struct {
	broker surface.ActionBroker
}

func (b *devicesActionBrokerAdapter) Dispatch(ctx context.Context, deviceID, tool string, args json.RawMessage) (json.RawMessage, error) {
	return b.broker.Dispatch(ctx, deviceID, tool, args)
}

func (a *deviceDoorAdapter) Audit(kind string, payload surface.DeviceAuditPayload) error {
	p := devices.DeviceAuditPayload{
		DeviceID:    payload.DeviceID,
		DisplayName: payload.DisplayName,
		Fingerprint: payload.Fingerprint,
		CredName:    payload.CredName,
		RailNames:   payload.RailNames,
		Actor:       payload.Actor,
		Via:         payload.Via,
		Reason:      payload.Reason,
	}
	rec, err := devices.BuildDeviceAuditRecord(kind, p)
	if err != nil {
		return err
	}
	if a.audit != nil {
		_ = a.audit.Append(rec)
	}
	return nil
}

func (a *deviceDoorAdapter) GetGracePeriod() time.Duration {
	return devices.DefaultGracePeriod
}

func (a *deviceDoorAdapter) SetGracePeriod(d time.Duration) {
	a.mgr.SetGracePeriod(d)
}
