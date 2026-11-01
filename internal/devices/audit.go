// Package devices implements the device rails core registry, stores, and tools per DEVICE-RAILS-SPEC.
package devices

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/lararium-app/lararium/internal/custos"
)

var (
	// phonePattern matches international or local telephone number formats with at least 7 digits.
	phonePattern = regexp.MustCompile(`(?:\+?\d{1,3}[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}|\+?\d{7,15}`)

	// tokenPattern matches canary tokens, bearer tokens, or secret needles.
	tokenPattern = regexp.MustCompile(`(?i)(canary[-_a-zA-Z0-9]+|secret[-_a-zA-Z0-9]+|needle[-_a-zA-Z0-9]+|lararium_[-_a-zA-Z0-9]+|bearer\s+[-_a-zA-Z0-9]+|[a-zA-Z0-9+/=_-]{32,})`)

	// emailPattern matches email addresses.
	emailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

	// ipPattern matches IPv4 addresses.
	ipPattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// DeviceAuditPayload contains fields allowed for device rails audit logging per DEVICE-RAILS-SPEC D8 / R5.
// Emitters may only record device_id, display_name, rail names, fingerprint, credential NAME —
// never tokens, phone numbers, addresses, or rail payload data.
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

// closedAuditKinds lists the 4 closed device audit kinds.
var closedAuditKinds = map[string]bool{
	custos.AuditKindDeviceRegistered:         true,
	custos.AuditKindDeviceForgotten:          true,
	custos.AuditKindDeviceRailToggled:        true,
	custos.AuditKindDeviceRegistrationDenied: true,
}

// BuildDeviceAuditRecord constructs a validated custos.AuditRecord.
// It strictly rejects any canary tokens, phone numbers, addresses, or rail payload data.
func BuildDeviceAuditRecord(kind string, p DeviceAuditPayload) (custos.AuditRecord, error) {
	if !closedAuditKinds[kind] {
		return custos.AuditRecord{}, fmt.Errorf("%w: unrecognized device audit kind %q", ErrForbiddenAuditData, kind)
	}

	// Validate device_id
	if err := ValidateDeviceID(p.DeviceID); err != nil {
		return custos.AuditRecord{}, fmt.Errorf("%w: invalid device_id %q: %w", ErrForbiddenAuditData, p.DeviceID, err)
	}

	// Validate rail names (must only be vocabulary rails)
	for _, r := range p.RailNames {
		if _, ok := closedRails[r]; !ok {
			return custos.AuditRecord{}, fmt.Errorf("%w: forbidden rail name %q in audit record", ErrForbiddenAuditData, r)
		}
	}

	// Reject canary tokens, phone numbers, addresses, or JSON payload fragments in display name, cred name, or reason
	for label, text := range map[string]string{
		"display_name": p.DisplayName,
		"cred_name":    p.CredName,
		"reason":       p.Reason,
	} {
		if text == "" {
			continue
		}
		if phonePattern.MatchString(text) {
			return custos.AuditRecord{}, fmt.Errorf("%w: %s contains phone number", ErrForbiddenAuditData, label)
		}
		if tokenPattern.MatchString(text) {
			return custos.AuditRecord{}, fmt.Errorf("%w: %s contains token/canary", ErrForbiddenAuditData, label)
		}
		if emailPattern.MatchString(text) || ipPattern.MatchString(text) {
			return custos.AuditRecord{}, fmt.Errorf("%w: %s contains network address", ErrForbiddenAuditData, label)
		}
		if strings.ContainsAny(text, "{}[]\"") {
			return custos.AuditRecord{}, fmt.Errorf("%w: %s contains payload data", ErrForbiddenAuditData, label)
		}
	}

	actor := p.Actor
	if actor == "" {
		actor = "surface"
	}

	return custos.AuditRecord{
		Kind:        kind,
		DeviceID:    p.DeviceID,
		DisplayName: p.DisplayName,
		Fingerprint: p.Fingerprint,
		Cred:        p.CredName,
		Names:       p.RailNames,
		Actor:       actor,
		Via:         p.Via,
		Reason:      p.Reason,
	}, nil
}

// ScrubDeviceAuditPayload scrubs sensitive tokens, phone numbers, and addresses, replacing them with [REDACTED].
func ScrubDeviceAuditPayload(p DeviceAuditPayload) DeviceAuditPayload {
	scrub := func(s string) string {
		s = phonePattern.ReplaceAllString(s, "[REDACTED]")
		s = tokenPattern.ReplaceAllString(s, "[REDACTED]")
		s = emailPattern.ReplaceAllString(s, "[REDACTED]")
		s = ipPattern.ReplaceAllString(s, "[REDACTED]")
		return s
	}

	var safeRails []string
	for _, r := range p.RailNames {
		if _, ok := closedRails[r]; ok {
			safeRails = append(safeRails, r)
		} else {
			safeRails = append(safeRails, "[REDACTED]")
		}
	}

	return DeviceAuditPayload{
		DeviceID:    p.DeviceID,
		DisplayName: scrub(p.DisplayName),
		Fingerprint: p.Fingerprint,
		CredName:    scrub(p.CredName),
		RailNames:   safeRails,
		Actor:       p.Actor,
		Via:         p.Via,
		Reason:      scrub(p.Reason),
	}
}
