package main

import (
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/devices"
	"github.com/lararium-app/lararium/internal/surface"
)

func TestDeviceToolSourceWiringInHearthd(t *testing.T) {
	home := t.TempDir()
	devMgr, err := devices.NewManager(home)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := devices.Device{
		DeviceID: "pixel-wired",
		RailsDeclared: []devices.DeclaredRail{
			{Name: devices.RailCalendar, ReadOnly: true},
			{Name: devices.RailSMS, ReadOnly: false},
		},
	}
	if err := devMgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Sync calendar data
	_ = devMgr.ApplySync("pixel-wired", devices.RailCalendar, []byte(`[{"title":"Standup"}]`), nil, 1)

	// Set online
	devMgr.SetOnline("pixel-wired", true)

	// Register device tools
	devToolSrc := devMgr.ToolSource()
	setHearthDeviceSource(devToolSrc, devMgr)

	// Verify device tools produced
	tools := devToolSrc()
	var foundCal, foundSMS bool
	for _, tool := range tools {
		if tool.Spec.Name == "mcp__pixel-wired__calendar_list" {
			foundCal = true
			if !tool.Trusted {
				t.Fatalf("calendar tool should be trusted (read_only)")
			}
		}
		if tool.Spec.Name == "mcp__pixel-wired__sms_send" {
			foundSMS = true
			if tool.Trusted {
				t.Fatalf("sms tool should be untrusted")
			}
			if !tool.FullArgs {
				t.Fatalf("sms tool must carry FullArgs: true")
			}
		}
	}
	if !foundCal || !foundSMS {
		t.Fatalf("missing device tools: foundCal=%v foundSMS=%v", foundCal, foundSMS)
	}

	// Test case 2: Always-rule minter resolves device tool to "<device_id>/<tool>" pattern
	a := newUnlockedAdapter(t)
	srv := &surface.Server{}
	srv.SetPolicySource(a)

	hook := turnAlwaysRuleMinter(srv, nil)
	modelTool := devices.ToolName("pixel-wired", devices.ToolSMSSend)

	// Pre-condition: decides ask
	if got := a.Decide("pixel-wired", "sms_send"); got != custos.VerdictAsk {
		t.Fatalf("initial Decide = %q, want ask", got)
	}

	// Always-verdict resolves model tool to "pixel-wired/sms_send" and mints rule
	if err := hook(modelTool); err != nil {
		t.Fatalf("turnAlwaysRuleMinter failed for device tool %q: %v", modelTool, err)
	}

	// Next turn: exact pair resolves to auto!
	if got := a.Decide("pixel-wired", "sms_send"); got != custos.VerdictAuto {
		t.Fatalf("after always mint Decide = %q, want auto", got)
	}
}
