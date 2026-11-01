package surface

import (
	"sort"
	"time"
)

type pendingDevice struct {
	deviceID     string
	displayName  string
	rails        []DeclaredRail
	cardID       string
	registeredAt time.Time
	fingerprint  string
}

// deviceRows snapshots all registered and pending device records, deriving their app-facing state per DEVICE-RAILS-SPEC D7.
func (s *Server) deviceRows() []DeviceCard {
	door := s.getDeviceDoor()

	var out []DeviceCard
	committed := make(map[string]bool)

	if door != nil {
		out = append(out, door.List()...)
		for _, card := range out {
			committed[card.DeviceID] = true
		}
	}

	// Add pending rows that are not yet committed per connectors_row.go pattern
	s.devicePendingMu.Lock()
	for id, p := range s.devicePending {
		if !committed[id] {
			rails := p.rails
			if rails == nil {
				rails = []DeclaredRail{}
			}
			regAt := p.registeredAt
			if regAt.IsZero() {
				regAt = time.Now().UTC()
			}
			out = append(out, DeviceCard{
				DeviceID:     p.deviceID,
				DisplayName:  p.displayName,
				State:        AppStateNotConnected,
				Reason:       connReasonPendingAppvl,
				Rails:        rails,
				LastSeen:     nil,
				RegisteredAt: regAt.UTC().Format(time.RFC3339),
			})
		}
	}
	s.devicePendingMu.Unlock()

	if out == nil {
		out = []DeviceCard{}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}
