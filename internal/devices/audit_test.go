package devices

import (
	"errors"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

func TestAuditCanaryAndPhoneRejection(t *testing.T) {
	// Baseline valid payload
	validPayload := DeviceAuditPayload{
		DeviceID:    "pixel-8",
		DisplayName: "My Pixel 8",
		Fingerprint: "fp-abc12345",
		CredName:    "pairing-token-slot",
		RailNames:   []string{RailCalendar, RailSMS},
	}

	rec, err := BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, validPayload)
	if err != nil {
		t.Fatalf("valid BuildDeviceAuditRecord failed: %v", err)
	}
	if rec.DeviceID != "pixel-8" || rec.DisplayName != "My Pixel 8" || len(rec.Names) != 2 {
		t.Fatalf("unexpected record fields: %+v", rec)
	}

	// 1. Canary token in DisplayName -> must be rejected
	canaryPayload := validPayload
	canaryPayload.DisplayName = "Pixel with canary-token-xyz987"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, canaryPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on canary token, got %v", err)
	}

	// 2. Secret needle in Reason -> must be rejected
	needlePayload := validPayload
	needlePayload.Reason = "secret_needle_value_here"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistrationDenied, needlePayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on secret needle, got %v", err)
	}

	// 3. Bearer token in CredName -> must be rejected
	bearerPayload := validPayload
	bearerPayload.CredName = "bearer-secret-tok-12345"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, bearerPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on bearer token in cred, got %v", err)
	}

	// 4. Phone number in DisplayName -> must be rejected
	phonePayload := validPayload
	phonePayload.DisplayName = "Alice +1-555-867-5309"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, phonePayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on phone number in DisplayName, got %v", err)
	}

	phonePayload2 := validPayload
	phonePayload2.DisplayName = "Alice +15558675309"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, phonePayload2)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on raw international phone number, got %v", err)
	}

	// 5. Email address in DisplayName -> must be rejected
	emailPayload := validPayload
	emailPayload.DisplayName = "alice@example.com Phone"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, emailPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on email address, got %v", err)
	}

	// 6. IP address in DisplayName -> must be rejected
	ipPayload := validPayload
	ipPayload.DisplayName = "Device at 192.168.1.100"
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, ipPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on IP address, got %v", err)
	}

	// 7. Rail payload JSON in DisplayName -> must be rejected
	jsonPayload := validPayload
	jsonPayload.DisplayName = `{"notification":"hello"}`
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, jsonPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on JSON payload, got %v", err)
	}

	// 8. Undeclared rail name in RailNames -> must be rejected
	badRailPayload := validPayload
	badRailPayload.RailNames = []string{"camera", "contacts"}
	_, err = BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, badRailPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on undeclared rail name, got %v", err)
	}

	// 9. Unclosed audit kind -> must be rejected
	_, err = BuildDeviceAuditRecord("unauthorized_audit_kind", validPayload)
	if !errors.Is(err, ErrForbiddenAuditData) {
		t.Fatalf("expected ErrForbiddenAuditData on unknown audit kind, got %v", err)
	}
}

func TestAuditScrubber(t *testing.T) {
	dirty := DeviceAuditPayload{
		DeviceID:    "pixel-8",
		DisplayName: "Bob phone +1-555-123-4567 with canary_token_999 at 10.0.0.1",
		Fingerprint: "fp-xyz",
		CredName:    "bearer_secret_tok",
		RailNames:   []string{RailCalendar, "unknown_secret_rail"},
		Reason:      "contact bob@example.com for secret_canary",
	}

	clean := ScrubDeviceAuditPayload(dirty)

	if strings.Contains(clean.DisplayName, "+1-555-123-4567") {
		t.Fatalf("DisplayName phone not scrubbed: %s", clean.DisplayName)
	}
	if strings.Contains(clean.DisplayName, "canary_token_999") {
		t.Fatalf("DisplayName canary token not scrubbed: %s", clean.DisplayName)
	}
	if strings.Contains(clean.DisplayName, "10.0.0.1") {
		t.Fatalf("DisplayName IP not scrubbed: %s", clean.DisplayName)
	}
	if strings.Contains(clean.CredName, "secret") {
		t.Fatalf("CredName secret not scrubbed: %s", clean.CredName)
	}
	if strings.Contains(clean.Reason, "bob@example.com") {
		t.Fatalf("Reason email not scrubbed: %s", clean.Reason)
	}
	if strings.Contains(clean.Reason, "canary") {
		t.Fatalf("Reason canary not scrubbed: %s", clean.Reason)
	}
	if len(clean.RailNames) != 2 || clean.RailNames[1] != "[REDACTED]" {
		t.Fatalf("non-vocabulary rail was not redacted: %+v", clean.RailNames)
	}

	// Now the scrubbed payload can be safely built (or verified)
	clean.DisplayName = "Bob Phone"
	clean.CredName = "pairing-token"
	clean.Reason = "user requested"
	clean.RailNames = []string{RailCalendar}

	rec, err := BuildDeviceAuditRecord(custos.AuditKindDeviceRegistered, clean)
	if err != nil {
		t.Fatalf("clean payload after scrubbing failed build: %v", err)
	}
	if rec.DeviceID != "pixel-8" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}
