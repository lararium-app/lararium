package devices

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRailsTruthTable(t *testing.T) {
	// Every closed vocabulary rail and flag combo
	tests := []struct {
		name    string
		rails   []DeclaredRail
		wantErr bool
		reason  string
	}{
		// Valid single read rails
		{"calendar valid", []DeclaredRail{{Name: RailCalendar, ReadOnly: true}}, false, ""},
		{"contacts valid", []DeclaredRail{{Name: RailContacts, ReadOnly: true}}, false, ""},
		{"notifications valid", []DeclaredRail{{Name: RailNotifications, ReadOnly: true}}, false, ""},
		{"calllog valid", []DeclaredRail{{Name: RailCallLog, ReadOnly: true}}, false, ""},
		{"health valid", []DeclaredRail{{Name: RailHealth, ReadOnly: true}}, false, ""},

		// Misflagged read rails (must be read_only: true)
		{"calendar misflagged", []DeclaredRail{{Name: RailCalendar, ReadOnly: false}}, true, "must be read-only"},
		{"contacts misflagged", []DeclaredRail{{Name: RailContacts, ReadOnly: false}}, true, "must be read-only"},
		{"notifications misflagged", []DeclaredRail{{Name: RailNotifications, ReadOnly: false}}, true, "must be read-only"},
		{"calllog misflagged", []DeclaredRail{{Name: RailCallLog, ReadOnly: false}}, true, "must be read-only"},
		{"health misflagged", []DeclaredRail{{Name: RailHealth, ReadOnly: false}}, true, "must be read-only"},

		// Valid action rails (must NOT be read_only)
		{"dialer valid", []DeclaredRail{{Name: RailDialer, ReadOnly: false}}, false, ""},
		{"sms valid", []DeclaredRail{{Name: RailSMS, ReadOnly: false}}, false, ""},

		// Misflagged action rails
		{"dialer misflagged", []DeclaredRail{{Name: RailDialer, ReadOnly: true}}, true, "must not be read-only"},
		{"sms misflagged", []DeclaredRail{{Name: RailSMS, ReadOnly: true}}, true, "must not be read-only"},

		// All 7 valid rails declared together
		{"all 7 rails valid", []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailContacts, ReadOnly: true},
			{Name: RailNotifications, ReadOnly: true},
			{Name: RailCallLog, ReadOnly: true},
			{Name: RailHealth, ReadOnly: true},
			{Name: RailDialer, ReadOnly: false},
			{Name: RailSMS, ReadOnly: false},
		}, false, ""},

		// Empty rail list
		{"empty list", []DeclaredRail{}, true, "empty"},

		// Duplicate rail
		{"duplicate rail", []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailCalendar, ReadOnly: true},
		}, true, "duplicate"},

		// Undeclared / non-vocabulary rail
		{"undeclared rail camera", []DeclaredRail{{Name: "camera", ReadOnly: true}}, true, "undeclared"},
		{"undeclared rail location", []DeclaredRail{{Name: "location", ReadOnly: false}}, true, "undeclared"},

		// Mixed valid and invalid
		{"mixed valid and misflag", []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailSMS, ReadOnly: true}, // misflag
		}, true, "must not be read-only"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRails(tc.rails)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				var rme *ErrRailMismatch
				if !errors.As(err, &rme) {
					t.Fatalf("expected *ErrRailMismatch, got %T: %v", err, err)
				}
				if !errors.Is(err, ErrRailMismatch{Reason: ""}) {
					t.Fatalf("errors.Is(err, ErrRailMismatch) returned false")
				}
				if rme.Reason == "" {
					t.Fatalf("expected non-empty Reason in ErrRailMismatch")
				}
				if !strings.Contains(strings.ToLower(rme.Reason), strings.ToLower(tc.reason)) {
					t.Fatalf("expected reason containing %q, got %q", tc.reason, rme.Reason)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateDeviceID(t *testing.T) {
	valid := []string{
		"a",
		"0",
		"phone",
		"pixel-8",
		"samsung_galaxy",
		"pixel_8-pro",
		"device1234567890123456789012345", // 32 chars
	}
	for _, id := range valid {
		if err := ValidateDeviceID(id); err != nil {
			t.Errorf("ValidateDeviceID(%q) = %v; want nil", id, err)
		}
	}

	invalid := []string{
		"",
		"-phone",
		"_phone",
		"Phone",
		"Pixel8",
		"pixel.8",
		"pixel 8",
		"pixel/8",
		"device123456789012345678901234567", // 33 chars
	}
	for _, id := range invalid {
		if err := ValidateDeviceID(id); err == nil {
			t.Errorf("ValidateDeviceID(%q) = nil; want error", id)
		}
	}
}

func TestToolNameCapAndFNVParity(t *testing.T) {
	// Exact parity tests matching cmd/hearthd/mcp_tools_test.go
	if got := ToolName("github-rail", "create_pr"); got != "mcp__github-rail__create_pr" {
		t.Fatalf("ToolName = %q, want mcp__github-rail__create_pr", got)
	}
	if got := ToolName("a b", "tool.x!"); got != "mcp__a_b__tool_x_" {
		t.Fatalf("ToolName = %q, want mcp__a_b__tool_x_", got)
	}

	// 64-char cap and deterministic FNV-32a truncation
	long := ToolName(strings.Repeat("c", 40), strings.Repeat("t", 40))
	if len(long) != 64 {
		t.Fatalf("len(ToolName) = %d, want 64", len(long))
	}
	if !strings.HasPrefix(long, "mcp__cccc") {
		t.Fatalf("ToolName prefix = %q", long[:8])
	}
	if long != ToolName(strings.Repeat("c", 40), strings.Repeat("t", 40)) {
		t.Fatalf("ToolName not deterministic")
	}
	other := ToolName(strings.Repeat("c", 40), strings.Repeat("t", 39)+"z")
	if other == long {
		t.Fatalf("ToolName collision on differing inputs")
	}

	// Device rail tool names under 64 chars
	if got := ToolName("pixel", "dialer_call"); got != "mcp__pixel__dialer_call" {
		t.Fatalf("ToolName = %q", got)
	}
	if got := ToolName("pixel", "sms_send"); got != "mcp__pixel__sms_send" {
		t.Fatalf("ToolName = %q", got)
	}
}

func TestValidateArgs(t *testing.T) {
	// dialer_call
	if err := ValidateArgs(ToolDialerCall, `{"phone_number":"+15551234567"}`); err != nil {
		t.Fatalf("valid dialer_call rejected: %v", err)
	}
	if err := ValidateArgs(ToolDialerCall, `{"number":"+15551234567"}`); err != nil {
		t.Fatalf("valid dialer_call alias rejected: %v", err)
	}
	if err := ValidateArgs(ToolDialerCall, `{}`); err == nil {
		t.Fatalf("dialer_call without phone_number expected error")
	}
	if err := ValidateArgs(ToolDialerCall, `{"phone_number":""}`); err == nil {
		t.Fatalf("dialer_call with empty phone_number expected error")
	}

	// sms_send
	if err := ValidateArgs(ToolSMSSend, `{"recipient":"+15551234567","message":"hello"}`); err != nil {
		t.Fatalf("valid sms_send rejected: %v", err)
	}
	if err := ValidateArgs(ToolSMSSend, `{"to":"+15551234567","body":"hello"}`); err != nil {
		t.Fatalf("valid sms_send aliases rejected: %v", err)
	}
	if err := ValidateArgs(ToolSMSSend, `{"recipient":"+15551234567"}`); err == nil {
		t.Fatalf("sms_send without message expected error")
	}
	if err := ValidateArgs(ToolSMSSend, `{"message":"hello"}`); err == nil {
		t.Fatalf("sms_send without recipient expected error")
	}

	// read tools
	if err := ValidateArgs(ToolCalendarList, `{}`); err != nil {
		t.Fatalf("valid calendar_list rejected: %v", err)
	}
	if err := ValidateArgs(ToolContactsSearch, `{"query":"Alice"}`); err != nil {
		t.Fatalf("valid contacts_search rejected: %v", err)
	}
	if err := ValidateArgs(ToolContactsSearch, `{"query":123}`); err == nil {
		t.Fatalf("contacts_search non-string query expected error")
	}
	if err := ValidateArgs(ToolCalendarList, `invalid json`); err == nil {
		t.Fatalf("malformed JSON expected error")
	}
}

func TestRailHelpers(t *testing.T) {
	if !IsReadRail(RailCalendar) || !IsReadRail(RailContacts) || !IsReadRail(RailNotifications) || !IsReadRail(RailCallLog) || !IsReadRail(RailHealth) {
		t.Fatalf("expected all 5 read rails to return IsReadRail = true")
	}
	if IsReadRail(RailDialer) || IsReadRail(RailSMS) {
		t.Fatalf("action rails must not return IsReadRail = true")
	}
	if !IsActionRail(RailDialer) || !IsActionRail(RailSMS) {
		t.Fatalf("expected dialer and sms to return IsActionRail = true")
	}

	for rail, tool := range railTools {
		gotTool, ok := ToolForRail(rail)
		if !ok || gotTool != tool {
			t.Fatalf("ToolForRail(%q) = %q, want %q", rail, gotTool, tool)
		}
		gotRail, ok := RailForTool(tool)
		if !ok || gotRail != rail {
			t.Fatalf("RailForTool(%q) = %q, want %q", tool, gotRail, rail)
		}
	}
}
