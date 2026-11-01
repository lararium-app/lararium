package devices

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/router"
)

type mockBroker struct {
	called     bool
	lastDevice string
	lastTool   string
	lastArgs   string
	resp       json.RawMessage
	err        error
}

func (b *mockBroker) Dispatch(_ context.Context, deviceID, tool string, args json.RawMessage) (json.RawMessage, error) {
	b.called = true
	b.lastDevice = deviceID
	b.lastTool = tool
	b.lastArgs = string(args)
	return b.resp, b.err
}

type mockPolicyDecider struct {
	verdicts map[string]string
}

func (p *mockPolicyDecider) Decide(connector, tool string) string {
	key := connector + "/" + tool
	if v, ok := p.verdicts[key]; ok {
		return v
	}
	return custos.VerdictAsk
}

func TestToolSourceAndReadExecution(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID:    "pixel-tool",
		DisplayName: "Pixel Tool",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailDialer, ReadOnly: false},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Sync calendar data
	eventsData := `[{"title":"Dentist","start":"2026-11-01T17:00:00Z"}]`
	if err := mgr.ApplySync("pixel-tool", RailCalendar, []byte(eventsData), nil, 1); err != nil {
		t.Fatalf("ApplySync: %v", err)
	}

	toolSrc := mgr.ToolSource()
	tools := toolSrc()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	var calTool, dialTool *router.ToolSpec
	for _, tool := range tools {
		if tool.Spec.Name == "mcp__pixel-tool__calendar_list" {
			s := tool.Spec
			calTool = &s
			if !tool.Trusted {
				t.Fatalf("read rail tool should be trusted (no approval card)")
			}
			if tool.FullArgs {
				t.Fatalf("read rail tool does not need FullArgs")
			}
		}
		if tool.Spec.Name == "mcp__pixel-tool__dialer_call" {
			s := tool.Spec
			dialTool = &s
			if tool.Trusted {
				t.Fatalf("action rail tool should be untrusted by default")
			}
			if !tool.FullArgs {
				t.Fatalf("action rail tool MUST carry FullArgs: true")
			}
		}
	}
	if calTool == nil || dialTool == nil {
		t.Fatalf("missing expected tools: calTool=%v dialTool=%v", calTool, dialTool)
	}

	// Execute read tool while ONLINE
	mgr.SetOnline("pixel-tool", true)
	calLoopTool := tools[0]
	if calLoopTool.Spec.Name != "mcp__pixel-tool__calendar_list" {
		calLoopTool = tools[1]
	}

	resOnline, err := calLoopTool.Run(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Run cal while online: %v", err)
	}
	if !strings.Contains(resOnline, "Dentist") {
		t.Fatalf("resOnline = %q, want content containing Dentist", resOnline)
	}

	// Advance clock by 10 minutes and execute while OFFLINE
	clk.Advance(10 * time.Minute)
	mgr.SetGracePeriod(1 * time.Millisecond)
	mgr.SetOnline("pixel-tool", false)
	time.Sleep(5 * time.Millisecond) // expire grace

	resOffline, err := calLoopTool.Run(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Run cal while offline: %v", err)
	}

	expectedMarker := "[stale: phone offline since 2026-11-01T16:00:00Z, lag 10m0s]"
	if !strings.HasPrefix(resOffline, expectedMarker) {
		t.Fatalf("resOffline lacks staleness marker: %q", resOffline)
	}
	if !strings.Contains(resOffline, "Dentist") {
		t.Fatalf("resOffline lacks data: %q", resOffline)
	}
}

func TestActionToolDispatchAndFastFail(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID: "pixel-action",
		RailsDeclared: []DeclaredRail{
			{Name: RailDialer, ReadOnly: false},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	broker := &mockBroker{resp: json.RawMessage(`{"status":"calling"}`)}
	mgr.SetActionBroker(broker)

	tools := mgr.ToolSource()()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	actionTool := tools[0]

	// 1. Device is offline: fast-fails ErrDeviceOffline without dispatching
	mgr.SetOnline("pixel-action", false)
	_, err = actionTool.Run(context.Background(), `{"phone_number":"+15551234567"}`)
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("expected ErrDeviceOffline, got %v", err)
	}
	if broker.called {
		t.Fatalf("broker must not be called when device offline")
	}

	// 2. Device is online, but no broker attached: fast-fails ErrDeviceOffline
	mgr.SetOnline("pixel-action", true)
	mgr.SetActionBroker(nil)
	_, err = actionTool.Run(context.Background(), `{"phone_number":"+15551234567"}`)
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("expected ErrDeviceOffline with nil broker, got %v", err)
	}

	// 3. Device online, broker attached, but schema drift (missing phone_number)
	mgr.SetActionBroker(broker)
	_, err = actionTool.Run(context.Background(), `{"invalid_arg":"123"}`)
	if !errors.Is(err, ErrSchemaDrift) {
		t.Fatalf("expected ErrSchemaDrift, got %v", err)
	}
	if broker.called {
		t.Fatalf("broker must not be called on schema drift")
	}

	// 4. Successful dispatch
	res, err := actionTool.Run(context.Background(), `{"phone_number":"+15551234567"}`)
	if err != nil {
		t.Fatalf("Run dialer: %v", err)
	}
	if !broker.called || broker.lastDevice != "pixel-action" || broker.lastTool != "dialer_call" {
		t.Fatalf("broker dispatched: called=%v dev=%s tool=%s", broker.called, broker.lastDevice, broker.lastTool)
	}
	if res != `{"status":"calling"}` {
		t.Fatalf("res = %q, want %q", res, `{"status":"calling"}`)
	}
}

func TestPolicyGatingAndAlwaysRulePattern(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID: "pixel-pol",
		RailsDeclared: []DeclaredRail{
			{Name: RailDialer, ReadOnly: false},
			{Name: RailCalendar, ReadOnly: true},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Test RulePattern reverse mapping
	pattern, ok := mgr.RulePattern("mcp__pixel-pol__dialer_call")
	if !ok || pattern != "pixel-pol/dialer_call" {
		t.Fatalf("RulePattern dialer = (%q, %v), want (pixel-pol/dialer_call, true)", pattern, ok)
	}
	patternCal, okCal := mgr.RulePattern("mcp__pixel-pol__calendar_list")
	if !okCal || patternCal != "pixel-pol/calendar_list" {
		t.Fatalf("RulePattern calendar = (%q, %v), want (pixel-pol/calendar_list, true)", patternCal, okCal)
	}
	if _, okUnknown := mgr.RulePattern("unknown_tool"); okUnknown {
		t.Fatalf("RulePattern unknown_tool returned true")
	}

	// Policy gating: Deny
	decider := &mockPolicyDecider{
		verdicts: map[string]string{
			"pixel-pol/dialer_call":   custos.VerdictDeny,
			"pixel-pol/calendar_list": custos.VerdictAuto,
		},
	}
	tools := mgr.ToolSource(decider)()
	for _, tool := range tools {
		if tool.Spec.Name == "mcp__pixel-pol__dialer_call" {
			// Deny verdict marks Trusted=true so Gate allows Run, which blocks
			if !tool.Trusted {
				t.Fatalf("expected Trusted=true on Deny tool")
			}
			_, err := tool.Run(context.Background(), `{"phone_number":"+15551234567"}`)
			if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
				t.Fatalf("expected blocked by policy error, got %v", err)
			}
		}
	}
}

func TestRailToggleDropsToolAndRejectsSync(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	clk := newMockClock(now)

	mgr, err := NewManager(dir, WithClock(clk))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	dev := Device{
		DeviceID: "pixel-toggle",
		RailsDeclared: []DeclaredRail{
			{Name: RailCalendar, ReadOnly: true},
			{Name: RailSMS, ReadOnly: false},
		},
	}
	if err := mgr.Register(dev); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 2 tools initially
	if len(mgr.ToolSource()()) != 2 {
		t.Fatalf("expected 2 tools initially")
	}

	// Disable calendar rail
	if err := mgr.SetRailEnabled("pixel-toggle", RailCalendar, false); err != nil {
		t.Fatalf("SetRailEnabled(false): %v", err)
	}

	// Calendar tool must drop from catalog
	toolsAfterDisable := mgr.ToolSource()()
	if len(toolsAfterDisable) != 1 || toolsAfterDisable[0].Spec.Name != "mcp__pixel-toggle__sms_send" {
		t.Fatalf("expected only sms tool after disable, got %d", len(toolsAfterDisable))
	}

	// Sync for disabled rail must be rejected
	err = mgr.ApplySync("pixel-toggle", RailCalendar, []byte(`[]`), nil, 1)
	if !errors.Is(err, ErrRailDisabled) {
		t.Fatalf("expected ErrRailDisabled on disabled rail sync, got %v", err)
	}

	// Re-enable calendar rail
	if err := mgr.SetRailEnabled("pixel-toggle", RailCalendar, true); err != nil {
		t.Fatalf("SetRailEnabled(true): %v", err)
	}

	// Tool restored and sync allowed
	if len(mgr.ToolSource()()) != 2 {
		t.Fatalf("expected 2 tools restored after re-enabling")
	}
	if err := mgr.ApplySync("pixel-toggle", RailCalendar, []byte(`[]`), nil, 1); err != nil {
		t.Fatalf("expected sync to succeed after re-enable, got %v", err)
	}
}
