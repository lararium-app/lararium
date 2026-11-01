package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/devices"
	"github.com/lararium-app/lararium/internal/loop"
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

func TestDeviceDoorWiringAndActionLoopInHearthd(t *testing.T) {
	home := t.TempDir()
	store, err := surface.OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("test-token")
	if err != nil {
		t.Fatal(err)
	}

	srv := &surface.Server{
		Store: store,
		Cfg:   surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	approvalHub := surface.NewApprovalHub(time.Minute)
	srv.SetApprovalHub(approvalHub)

	wireDevices(home, nil, nil, nil, srv)

	if srv.DeviceBroker() == nil {
		t.Fatalf("DeviceBroker is nil after wireDevices")
	}

	h := surface.NewServer(srv)

	doReq := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// 1. Register device via API
	regBody := `{"device_id":"pixel-loop","display_name":"Loop Pixel","rails_declared":[{"name":"sms","read_only":false}]}`
	regW := doReq(http.MethodPost, "/v1/devices/register", regBody)
	if regW.Code != http.StatusAccepted {
		t.Fatalf("register status = %d: %s", regW.Code, regW.Body.String())
	}
	var regRes struct {
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal(regW.Body.Bytes(), &regRes); err != nil {
		t.Fatal(err)
	}

	// Approve card
	n := approvalHub.Resolve("", regRes.CardID, true)
	if n == 0 {
		t.Fatalf("resolve card failed: n=0")
	}

	// Wait for registration to complete in devices store
	deadline := time.Now().Add(3 * time.Second)
	var registered bool
	for time.Now().Before(deadline) {
		rowW := doReq(http.MethodGet, "/v1/devices", "")
		if strings.Contains(rowW.Body.String(), `"pixel-loop"`) && !strings.Contains(rowW.Body.String(), `"pending_approval"`) {
			registered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !registered {
		t.Fatalf("device did not complete registration")
	}

	// 2. Open SSE stream
	ctxStream, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()

	streamReq := httptest.NewRequest(http.MethodGet, "/v1/devices/pixel-loop/stream", nil).WithContext(ctxStream)
	streamReq.Host = "127.0.0.1:7717"
	streamReq.Header.Set("Authorization", "Bearer "+tok)
	streamW := newSafeRecorder()

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		h.ServeHTTP(streamW, streamReq)
	}()

	// Wait for device to be online in row listing
	deadline = time.Now().Add(3 * time.Second)
	var online bool
	for time.Now().Before(deadline) {
		rowW := doReq(http.MethodGet, "/v1/devices", "")
		if strings.Contains(rowW.Body.String(), `"state":"read+act"`) {
			online = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !online {
		t.Fatalf("device did not become online")
	}

	// 3. Find device tool from hearthDeviceSource
	deviceWiringMu.RLock()
	toolSrc := hearthDeviceSource
	deviceWiringMu.RUnlock()
	if toolSrc == nil {
		t.Fatalf("hearthDeviceSource is nil")
	}

	tools := toolSrc()
	var smsTool *loop.Tool
	for i := range tools {
		if tools[i].Spec.Name == "mcp__pixel-loop__sms_send" {
			smsTool = &tools[i]
			break
		}
	}
	if smsTool == nil {
		t.Fatalf("mcp__pixel-loop__sms_send not found in tool source")
	}

	// 4. Invoke tool in background
	type callOutcome struct {
		res string
		err error
	}
	outcomeCh := make(chan callOutcome, 1)
	go func() {
		res, err := smsTool.Run(context.Background(), `{"recipient":"+15550199","message":"hello"}`)
		outcomeCh <- callOutcome{res: res, err: err}
	}()

	// 5. Read SSE frame from streamW
	var actionID string
	deadline = time.Now().Add(3 * time.Second)
	actRe := regexp.MustCompile(`"action_id":"([^"]+)"`)
	for time.Now().Before(deadline) {
		body := streamW.BodyString()
		if strings.Contains(body, "event: action_dispatch") {
			m := actRe.FindStringSubmatch(body)
			if len(m) >= 2 {
				actionID = m[1]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if actionID == "" {
		t.Fatalf("did not receive action_dispatch frame on stream")
	}

	// 6. Post receipt back to API
	receiptBody := `{"ok":true,"result":{"status":"delivered"}}`
	rcptW := doReq(http.MethodPost, "/v1/devices/pixel-loop/actions/"+actionID+"/receipt", receiptBody)
	if rcptW.Code != http.StatusOK {
		t.Fatalf("receipt status = %d: %s", rcptW.Code, rcptW.Body.String())
	}

	// 7. Verify tool handler completes
	select {
	case out := <-outcomeCh:
		if out.err != nil {
			t.Fatalf("tool handler returned error: %v", out.err)
		}
		if !strings.Contains(out.res, "delivered") {
			t.Fatalf("tool handler returned unexpected result: %s", out.res)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("tool handler timed out waiting for receipt resolution")
	}

	// 8. Disconnect stream -> verify tool handler fast-fails offline
	cancelStream()
	<-streamDone

	_, err = smsTool.Run(context.Background(), `{"recipient":"+15550199","message":"hello"}`)
	if err == nil {
		t.Fatalf("expected error invoking tool while offline, got nil")
	}
}

type safeRecorder struct {
	mu     sync.Mutex
	header http.Header
	code   int
	buf    bytes.Buffer
}

func newSafeRecorder() *safeRecorder {
	return &safeRecorder{
		header: make(http.Header),
		code:   http.StatusOK,
	}
}

func (r *safeRecorder) Header() http.Header {
	return r.header
}

func (r *safeRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *safeRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *safeRecorder) Flush() {}

func (r *safeRecorder) BodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *safeRecorder) Code() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}
