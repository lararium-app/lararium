package devices

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// deviceIDRegexp validates device_id slug per DEVICE-RAILS-SPEC D2: ^[a-z0-9][a-z0-9_-]{0,31}$.
var deviceIDRegexp = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,31}$")

// ValidateDeviceID checks that id conforms to the device_id slug grammar.
func ValidateDeviceID(id string) error {
	if !deviceIDRegexp.MatchString(id) {
		return fmt.Errorf("%w: %q does not match ^[a-z0-9][a-z0-9_-]{0,31}$", ErrInvalidDeviceID, id)
	}
	return nil
}

// closedRails defines the expected read_only flag for each rail in the closed vocabulary.
var closedRails = map[string]bool{
	RailCalendar:      true,
	RailContacts:      true,
	RailNotifications: true,
	RailCallLog:       true,
	RailHealth:        true,
	RailDialer:        false,
	RailSMS:           false,
}

// railTools maps each declared rail to its single frozen tool per DEVICE-RAILS-SPEC D4.
var railTools = map[string]string{
	RailCalendar:      ToolCalendarList,
	RailContacts:      ToolContactsSearch,
	RailNotifications: ToolNotificationsList,
	RailCallLog:       ToolCallLogSearch,
	RailHealth:        ToolHealthLatest,
	RailDialer:        ToolDialerCall,
	RailSMS:           ToolSMSSend,
}

// toolRails reverse maps a tool name to its canonical rail.
var toolRails = map[string]string{
	ToolCalendarList:      RailCalendar,
	ToolContactsSearch:    RailContacts,
	ToolNotificationsList: RailNotifications,
	ToolCallLogSearch:     RailCallLog,
	ToolHealthLatest:      RailHealth,
	ToolDialerCall:        RailDialer,
	ToolSMSSend:           RailSMS,
}

// ValidateRails validates a declared rail set against the closed rail vocabulary per DEVICE-RAILS-SPEC D4.
// Returns a typed ErrRailMismatch on undeclared rail, misflag, empty list, or duplicate.
func ValidateRails(rails []DeclaredRail) error {
	if len(rails) == 0 {
		return &ErrRailMismatch{Reason: "rail list must not be empty"}
	}
	seen := make(map[string]bool, len(rails))
	for _, r := range rails {
		if seen[r.Name] {
			return &ErrRailMismatch{Reason: fmt.Sprintf("duplicate rail %q", r.Name)}
		}
		seen[r.Name] = true

		expectedReadOnly, ok := closedRails[r.Name]
		if !ok {
			return &ErrRailMismatch{Reason: fmt.Sprintf("undeclared rail %q not in closed vocabulary", r.Name)}
		}
		if expectedReadOnly && !r.ReadOnly {
			return &ErrRailMismatch{Reason: fmt.Sprintf("read rail %q must be read-only", r.Name)}
		}
		if !expectedReadOnly && r.ReadOnly {
			return &ErrRailMismatch{Reason: fmt.Sprintf("action rail %q must not be read-only", r.Name)}
		}
	}
	return nil
}

// IsReadRail reports whether rail is a read rail in the closed vocabulary.
func IsReadRail(rail string) bool {
	ro, ok := closedRails[rail]
	return ok && ro
}

// IsActionRail reports whether rail is an action rail in the closed vocabulary.
func IsActionRail(rail string) bool {
	ro, ok := closedRails[rail]
	return ok && !ro
}

// ToolForRail returns the frozen tool name for a rail.
func ToolForRail(rail string) (string, bool) {
	t, ok := railTools[rail]
	return t, ok
}

// RailForTool returns the canonical rail for a frozen tool.
func RailForTool(tool string) (string, bool) {
	r, ok := toolRails[tool]
	return r, ok
}

// ToolName maps a device+tool pair onto a model-facing name that
// satisfies the strictest provider grammar (^[a-zA-Z0-9_-]{1,64}$).
// It uses the exact FNV-32a truncation at 64 characters used by cmd/hearthd/mcpwiring.go.
func ToolName(connector, tool string) string {
	name := "mcp__" + connector + "__" + tool
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	const maxName = 64
	if len(s) > maxName {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		suffix := fmt.Sprintf("_%08x", h.Sum32())
		s = s[:maxName-len(suffix)] + suffix
	}
	return s
}

// ToolSchema returns the JSON schema string for a device tool.
func ToolSchema(tool string) string {
	switch tool {
	case ToolCalendarList:
		return `{"type":"object","properties":{}}`
	case ToolContactsSearch:
		return `{"type":"object","properties":{"query":{"type":"string","description":"search query for contacts"}}}`
	case ToolNotificationsList:
		return `{"type":"object","properties":{}}`
	case ToolCallLogSearch:
		return `{"type":"object","properties":{"query":{"type":"string","description":"search query for call log entries"}}}`
	case ToolHealthLatest:
		return `{"type":"object","properties":{}}`
	case ToolDialerCall:
		return `{"type":"object","properties":{"phone_number":{"type":"string","description":"phone number to dial"}},"required":["phone_number"]}`
	case ToolSMSSend:
		return `{"type":"object","properties":{"recipient":{"type":"string","description":"recipient phone number"},"message":{"type":"string","description":"SMS message body"}},"required":["recipient","message"]}`
	default:
		return `{"type":"object"}`
	}
}

// ToolDescription returns a model-facing description for a device tool.
func ToolDescription(deviceID, tool string) string {
	switch tool {
	case ToolCalendarList:
		return fmt.Sprintf("List upcoming calendar events synced from device %s.", deviceID)
	case ToolContactsSearch:
		return fmt.Sprintf("Search address book contacts synced from device %s.", deviceID)
	case ToolNotificationsList:
		return fmt.Sprintf("List recent notifications synced from device %s.", deviceID)
	case ToolCallLogSearch:
		return fmt.Sprintf("Search recent call log entries synced from device %s.", deviceID)
	case ToolHealthLatest:
		return fmt.Sprintf("Retrieve latest health and fitness metrics synced from device %s.", deviceID)
	case ToolDialerCall:
		return fmt.Sprintf("Place a phone call on device %s to the specified phone number.", deviceID)
	case ToolSMSSend:
		return fmt.Sprintf("Send an SMS text message on device %s to recipient.", deviceID)
	default:
		return fmt.Sprintf("Device %s tool %s.", deviceID, tool)
	}
}

// ValidateArgs validates tool call arguments against the frozen tool input schema.
// Drift rejects execution without dispatching to the action broker.
func ValidateArgs(tool string, argsJSON string) error {
	raw := strings.TrimSpace(argsJSON)
	if raw == "" {
		raw = "{}"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return fmt.Errorf("%w: invalid JSON object: %w", ErrSchemaDrift, err)
	}

	switch tool {
	case ToolDialerCall:
		pn, hasPN := m["phone_number"]
		if !hasPN {
			// Also check "number" as common alias
			pn, hasPN = m["number"]
		}
		if !hasPN {
			return fmt.Errorf("%w: dialer_call missing required 'phone_number'", ErrSchemaDrift)
		}
		pnStr, ok := pn.(string)
		if !ok || strings.TrimSpace(pnStr) == "" {
			return fmt.Errorf("%w: dialer_call 'phone_number' must be a non-empty string", ErrSchemaDrift)
		}

	case ToolSMSSend:
		rec, hasRec := m["recipient"]
		if !hasRec {
			rec, hasRec = m["to"]
		}
		if !hasRec {
			return fmt.Errorf("%w: sms_send missing required 'recipient'", ErrSchemaDrift)
		}
		recStr, ok := rec.(string)
		if !ok || strings.TrimSpace(recStr) == "" {
			return fmt.Errorf("%w: sms_send 'recipient' must be a non-empty string", ErrSchemaDrift)
		}

		msg, hasMsg := m["message"]
		if !hasMsg {
			msg, hasMsg = m["body"]
		}
		if !hasMsg {
			return fmt.Errorf("%w: sms_send missing required 'message'", ErrSchemaDrift)
		}
		msgStr, ok := msg.(string)
		if !ok || strings.TrimSpace(msgStr) == "" {
			return fmt.Errorf("%w: sms_send 'message' must be a non-empty string", ErrSchemaDrift)
		}

	case ToolCalendarList, ToolNotificationsList, ToolHealthLatest:
		// No required parameters; JSON object already checked

	case ToolContactsSearch, ToolCallLogSearch:
		// Optional query; if provided must be a string
		if q, ok := m["query"]; ok && q != nil {
			if _, isStr := q.(string); !isStr {
				return fmt.Errorf("%w: %s 'query' must be a string", ErrSchemaDrift, tool)
			}
		}
	}
	return nil
}
