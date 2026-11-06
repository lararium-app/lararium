package custos_test

// V-T3 suite (DEVICE-RAILS-SPEC "Exit criterion"): end-to-end proofs for
// device rails (phone as paired push client) (T3). Real components only:
// real custosd, real DoorServer, real custosdoor client, real surface.Server,
// real nuntius.PairStore, real devices.Manager, real fake-phone HTTPS client.
//
// Run all nine proofs together with:
//
//	go test -run 'VT3' ./internal/custos/ -count=1 -v

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/devices"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

// vt3CanaryNeedle is the distinctive fixture secret for V-T3 secret leak proofs.
const vt3CanaryNeedle = "fx-liv-VT3SUITE-CANARY-00000000000000000000-9876"

// ---------------------------------------------------------------------------
// Adapters and fake-phone client fixture
// ---------------------------------------------------------------------------

// vt3PairStoreAdapter adapts *nuntius.PairStore to surface.PairStore.
type vt3PairStoreAdapter struct {
	store *nuntius.PairStore
}

func (a *vt3PairStoreAdapter) Redeem(userID, code string, now time.Time) (surface.RedeemResult, error) {
	res, err := a.store.Redeem(userID, code, now)
	if err != nil {
		return surface.RedeemInvalid, err
	}
	switch res {
	case nuntius.RedeemPaired:
		return surface.RedeemPaired, nil
	case nuntius.RedeemAlready:
		return surface.RedeemAlready, nil
	case nuntius.RedeemInvalid:
		return surface.RedeemInvalid, nil
	default:
		return surface.RedeemInvalid, nil
	}
}

// vt3DeviceDoorAdapter adapts *devices.Manager to surface.DeviceDoor per DEVICE-RAILS-SPEC D1-D8.
type vt3DeviceDoorAdapter struct {
	mgr           *devices.Manager
	audit         *custos.AuditLogger
	gracePeriod   time.Duration
	writerWg      sync.WaitGroup
	activeWriters atomic.Int64
	closed        atomic.Bool
}

func newVT3DeviceDoorAdapter(mgr *devices.Manager, audit *custos.AuditLogger) *vt3DeviceDoorAdapter {
	return &vt3DeviceDoorAdapter{
		mgr:         mgr,
		audit:       audit,
		gracePeriod: devices.DefaultGracePeriod,
	}
}

func (a *vt3DeviceDoorAdapter) trackWrite() (func(), error) {
	if a.closed.Load() {
		return nil, errors.New("device door closed")
	}
	a.writerWg.Add(1)
	a.activeWriters.Add(1)
	return func() {
		a.activeWriters.Add(-1)
		a.writerWg.Done()
	}, nil
}

// Drain marks the door closed and waits for all active writes to complete.
func (a *vt3DeviceDoorAdapter) Drain() {
	a.closed.Store(true)
	a.writerWg.Wait()
}

// ActiveWriters returns the count of in-flight write operations.
func (a *vt3DeviceDoorAdapter) ActiveWriters() int64 {
	return a.activeWriters.Load()
}

var _ surface.DeviceDoor = (*vt3DeviceDoorAdapter)(nil)

func (a *vt3DeviceDoorAdapter) ValidateID(deviceID string) error {
	return devices.ValidateDeviceID(deviceID)
}

func (a *vt3DeviceDoorAdapter) ValidateRails(rails []surface.DeclaredRail) error {
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

func (a *vt3DeviceDoorAdapter) Register(deviceID, displayName string, rails []surface.DeclaredRail) error {
	done, err := a.trackWrite()
	if err != nil {
		return err
	}
	defer done()

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

func (a *vt3DeviceDoorAdapter) List() []surface.DeviceCard {
	devs := a.mgr.List()
	out := make([]surface.DeviceCard, 0, len(devs))
	for _, dev := range devs {
		out = append(out, toVT3SurfaceDeviceCard(a.mgr, dev))
	}
	return out
}

func (a *vt3DeviceDoorAdapter) Get(deviceID string) (surface.DeviceCard, error) {
	dev, err := a.mgr.Get(deviceID)
	if err != nil {
		if errors.Is(err, devices.ErrDeviceNotFound) || errors.Is(err, devices.ErrDeviceTombstoned) {
			return surface.DeviceCard{}, surface.ErrDeviceNotFound
		}
		return surface.DeviceCard{}, err
	}
	return toVT3SurfaceDeviceCard(a.mgr, dev), nil
}

func toVT3SurfaceDeviceCard(mgr *devices.Manager, dev devices.Device) surface.DeviceCard {
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

func (a *vt3DeviceDoorAdapter) Forget(deviceID string) error {
	done, err := a.trackWrite()
	if err != nil {
		return err
	}
	defer done()

	return a.mgr.Forget(deviceID)
}

func (a *vt3DeviceDoorAdapter) SetRailEnabled(deviceID, rail string, enabled bool) error {
	done, err := a.trackWrite()
	if err != nil {
		return err
	}
	defer done()

	err = a.mgr.SetRailEnabled(deviceID, rail, enabled)
	if errors.Is(err, devices.ErrRailNotFound) {
		return surface.ErrRailNotFound
	}
	return err
}

func (a *vt3DeviceDoorAdapter) SetOnline(deviceID string, online bool) {
	a.mgr.SetOnline(deviceID, online)
}

func (a *vt3DeviceDoorAdapter) IsOnline(deviceID string) bool {
	return a.mgr.IsOnline(deviceID)
}

func (a *vt3DeviceDoorAdapter) LastSeen(deviceID string) time.Time {
	return a.mgr.LastSeen(deviceID)
}

func (a *vt3DeviceDoorAdapter) ApplySync(deviceID, rail string, snapshot []byte, deltas []surface.DeviceDelta, seq int64) error {
	done, err := a.trackWrite()
	if err != nil {
		return err
	}
	defer done()

	devDeltas := make([]devices.Delta, len(deltas))
	for i, d := range deltas {
		devDeltas[i] = devices.Delta{
			Op:   d.Op,
			ID:   d.ID,
			Data: d.Data,
		}
	}
	err = a.mgr.ApplySync(deviceID, rail, snapshot, devDeltas, seq)
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

func (a *vt3DeviceDoorAdapter) SetActionBroker(broker surface.ActionBroker) {
	a.mgr.SetActionBroker(broker)
}

func (a *vt3DeviceDoorAdapter) Audit(kind string, payload surface.DeviceAuditPayload) error {
	done, err := a.trackWrite()
	if err != nil {
		return err
	}
	defer done()

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

func (a *vt3DeviceDoorAdapter) GetGracePeriod() time.Duration {
	if a.gracePeriod <= 0 {
		return devices.DefaultGracePeriod
	}
	return a.gracePeriod
}

func (a *vt3DeviceDoorAdapter) SetGracePeriod(d time.Duration) {
	a.gracePeriod = d
	a.mgr.SetGracePeriod(d)
}

// ---------------------------------------------------------------------------
// SSE stream helpers
// ---------------------------------------------------------------------------

type vt3SSEEvent struct {
	Kind string
	Data string
}

type vt3SSEReader struct {
	resp   *http.Response
	r      *bufio.Reader
	events chan vt3SSEEvent
	errs   chan error
	done   chan struct{}
}

func (w *vt3World) openSSE(t *testing.T, path string) *vt3SSEReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.tsrv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.adminToken)
	req.Header.Set("Accept", "text/event-stream")

	timer := time.AfterFunc(30*time.Second, cancel)
	resp, err := w.client.Do(req)
	timer.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("SSE GET %s = %d", path, resp.StatusCode)
	}

	reader := &vt3SSEReader{
		resp:   resp,
		r:      bufio.NewReader(resp.Body),
		events: make(chan vt3SSEEvent, 64),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
	}
	t.Cleanup(func() {
		close(reader.done)
		_ = resp.Body.Close()
	})

	go func() {
		var curEvent, curData string
		for {
			select {
			case <-reader.done:
				return
			default:
			}
			line, rerr := reader.r.ReadString('\n')
			if rerr != nil {
				select {
				case <-reader.done:
				case reader.errs <- rerr:
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, ":") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				curEvent = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				curData = strings.TrimPrefix(line, "data: ")
			case line == "" && curEvent != "":
				ev := vt3SSEEvent{Kind: curEvent, Data: curData}
				curEvent, curData = "", ""
				select {
				case <-reader.done:
					return
				case reader.events <- ev:
				}
			}
		}
	}()

	return reader
}

func (r *vt3SSEReader) nextEvent(t *testing.T) vt3SSEEvent {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev
	case err := <-r.errs:
		t.Fatalf("SSE stream error: %v", err)
		return vt3SSEEvent{}
	case <-time.After(5 * time.Second):
		t.Fatalf("SSE read timed out")
		return vt3SSEEvent{}
	}
}

// ---------------------------------------------------------------------------
// Fake-Phone HTTPS Client Fixture (Deliverable 1 / spec R6)
// ---------------------------------------------------------------------------

type actionDispatchFrame struct {
	ActionID string          `json:"action_id"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
}

type fakePhone struct {
	t        *testing.T
	w        *vt3World
	client   *http.Client
	baseURL  string
	token    string
	deviceID string

	mu       sync.Mutex
	railSeqs map[string]int64

	streamMu     sync.Mutex
	streamCancel context.CancelFunc
	streamDone   chan struct{}
	streamEvents chan vt3SSEEvent
	streamErrs   chan error

	scriptMu      sync.Mutex
	receiptScript func(action actionDispatchFrame) (ok bool, result any, errStr string)
}

func waitVT3Done(t *testing.T, done <-chan struct{}, label string) bool {
	t.Helper()
	wait := 30 * time.Second
	if deadline, ok := t.Deadline(); ok {
		wait = min(wait, max(time.Until(deadline)-time.Second, time.Millisecond))
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		t.Errorf("timed out waiting for %s", label)
		return false
	}
}

func (w *vt3World) newPhone(t *testing.T) *fakePhone {
	t.Helper()
	return &fakePhone{
		t:            t,
		w:            w,
		client:       w.tsrv.Client(),
		baseURL:      w.tsrv.URL,
		railSeqs:     make(map[string]int64),
		streamEvents: make(chan vt3SSEEvent, 64),
		streamErrs:   make(chan error, 1),
	}
}

func (p *fakePhone) Token() string {
	return p.token
}

func (p *fakePhone) SetToken(tok string) {
	p.token = tok
}

func (p *fakePhone) Pair(t *testing.T, deviceName string) string {
	t.Helper()
	now := time.Now().UTC()
	code, err := p.w.pairStore.Mint(now, 10*time.Minute)
	if err != nil {
		t.Fatalf("pairStore.Mint: %v", err)
	}

	body := fmt.Sprintf(`{"code":%q,"device_name":%q}`, code, deviceName)
	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/v1/pair", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest /v1/pair: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatalf("Do /v1/pair: %v", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair status = %d: %s", resp.StatusCode, respBytes)
	}

	var pairRes struct {
		Token  string `json:"token"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(respBytes, &pairRes); err != nil || pairRes.Token == "" {
		t.Fatalf("unmarshal pair response %s: %v", respBytes, err)
	}

	p.token = pairRes.Token
	return p.token
}

func (p *fakePhone) Register(t *testing.T, deviceID, displayName string, rails []surface.DeclaredRail, fingerprint ...string) (string, int, string) {
	t.Helper()
	p.deviceID = deviceID
	reqMap := map[string]any{
		"device_id":      deviceID,
		"display_name":   displayName,
		"rails_declared": rails,
	}
	if len(fingerprint) > 0 && fingerprint[0] != "" {
		reqMap["fingerprint"] = fingerprint[0]
	}
	b, err := json.Marshal(reqMap)
	if err != nil {
		t.Fatalf("marshal register req: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/v1/devices/register", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest /v1/devices/register: %v", err)
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatalf("Do /v1/devices/register: %v", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	rawBody := string(respBytes)
	statusCode := resp.StatusCode
	cardID := ""

	if statusCode == http.StatusAccepted {
		var res struct {
			CardID string `json:"card_id"`
		}
		_ = json.Unmarshal(respBytes, &res)
		cardID = res.CardID
	}
	return cardID, statusCode, rawBody
}

func (p *fakePhone) OpenStream(t *testing.T) {
	t.Helper()
	t.Cleanup(p.CloseStream)
	p.streamMu.Lock()
	defer p.streamMu.Unlock()

	if p.streamCancel != nil {
		p.streamCancel()
		if p.streamDone != nil && !waitVT3Done(t, p.streamDone, "previous phone stream to stop") {
			t.FailNow()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.streamCancel = cancel
	p.streamDone = nil
	p.streamEvents = make(chan vt3SSEEvent, 64)
	p.streamErrs = make(chan error, 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/devices/"+p.deviceID+"/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest stream: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "text/event-stream")

	timer := time.AfterFunc(30*time.Second, cancel)
	resp, err := p.client.Do(req) //nolint:bodyclose // stream body is closed in background reader goroutine
	timer.Stop()
	if err != nil {
		t.Fatalf("Do stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET stream status = %d", resp.StatusCode)
	}

	p.streamDone = make(chan struct{})
	go func() {
		defer close(p.streamDone)
		defer resp.Body.Close()
		r := bufio.NewReader(resp.Body)
		var curEvent, curData string
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line, rerr := r.ReadString('\n')
			if rerr != nil {
				select {
				case <-ctx.Done():
				case p.streamErrs <- rerr:
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, ":") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				curEvent = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				curData = strings.TrimPrefix(line, "data: ")
			case line == "" && curEvent != "":
				ev := vt3SSEEvent{Kind: curEvent, Data: curData}
				curEvent, curData = "", ""

				if ev.Kind == "action_dispatch" {
					var frame actionDispatchFrame
					if jerr := json.Unmarshal([]byte(ev.Data), &frame); jerr == nil {
						p.scriptMu.Lock()
						fn := p.receiptScript
						p.scriptMu.Unlock()
						if fn != nil {
							go func(act actionDispatchFrame) {
								ok, res, errStr := fn(act)
								p.submitReceipt(act.ActionID, ok, res, errStr)
							}(frame)
						}
					}
				}

				select {
				case <-ctx.Done():
					return
				case p.streamEvents <- ev:
				}
			}
		}
	}()
}

func (p *fakePhone) CloseStream() {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.streamCancel != nil {
		p.streamCancel()
		if p.streamDone != nil && !waitVT3Done(p.t, p.streamDone, "phone stream to stop") {
			return
		}
		p.streamCancel = nil
		p.streamDone = nil
	}
}

func (p *fakePhone) NextEvent(t *testing.T) vt3SSEEvent {
	t.Helper()
	select {
	case ev := <-p.streamEvents:
		return ev
	case err := <-p.streamErrs:
		t.Fatalf("stream error: %v", err)
		return vt3SSEEvent{}
	case <-time.After(5 * time.Second):
		t.Fatalf("stream read timed out")
		return vt3SSEEvent{}
	}
}

func (p *fakePhone) NextAction(t *testing.T) actionDispatchFrame {
	t.Helper()
	ev := p.NextEvent(t)
	if ev.Kind != "action_dispatch" {
		t.Fatalf("expected action_dispatch event, got kind=%s data=%s", ev.Kind, ev.Data)
	}
	var f actionDispatchFrame
	if err := json.Unmarshal([]byte(ev.Data), &f); err != nil {
		t.Fatalf("unmarshal actionDispatchFrame %s: %v", ev.Data, err)
	}
	return f
}

func (p *fakePhone) Sync(t *testing.T, rail string, snapshot any, deltas []surface.DeviceDelta, seqOpt ...int64) (int, string) {
	t.Helper()
	p.mu.Lock()
	if p.railSeqs == nil {
		p.railSeqs = make(map[string]int64)
	}
	var seq int64
	if len(seqOpt) > 0 {
		seq = seqOpt[0]
		p.railSeqs[rail] = seq
	} else {
		p.railSeqs[rail]++
		seq = p.railSeqs[rail]
	}
	p.mu.Unlock()

	reqMap := map[string]any{
		"rail": rail,
		"seq":  seq,
	}
	if snapshot != nil {
		reqMap["snapshot"] = snapshot
	}
	if len(deltas) > 0 {
		reqMap["deltas"] = deltas
	}

	b, err := json.Marshal(reqMap)
	if err != nil {
		t.Fatalf("marshal sync req: %v", err)
		return 0, ""
	}

	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/v1/devices/"+p.deviceID+"/sync", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest sync: %v", err)
		return 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatalf("Do sync: %v", err)
		return 0, ""
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBytes)
}

func (p *fakePhone) SubmitReceipt(t *testing.T, actionID string, ok bool, result any, errStr string) (int, string) {
	t.Helper()
	return p.submitReceipt(actionID, ok, result, errStr)
}

func (p *fakePhone) submitReceipt(actionID string, ok bool, result any, errStr string) (int, string) {
	reqMap := map[string]any{
		"ok": ok,
	}
	if result != nil {
		reqMap["result"] = result
	}
	if errStr != "" {
		reqMap["error"] = errStr
	}

	b, err := json.Marshal(reqMap)
	if err != nil {
		return 0, ""
	}

	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/v1/devices/"+p.deviceID+"/actions/"+actionID+"/receipt", bytes.NewReader(b))
	if err != nil {
		return 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBytes)
}

func (p *fakePhone) SetReceiptScript(fn func(action actionDispatchFrame) (bool, any, string)) {
	p.scriptMu.Lock()
	defer p.scriptMu.Unlock()
	p.receiptScript = fn
}

// ---------------------------------------------------------------------------
// Mock Model & Session Runner for loop interaction
// ---------------------------------------------------------------------------

type vt3Model struct {
	calls []router.ToolCall
	n     int32
}

func (*vt3Model) Name() string { return "vt3model" }
func (*vt3Model) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}, nil
}

func (m *vt3Model) Complete(_ context.Context, _ []router.Message, opts router.Options) (*router.Completion, error) {
	if atomic.AddInt32(&m.n, 1) == 1 && len(opts.Tools) > 0 {
		return &router.Completion{ToolCalls: m.calls, Model: "vt3model"}, nil
	}
	return &router.Completion{Text: "done", Model: "vt3model"}, nil
}

// ---------------------------------------------------------------------------
// vt3World fixture
// ---------------------------------------------------------------------------

type vt3World struct {
	t           *testing.T
	dir         string
	stateDir    string
	d           *custos.Daemon
	hub         *surface.ApprovalHub
	doorClient  *custosdoor.Client
	doorCancel  context.CancelFunc
	doorDone    chan struct{}
	adminToken  string
	srv         *surface.Server
	tsrv        *httptest.Server
	client      *http.Client
	logbuf      *safeBuf
	pairStore   *nuntius.PairStore
	auditLogger *custos.AuditLogger
	devMgr      *devices.Manager
	devDoor     *vt3DeviceDoorAdapter
	doors       []*vt3DeviceDoorAdapter
	doorsMu     sync.Mutex
}

func newVT3World(t *testing.T) *vt3World {
	t.Helper()
	w := &vt3World{t: t, logbuf: &safeBuf{}}

	prevW, prevF := log.Writer(), log.Flags()
	log.SetOutput(w.logbuf)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevF) })

	w.dir = t.TempDir()
	w.stateDir = filepath.Join(w.dir, "custos")
	if err := os.MkdirAll(w.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &custos.Config{
		AskHoldTimeout: 30 * time.Second,
	}
	w.d = custos.NewDaemon(w.stateDir, cfg)
	v := w.d.Vault()
	if !v.Exists() {
		if err := v.Init(lcPass); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Unlock(lcPass, false); err != nil {
		t.Fatal(err)
	}
	if err := w.d.Start(); err != nil {
		t.Fatal(err)
	}
	w.hub = w.d.Hub()

	sockPath, tokenPath := custos.DoorPaths(w.stateDir)
	attachedCh := make(chan struct{}, 1)
	var err error
	w.doorClient, err = custosdoor.New(custosdoor.Options{
		SockPath:   sockPath,
		TokenPath:  tokenPath,
		Name:       "hearthd",
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		AckTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	doorCtx, doorCancel := context.WithCancel(context.Background())
	w.doorCancel = doorCancel
	w.doorDone = make(chan struct{})

	unsubMCP := w.doorClient.SubscribeMCP(func([]custosdoor.ConnectorStatus, []custosdoor.ToolInfo) {
		select {
		case attachedCh <- struct{}{}:
		default:
		}
	})
	defer unsubMCP()

	go func() {
		defer close(w.doorDone)
		w.doorClient.Run(doorCtx)
	}()

	select {
	case <-attachedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("doorClient did not attach to DoorServer")
	}

	tokenStore, err := surface.OpenTokenStore(filepath.Join(w.dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	w.adminToken, err = tokenStore.Create("vt3-admin")
	if err != nil {
		t.Fatal(err)
	}

	pairDir := filepath.Join(w.dir, "nuntius")
	w.pairStore, err = nuntius.NewPairStore(pairDir)
	if err != nil {
		t.Fatal(err)
	}

	w.devMgr, err = devices.NewManager(w.dir)
	if err != nil {
		t.Fatal(err)
	}

	w.auditLogger = custos.NewAuditLogger(w.stateDir)
	_ = w.auditLogger.EnsureAuditDir()
	w.devDoor = newVT3DeviceDoorAdapter(w.devMgr, w.auditLogger)
	w.doors = []*vt3DeviceDoorAdapter{w.devDoor}

	w.srv = &surface.Server{
		Store:     tokenStore,
		PairStore: &vt3PairStoreAdapter{store: w.pairStore},
		Cfg:       surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	w.srv.SetApprovalHub(w.hub)
	w.srv.SetCustosRegistry(&vt2DoorAdapter{cl: w.doorClient})
	w.srv.SetConnectorDoor(&vt2ConnectorDoorAdapter{client: w.doorClient})
	w.srv.SetCredentialSource(vt1CredAdapter{v: v})
	w.srv.SetDeviceDoor(w.devDoor)

	// Plain HTTPS test server per Deliverable 1
	w.tsrv = httptest.NewTLSServer(surface.NewServer(w.srv))
	t.Cleanup(func() {
		w.tsrv.CloseClientConnections()
		w.tsrv.Close()
	})
	w.client = w.tsrv.Client()

	t.Cleanup(w.cleanup)
	return w
}

func (w *vt3World) ActiveWriters() int64 {
	w.doorsMu.Lock()
	defer w.doorsMu.Unlock()
	var total int64
	for _, d := range w.doors {
		total += d.ActiveWriters()
	}
	return total
}

func (w *vt3World) cleanup() {
	if w.doorCancel != nil {
		w.doorCancel()
	}
	if w.doorDone != nil && !waitVT3Done(w.t, w.doorDone, "door client to stop") {
		return
	}
	if w.d != nil {
		w.d.Stop()
		for _, c := range w.d.Rail().Connectors() {
			c.Shutdown(false)
		}
	}
	if w.srv != nil {
		w.srv.Drain()
	}
	w.doorsMu.Lock()
	for _, d := range w.doors {
		d.Drain()
	}
	w.doorsMu.Unlock()
	if w.devMgr != nil {
		_ = w.devMgr.Close()
	}
}

func (w *vt3World) restartDaemon() {
	w.t.Helper()
	if w.srv != nil {
		w.srv.Drain()
	}
	if w.devDoor != nil {
		w.devDoor.Drain()
	}
	if w.devMgr != nil {
		_ = w.devMgr.Close()
	}

	var err error
	w.devMgr, err = devices.NewManager(w.dir)
	if err != nil {
		w.t.Fatalf("restart devices.NewManager: %v", err)
	}
	w.devDoor = newVT3DeviceDoorAdapter(w.devMgr, w.auditLogger)
	w.doorsMu.Lock()
	w.doors = append(w.doors, w.devDoor)
	w.doorsMu.Unlock()
	w.srv.SetDeviceDoor(w.devDoor)
}

func (w *vt3World) toolSource() []loop.Tool {
	src := w.devMgr.ToolSource()
	if src == nil {
		return nil
	}
	return src()
}

func (w *vt3World) postAdmin(path, body string) (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.tsrv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt3World) patchAdmin(path, body string) (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPatch, w.tsrv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt3World) deleteAdmin(path string) (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodDelete, w.tsrv.URL+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.adminToken)
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt3World) getAdmin(path string) (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.tsrv.URL+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.adminToken)
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt3World) resolveCard(t *testing.T, cardID string) {
	t.Helper()
	body, code := w.postAdmin("/v1/custos/cards/"+cardID+"/resolve", `{"verdict":"once"}`)
	if code != http.StatusOK {
		t.Fatalf("resolveCard %s status = %d: %s", cardID, code, body)
	}
}

func (w *vt3World) runSession(t *testing.T, calls []router.ToolCall, tools []loop.Tool, ap loop.Approver) (string, error) {
	t.Helper()
	dir := t.TempDir()
	lg, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &vt3Model{calls: calls}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: m}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: m}}})
	sess := &loop.Session{Log: lg, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80}
	return sess.RunTurnTools(context.Background(), "go", nil, tools, ap)
}

// assertNoNeedleToken asserts neither raw needle nor standard/raw/URL base64 forms
// appear in the payload.
func assertNoNeedleToken(t *testing.T, label, data, needle string) {
	t.Helper()
	if needle == "" {
		return
	}
	if strings.Contains(data, needle) {
		t.Errorf("%s contains secret needle %q", label, needle)
	}
	encs := []string{
		base64.StdEncoding.EncodeToString([]byte(needle)),
		base64.RawStdEncoding.EncodeToString([]byte(needle)),
		base64.URLEncoding.EncodeToString([]byte(needle)),
		base64.RawURLEncoding.EncodeToString([]byte(needle)),
	}
	for off := 1; off < 3; off++ {
		enc := base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("x", off) + needle))
		if len(enc) > 8 {
			encs = append(encs, enc[4:len(enc)-4])
		}
	}
	for _, enc := range encs {
		if strings.Contains(data, enc) {
			t.Errorf("%s contains base64-encoded needle (%s)", label, enc)
		}
	}
}

// ---------------------------------------------------------------------------
// V-T3 exit suite: TestVT3a through TestVT3i
// ---------------------------------------------------------------------------

// TestVT3a proves: register→card→approve→row online, catalog tools carry
// mcp__<device>__ prefix, device_state arrives over SSE without polling.
func TestVT3a(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	// 1. Phone pairs to obtain bearer token
	phone.Pair(t, "pixel-a")

	// 2. Open SSE streams before acting
	cardSSE := w.openSSE(t, "/v1/custos/cards/events")
	devSSE := w.openSSE(t, "/v1/devices/events")

	// 3. Register device with read and action rails
	rails := []surface.DeclaredRail{
		{Name: devices.RailCalendar, ReadOnly: true},
		{Name: devices.RailSMS, ReadOnly: false},
	}
	cardID, code, body := phone.Register(t, "pixel-a", "Pixel A Phone", rails)
	if code != http.StatusAccepted || cardID == "" {
		t.Fatalf("Register status = %d: %s", code, body)
	}

	// 4. Custody card arrives over SSE without test polling
	cardEv := cardSSE.nextEvent(t)
	if cardEv.Kind != "custos_card" || !strings.Contains(cardEv.Data, cardID) {
		t.Fatalf("unexpected card event: kind=%s data=%s", cardEv.Kind, cardEv.Data)
	}

	// 5. Approve the custody card
	w.resolveCard(t, cardID)

	// 6. Connect the phone stream
	phone.OpenStream(t)

	// 7. device_state arrives over SSE without test polling
	var stateEv vt3SSEEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ev := devSSE.nextEvent(t)
		if ev.Kind == "device_state" && strings.Contains(ev.Data, "pixel-a") && strings.Contains(ev.Data, "read+act") {
			stateEv = ev
			break
		}
	}
	if stateEv.Kind == "" {
		t.Fatalf("device_state event with state read+act not received over SSE")
	}

	// 8. Device row in GET /v1/devices is online (read+act)
	listBody, listCode := w.getAdmin("/v1/devices")
	if listCode != http.StatusOK {
		t.Fatalf("GET /v1/devices = %d: %s", listCode, listBody)
	}
	if !strings.Contains(listBody, `"pixel-a"`) || !strings.Contains(listBody, `"read+act"`) {
		t.Fatalf("GET /v1/devices row not online: %s", listBody)
	}

	// 9. Catalog tools carry mcp__<device>__ prefix
	tools := w.toolSource()
	var foundCal, foundSMS bool
	for _, tool := range tools {
		if tool.Spec.Name == "mcp__pixel-a__calendar_list" {
			foundCal = true
		}
		if tool.Spec.Name == "mcp__pixel-a__sms_send" {
			foundSMS = true
		}
	}
	if !foundCal || !foundSMS {
		t.Fatalf("tools missing prefix: foundCal=%v, foundSMS=%v in %+v", foundCal, foundSMS, tools)
	}
}

// TestVT3b proves: snapshot then delta sync→session-loop read serves synced data
// in-process; last_used stamped; read rails raise NO card.
func TestVT3b(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	phone.Pair(t, "pixel-b")
	rails := []surface.DeclaredRail{
		{Name: devices.RailContacts, ReadOnly: true},
	}
	cardID, code, body := phone.Register(t, "pixel-b", "Pixel B Phone", rails)
	if code != http.StatusAccepted {
		t.Fatalf("Register status = %d: %s", code, body)
	}
	w.resolveCard(t, cardID)
	phone.OpenStream(t)

	// 1. Snapshot sync (seq 1)
	snapJSON := []map[string]any{
		{"id": "c1", "name": "Alice Smith"},
	}
	syncCode, syncBody := phone.Sync(t, devices.RailContacts, snapJSON, nil, 1)
	if syncCode != http.StatusOK {
		t.Fatalf("snapshot sync status = %d: %s", syncCode, syncBody)
	}

	// 2. Incremental delta sync (seq 2)
	deltaJSON := []surface.DeviceDelta{
		{Op: "add", ID: "c2", Data: json.RawMessage(`{"name":"Bob Jones"}`)},
	}
	syncCode2, syncBody2 := phone.Sync(t, devices.RailContacts, nil, deltaJSON, 2)
	if syncCode2 != http.StatusOK {
		t.Fatalf("delta sync status = %d: %s", syncCode2, syncBody2)
	}

	// 3. Verify last_seen is stamped on the device
	dev, err := w.devMgr.Get("pixel-b")
	if err != nil {
		t.Fatalf("devMgr.Get: %v", err)
	}
	if dev.LastSeen.IsZero() {
		t.Fatalf("expected last_seen to be stamped on sync")
	}

	// 4. Session-loop read serves synced data in-process without raising any card
	tools := w.toolSource()
	var contactsTool *loop.Tool
	for i := range tools {
		if tools[i].Spec.Name == "mcp__pixel-b__contacts_search" {
			contactsTool = &tools[i]
			break
		}
	}
	if contactsTool == nil {
		t.Fatalf("contacts tool not found in tool source")
	}
	if !contactsTool.Trusted {
		t.Fatalf("read rail tool must be Trusted: true (raises NO card)")
	}

	approverCalled := false
	approver := func(_, argsSummary string) (bool, string) {
		approverCalled = true
		return true, "turn-appr-1"
	}

	calls := []router.ToolCall{
		{Name: "mcp__pixel-b__contacts_search", ArgsJSON: `{"query":"Alice"}`},
	}
	outcomeText, err := w.runSession(t, calls, tools, approver)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if approverCalled {
		t.Fatalf("read rails MUST raise NO card")
	}

	// Synced data served in-process
	if outcomeText != "done" {
		t.Fatalf("unexpected turn outcome %q", outcomeText)
	}

	readDirect, err := contactsTool.Run(context.Background(), `{"query":"Alice"}`)
	if err != nil {
		t.Fatalf("contactsTool.Run: %v", err)
	}
	if !strings.Contains(readDirect, "Alice Smith") || !strings.Contains(readDirect, "Bob Jones") {
		t.Fatalf("synced data not returned in-process: %s", readDirect)
	}
}

// TestVT3c proves: dialer/sms call parks a card whose body contains the FULL
// args JSON (assert a long message body survives verbatim — this kills the
// 200-byte summarize truncation for good); approve→action_dispatch frame→receipt
// resolves the tool call with the receipt result.
func TestVT3c(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	phone.Pair(t, "pixel-c")
	rails := []surface.DeclaredRail{
		{Name: devices.RailSMS, ReadOnly: false},
	}
	cardID, code, body := phone.Register(t, "pixel-c", "Pixel C Phone", rails)
	if code != http.StatusAccepted {
		t.Fatalf("Register status = %d: %s", code, body)
	}
	w.resolveCard(t, cardID)
	phone.OpenStream(t)

	tools := w.toolSource()
	var smsTool *loop.Tool
	for i := range tools {
		if tools[i].Spec.Name == "mcp__pixel-c__sms_send" {
			smsTool = &tools[i]
			break
		}
	}
	if smsTool == nil {
		t.Fatalf("sms tool not found in tool source")
	}
	if smsTool.Trusted {
		t.Fatalf("action rail tool must NOT be Trusted")
	}
	if !smsTool.FullArgs {
		t.Fatalf("action rail tool MUST carry FullArgs: true")
	}

	// Construct a long message body well exceeding the 200-byte summarizer truncation limit
	longMsg := "VERBATIM-SMS-START-" + strings.Repeat("0123456789abcdefghijklmnopqrstuvwxyz", 10) + "-VERBATIM-SMS-END"
	if len(longMsg) <= 200 {
		t.Fatalf("longMsg must exceed 200 bytes, got %d", len(longMsg))
	}

	argsJSON := fmt.Sprintf(`{"recipient":"+15550199","message":%q}`, longMsg)

	// Park turn approval card
	reviewCh := make(chan string, 1)
	turnDecisionCh := make(chan bool, 1)
	approver := func(_, argsSummary string) (bool, string) {
		select {
		case reviewCh <- argsSummary:
		default:
		}
		select {
		case allow := <-turnDecisionCh:
			return allow, "turn-c-1"
		case <-time.After(5 * time.Second):
			return false, "timeout"
		}
	}

	callOutcomeCh := make(chan struct {
		text string
		err  error
	}, 1)

	calls := []router.ToolCall{
		{Name: "mcp__pixel-c__sms_send", ArgsJSON: argsJSON},
	}
	go func() {
		text, rerr := w.runSession(t, calls, tools, approver)
		callOutcomeCh <- struct {
			text string
			err  error
		}{text: text, err: rerr}
	}()

	// 1. Verify card body contains the FULL args JSON verbatim (kills 200-byte summarizer truncation)
	var capturedCardReview string
	select {
	case capturedCardReview = <-reviewCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("turn approver was not called")
	}
	if !strings.Contains(capturedCardReview, longMsg) {
		t.Fatalf("card args summary truncated or missing longMsg: length=%d, summary=%q", len(capturedCardReview), capturedCardReview)
	}
	if strings.HasSuffix(capturedCardReview, "…") {
		t.Fatalf("card args summary was truncated with ellipsis: %q", capturedCardReview)
	}

	// 2. Approve the turn card
	turnDecisionCh <- true

	// 3. action_dispatch frame arrives on phone stream
	actFrame := phone.NextAction(t)
	if actFrame.Tool != devices.ToolSMSSend {
		t.Fatalf("action_dispatch tool = %q, want %q", actFrame.Tool, devices.ToolSMSSend)
	}
	if !strings.Contains(string(actFrame.Args), longMsg) {
		t.Fatalf("action_dispatch frame missing verbatim longMsg: %s", actFrame.Args)
	}

	// 4. Phone submits receipt
	receiptResult := map[string]any{"status": "delivered", "message_id": "msg-7717"}
	rcptCode, rcptBody := phone.SubmitReceipt(t, actFrame.ActionID, true, receiptResult, "")
	if rcptCode != http.StatusOK {
		t.Fatalf("SubmitReceipt status = %d: %s", rcptCode, rcptBody)
	}

	// 5. Tool call resolves with the receipt result
	select {
	case outcome := <-callOutcomeCh:
		if outcome.err != nil {
			t.Fatalf("runSession failed: %v", outcome.err)
		}
		if outcome.text != "done" {
			t.Fatalf("unexpected turn text %q", outcome.text)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("turn did not complete after receipt submitted")
	}
}

// TestVT3d proves: close stream→online until 30s grace elapses (inject/short-circuit
// the grace timer ONLY via exported test hooks if the merged code provides them;
// otherwise drive real 30s in ONE test with t.Parallel discipline); reads carry
// the exact staleness marker; action fast-fails device_offline; reconnect→online
// WITHOUT re-register.
func TestVT3d(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	// Inject a short grace window via the merged exported test hooks.
	// 500ms (was 50ms): short-circuits the grace timer while leaving enough
	// headroom that the post-close "still online" assert cannot lose a race
	// to a scheduler stall on loaded CI runners (lararium#261).
	w.devDoor.SetGracePeriod(500 * time.Millisecond)

	phone.Pair(t, "pixel-d")
	rails := []surface.DeclaredRail{
		{Name: devices.RailCalendar, ReadOnly: true},
		{Name: devices.RailDialer, ReadOnly: false},
	}
	cardID, code, body := phone.Register(t, "pixel-d", "Pixel D Phone", rails)
	if code != http.StatusAccepted {
		t.Fatalf("Register status = %d: %s", code, body)
	}
	w.resolveCard(t, cardID)

	// Sync calendar data
	phone.Sync(t, devices.RailCalendar, []map[string]any{{"title": "Sprint Review"}}, nil, 1)

	// Connect stream
	phone.OpenStream(t)
	// Online-marking is async behind the stream attach; wait for it instead of
	// asserting instantly (loaded runners lost this race, lararium#261).
	waitFor(t, "device online after OpenStream", func() bool {
		return w.devDoor.IsOnline("pixel-d")
	})

	// 1. Close stream
	phone.CloseStream()

	// 2. Immediately assert device remains online during active grace window
	if !w.devDoor.IsOnline("pixel-d") {
		t.Fatalf("device MUST remain online during the active grace period")
	}

	// 3. Wait for grace timer (50ms) to elapse
	waitFor(t, "device transitions to offline after grace window expires", func() bool {
		return !w.devDoor.IsOnline("pixel-d")
	})

	// GET /v1/devices shows not-connected and offline
	listBody, listCode := w.getAdmin("/v1/devices")
	if listCode != http.StatusOK {
		t.Fatalf("GET /v1/devices = %d: %s", listCode, listBody)
	}
	if !strings.Contains(listBody, `"state":"not-connected"`) || !strings.Contains(listBody, `"reason":"offline"`) {
		t.Fatalf("row not marked offline in listing: %s", listBody)
	}

	// 4. Reads carry the exact staleness marker
	tools := w.toolSource()
	var calTool, dialTool *loop.Tool
	for i := range tools {
		if tools[i].Spec.Name == "mcp__pixel-d__calendar_list" {
			calTool = &tools[i]
		}
		if tools[i].Spec.Name == "mcp__pixel-d__dialer_call" {
			dialTool = &tools[i]
		}
	}
	if calTool == nil || dialTool == nil {
		t.Fatalf("missing tools: cal=%v dial=%v", calTool, dialTool)
	}

	calRead, err := calTool.Run(context.Background(), "{}")
	if err != nil {
		t.Fatalf("calTool.Run: %v", err)
	}
	if !strings.HasPrefix(calRead, "[stale: phone offline since ") || !strings.Contains(calRead, ", lag ") {
		t.Fatalf("read output missing exact staleness marker: %q", calRead)
	}
	if !strings.Contains(calRead, "Sprint Review") {
		t.Fatalf("cached calendar data missing from read output: %s", calRead)
	}

	// 5. Action fast-fails device_offline
	_, dialErr := dialTool.Run(context.Background(), `{"number":"+15550199"}`)
	if dialErr == nil || !errors.Is(dialErr, devices.ErrDeviceOffline) {
		t.Fatalf("expected ErrDeviceOffline, got %v", dialErr)
	}

	// 6. Reconnect stream -> online WITHOUT re-registration
	phone.OpenStream(t)
	waitFor(t, "reconnect restores online", func() bool {
		return w.devDoor.IsOnline("pixel-d")
	})

	rowBody, rowCode := w.getAdmin("/v1/devices/pixel-d")
	if rowCode != http.StatusOK {
		t.Fatalf("GET /v1/devices/pixel-d = %d: %s", rowCode, rowBody)
	}
	if !strings.Contains(rowBody, `"state":"read+act"`) {
		t.Fatalf("reconnected device state = %s, want read+act", rowBody)
	}
}

// TestVT3e proves: PATCH rail toggle (carded)→tool gone from catalog + sync for
// that rail rejected; re-enable round-trips; both toggles audited device_rail_toggled.
func TestVT3e(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	phone.Pair(t, "pixel-e")
	rails := []surface.DeclaredRail{
		{Name: devices.RailCalendar, ReadOnly: true},
		{Name: devices.RailSMS, ReadOnly: false},
	}
	cardID, code, body := phone.Register(t, "pixel-e", "Pixel E Phone", rails)
	if code != http.StatusAccepted {
		t.Fatalf("Register status = %d: %s", code, body)
	}
	w.resolveCard(t, cardID)
	phone.OpenStream(t)

	// Verify calendar tool initially present in catalog
	hasCalTool := func() bool {
		for _, t := range w.toolSource() {
			if t.Spec.Name == "mcp__pixel-e__calendar_list" {
				return true
			}
		}
		return false
	}
	if !hasCalTool() {
		t.Fatalf("calendar tool should be initially present in catalog")
	}

	// 1. PATCH rail toggle (carded disable)
	patchBody, patchCode := w.patchAdmin("/v1/devices/pixel-e/rails", `{"rail":"calendar","enabled":false}`)
	if patchCode != http.StatusAccepted {
		t.Fatalf("PATCH disable status = %d: %s", patchCode, patchBody)
	}
	var patchRes struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(patchBody), &patchRes)
	if patchRes.CardID == "" {
		t.Fatalf("expected card_id in PATCH response: %s", patchBody)
	}

	// Approve card
	w.resolveCard(t, patchRes.CardID)

	// 2. Tool is gone from catalog
	waitFor(t, "calendar tool gone from catalog", func() bool {
		return !hasCalTool()
	})

	// 3. Sync for disabled rail is rejected
	syncCode, syncBody := phone.Sync(t, devices.RailCalendar, []map[string]any{{"title": "nope"}}, nil, 1)
	if syncCode != http.StatusBadRequest && syncCode != http.StatusConflict {
		t.Fatalf("expected sync rejection for disabled rail, got %d: %s", syncCode, syncBody)
	}
	if !strings.Contains(syncBody, "rail_disabled") {
		t.Fatalf("sync response reason != rail_disabled: %s", syncBody)
	}

	// 4. Re-enable round-trips
	patchBody2, patchCode2 := w.patchAdmin("/v1/devices/pixel-e/rails", `{"rail":"calendar","enabled":true}`)
	if patchCode2 != http.StatusAccepted {
		t.Fatalf("PATCH enable status = %d: %s", patchCode2, patchBody2)
	}
	var patchRes2 struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(patchBody2), &patchRes2)
	w.resolveCard(t, patchRes2.CardID)

	waitFor(t, "calendar tool restored in catalog", func() bool {
		return hasCalTool()
	})

	syncCode2, syncBody2 := phone.Sync(t, devices.RailCalendar, []map[string]any{{"title": "restored"}}, nil, 2)
	if syncCode2 != http.StatusOK {
		t.Fatalf("sync after re-enable failed: code=%d %s", syncCode2, syncBody2)
	}

	// 5. Both toggles audited device_rail_toggled
	toggledAudits := auditByKind(t, w.stateDir, surface.AuditKindDeviceRailToggled)
	if len(toggledAudits) < 2 {
		t.Fatalf("expected at least 2 device_rail_toggled records, got %d", len(toggledAudits))
	}
	var hadDisable, hadEnable bool
	for _, rec := range toggledAudits {
		if strings.Contains(rec.Reason, "enabled=false") {
			hadDisable = true
		}
		if strings.Contains(rec.Reason, "enabled=true") {
			hadEnable = true
		}
	}
	if !hadDisable || !hadEnable {
		t.Fatalf("missing toggle audits: hadDisable=%v, hadEnable=%v", hadDisable, hadEnable)
	}
}

// TestVT3f proves: DELETE→gone from GET, device_gone on both SSE feeds, rail data
// files erased from disk (assert path absent), daemon restart keeps tombstone,
// re-register needs a fresh card.
func TestVT3f(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	phone.Pair(t, "pixel-f")
	rails := []surface.DeclaredRail{
		{Name: devices.RailContacts, ReadOnly: true},
	}
	cardID, code, body := phone.Register(t, "pixel-f", "Pixel F Phone", rails)
	if code != http.StatusAccepted {
		t.Fatalf("Register status = %d: %s", code, body)
	}
	w.resolveCard(t, cardID)
	phone.OpenStream(t)

	// Sync contacts data to ensure files exist on disk
	phone.Sync(t, devices.RailContacts, []map[string]any{{"name": "Delete Me"}}, nil, 1)

	railFile := filepath.Join(w.dir, "devices", "pixel-f", "contacts.json")
	if _, err := os.Stat(railFile); err != nil {
		t.Fatalf("rail data file not created: %v", err)
	}

	// Open both SSE feeds before DELETE
	aggSSE := w.openSSE(t, "/v1/devices/events")
	perDevSSE := w.openSSE(t, "/v1/devices/pixel-f/events")

	// 1. DELETE /v1/devices/pixel-f
	delBody, delCode := w.deleteAdmin("/v1/devices/pixel-f")
	if delCode != http.StatusOK && delCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d: %s", delCode, delBody)
	}

	// 2. Gone from GET
	listBody, listCode := w.getAdmin("/v1/devices")
	if listCode != http.StatusOK || strings.Contains(listBody, "pixel-f") {
		t.Fatalf("device still listed in GET /v1/devices: %s", listBody)
	}
	_, getCode := w.getAdmin("/v1/devices/pixel-f")
	if getCode != http.StatusNotFound {
		t.Fatalf("GET /v1/devices/pixel-f status = %d, want 404", getCode)
	}

	// 3. device_gone event on both SSE feeds
	aggEv := aggSSE.nextEvent(t)
	if aggEv.Kind != "device_gone" || !strings.Contains(aggEv.Data, "pixel-f") {
		t.Fatalf("aggregate feed expected device_gone, got kind=%s data=%s", aggEv.Kind, aggEv.Data)
	}

	perEv := perDevSSE.nextEvent(t)
	if perEv.Kind != "device_gone" || !strings.Contains(perEv.Data, "pixel-f") {
		t.Fatalf("per-device feed expected device_gone, got kind=%s data=%s", perEv.Kind, perEv.Data)
	}

	// 4. Rail data files erased from disk (assert path absent)
	deviceDir := filepath.Join(w.dir, "devices", "pixel-f")
	if _, err := os.Stat(deviceDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("device directory still exists on disk: %v", err)
	}

	// 5. Daemon restart keeps tombstone
	w.restartDaemon()
	if !w.devMgr.IsTombstoned("pixel-f") {
		t.Fatalf("expected device to remain tombstoned after restart")
	}

	// 6. Re-register needs a fresh card
	newCardID, newCode, newBody := phone.Register(t, "pixel-f", "Pixel F Resurrected", rails)
	if newCode != http.StatusAccepted || newCardID == "" {
		t.Fatalf("re-registration status = %d: %s", newCode, newBody)
	}
	if newCardID == cardID {
		t.Fatalf("re-registration MUST issue a fresh card_id")
	}
}

// TestVT3fTempdirCleanupRace regression-tests issue #246: ensures daemon restart
// and harness teardown drain and join every background writer before TempDir cleanup,
// leaving zero live-writer stragglers on the harness WaitGroup/counter seam.
func TestVT3fTempdirCleanupRace(t *testing.T) {
	var world *vt3World
	var oldDoor, newDoor *vt3DeviceDoorAdapter
	var tempDir string

	t.Run("run", func(t *testing.T) {
		w := newVT3World(t)
		world = w
		tempDir = w.dir
		oldDoor = w.devDoor

		phone := w.newPhone(t)
		phone.Pair(t, "pixel-race")
		rails := []surface.DeclaredRail{
			{Name: devices.RailContacts, ReadOnly: true},
		}
		cardID, code, body := phone.Register(t, "pixel-race", "Pixel Race", rails)
		if code != http.StatusAccepted {
			t.Fatalf("Register status = %d: %s", code, body)
		}
		w.resolveCard(t, cardID)
		phone.OpenStream(t)

		phone.Sync(t, devices.RailContacts, []map[string]any{{"name": "Race Me"}}, nil, 1)

		railFile := filepath.Join(w.dir, "devices", "pixel-race", "contacts.json")
		if _, err := os.Stat(railFile); err != nil {
			t.Fatalf("rail data file not created: %v", err)
		}

		// DELETE /v1/devices/pixel-race
		delBody, delCode := w.deleteAdmin("/v1/devices/pixel-race")
		if delCode != http.StatusOK && delCode != http.StatusNoContent {
			t.Fatalf("DELETE status = %d: %s", delCode, delBody)
		}

		// Step 5: Daemon restart drains old instance
		w.restartDaemon()
		newDoor = w.devDoor
		if oldDoor == newDoor {
			t.Fatalf("expected restartDaemon to allocate a new door adapter")
		}
		if oldDoor.ActiveWriters() != 0 {
			t.Fatalf("oldDoor active writers = %d after restartDaemon, want 0", oldDoor.ActiveWriters())
		}

		// Step 6: Re-register creates a fresh card; left pending at test return
		newCardID, newCode, newBody := phone.Register(t, "pixel-race", "Pixel Race Resurrected", rails)
		if newCode != http.StatusAccepted || newCardID == "" {
			t.Fatalf("re-registration status = %d: %s", newCode, newBody)
		}
		if newCardID == cardID {
			t.Fatalf("re-registration MUST issue a fresh card_id")
		}
		// Subtest finishes here with a pending registration card in flight.
		// All t.Cleanup callbacks (including w.cleanup and t.TempDir removal)
		// execute deterministically before t.Run returns.
	})

	// After subtest and harness cleanup have completed:
	if oldDoor == nil || newDoor == nil || world == nil {
		t.Fatalf("missing world or door adapters from subtest")
	}
	if count := oldDoor.ActiveWriters(); count != 0 {
		t.Fatalf("oldDoor active writers straggler count = %d, want 0", count)
	}
	if count := newDoor.ActiveWriters(); count != 0 {
		t.Fatalf("newDoor active writers straggler count = %d, want 0", count)
	}
	if count := world.ActiveWriters(); count != 0 {
		t.Fatalf("world active writers straggler count = %d, want 0", count)
	}

	// Verify tempDir was successfully removed by t.TempDir() cleanup without error
	if _, err := os.Stat(tempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp dir %s still exists or failed to remove: %v", tempDir, err)
	}
}

// TestVT3g proves: pairing token never leaks — assertNoNeedle across every
// response body, SSE frame, audit line, and log capture for a full
// register→sync→action lifecycle (assertion must cover base64/url-encoded forms too).
func TestVT3g(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	// Distinctive canary pairing token needle
	pairedTok := phone.Pair(t, "canary-phone")
	if pairedTok == "" {
		t.Fatalf("pairing failed")
	}

	// Collect bodies and frames for leak assertion
	var bodies []string
	var frames []string

	cardSSE := w.openSSE(t, "/v1/custos/cards/events")
	devSSE := w.openSSE(t, "/v1/devices/events")

	// 1. Register
	rails := []surface.DeclaredRail{
		{Name: devices.RailCalendar, ReadOnly: true},
		{Name: devices.RailSMS, ReadOnly: false},
	}
	cardID, regCode, regBody := phone.Register(t, "pixel-g", "Pixel G Phone", rails)
	if regCode != http.StatusAccepted {
		t.Fatalf("register failed: %s", regBody)
	}
	bodies = append(bodies, regBody)

	// 2. Card event
	cardEv := cardSSE.nextEvent(t)
	frames = append(frames, cardEv.Data)

	// Check pending turn cards
	for _, c := range w.hub.PendingTurnCards() {
		bodies = append(bodies, c.Review)
	}

	w.resolveCard(t, cardID)

	// 3. Open stream & receive state event
	phone.OpenStream(t)
	stEv := devSSE.nextEvent(t)
	frames = append(frames, stEv.Data)
	waitFor(t, "device online before action", func() bool {
		return w.devDoor.IsOnline("pixel-g")
	})

	// 4. Sync
	syncCode, syncBody := phone.Sync(t, devices.RailCalendar, []map[string]any{{"title": "confidential"}}, nil, 1)
	if syncCode != http.StatusOK {
		t.Fatalf("sync failed: %s", syncBody)
	}
	bodies = append(bodies, syncBody)

	// 5. Action dispatch & receipt
	smsTool := devices.ToolSMSSend
	modelTool := devices.ToolName("pixel-g", smsTool)
	toolSrc := w.toolSource()
	var actTool *loop.Tool
	for i := range toolSrc {
		if toolSrc[i].Spec.Name == modelTool {
			actTool = &toolSrc[i]
			break
		}
	}
	if actTool == nil {
		t.Fatalf("actTool not found")
	}

	actionCtx, cancelAction := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelAction()
	actionDone := make(chan error, 1)
	go func() {
		_, err := actTool.Run(actionCtx, `{"recipient":"+15551234","message":"canary test"}`)
		actionDone <- err
	}()

	var actionFrame actionDispatchFrame
	select {
	case ev := <-phone.streamEvents:
		if ev.Kind != "action_dispatch" {
			t.Fatalf("expected action_dispatch event, got kind=%s data=%s", ev.Kind, ev.Data)
		}
		if err := json.Unmarshal([]byte(ev.Data), &actionFrame); err != nil {
			t.Fatalf("unmarshal action dispatch: %v", err)
		}
	case err := <-phone.streamErrs:
		t.Fatalf("stream error: %v", err)
	case err := <-actionDone:
		t.Fatalf("action ended before dispatch: %v", err)
	case <-actionCtx.Done():
		t.Fatalf("timed out waiting for action dispatch: %v", actionCtx.Err())
	}
	frames = append(frames, actionFrame.Tool, string(actionFrame.Args))

	rcptCode, rcptBody := phone.SubmitReceipt(t, actionFrame.ActionID, true, map[string]string{"result": "ok"}, "")
	if rcptCode != http.StatusOK {
		t.Fatalf("receipt failed: %s", rcptBody)
	}
	bodies = append(bodies, rcptBody)
	select {
	case err := <-actionDone:
		if err != nil {
			t.Fatalf("action failed: %v", err)
		}
	case <-actionCtx.Done():
		t.Fatalf("timed out waiting for action completion: %v", actionCtx.Err())
	}

	// 6. List and Get rows
	listBody, _ := w.getAdmin("/v1/devices")
	bodies = append(bodies, listBody)

	// 7. Assert pairing token (raw, unprefixed secret hex, and all base64 encodings) never leaks
	needles := []string{pairedTok}
	if strings.HasPrefix(pairedTok, "lar1_") {
		needles = append(needles, strings.TrimPrefix(pairedTok, "lar1_"))
	}
	needles = append(needles, vt3CanaryNeedle)

	for _, needle := range needles {
		for i, b := range bodies {
			assertNoNeedleToken(t, fmt.Sprintf("response body #%d", i), b, needle)
		}
		for i, f := range frames {
			assertNoNeedleToken(t, fmt.Sprintf("SSE frame #%d", i), f, needle)
		}

		// Check all audit records
		for i, l := range readAuditLines(t, w.stateDir) {
			assertNoNeedleToken(t, fmt.Sprintf("audit line #%d", i), l, needle)
		}

		// Check server log output
		assertNoNeedleToken(t, "daemon log output", w.logbuf.String(), needle)
	}
}

// TestVT3h proves: misflag handshake→400 rail_mismatch, zero state written
// (store files byte-identical or absent; no card raised).
func TestVT3h(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	phone.Pair(t, "pixel-h")

	storePath := filepath.Join(w.stateDir, "devices.json")
	initialBytes, _ := os.ReadFile(storePath)
	initialCards := len(w.hub.PendingTurnCards())

	// 1. Read rail misflagged with read_only: false
	misflag1 := []surface.DeclaredRail{
		{Name: devices.RailCalendar, ReadOnly: false}, // Misflagged!
	}
	cardID1, code1, body1 := phone.Register(t, "pixel-h1", "Misflag 1", misflag1)
	if code1 != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", code1, body1)
	}
	if !strings.Contains(body1, `"rail_mismatch"`) {
		t.Fatalf("expected rail_mismatch reason, got %s", body1)
	}
	if cardID1 != "" {
		t.Fatalf("no card should be returned on handshake failure")
	}

	// 2. Action rail misflagged with read_only: true
	misflag2 := []surface.DeclaredRail{
		{Name: devices.RailSMS, ReadOnly: true}, // Misflagged!
	}
	cardID2, code2, body2 := phone.Register(t, "pixel-h2", "Misflag 2", misflag2)
	if code2 != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", code2, body2)
	}
	if !strings.Contains(body2, `"rail_mismatch"`) {
		t.Fatalf("expected rail_mismatch reason, got %s", body2)
	}
	if cardID2 != "" {
		t.Fatalf("no card should be returned on handshake failure")
	}

	// 3. Undeclared non-first-party rail
	misflag3 := []surface.DeclaredRail{
		{Name: "cryptocurrency_miner", ReadOnly: false},
	}
	cardID3, code3, body3 := phone.Register(t, "pixel-h3", "Misflag 3", misflag3)
	if code3 != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", code3, body3)
	}
	if !strings.Contains(body3, `"rail_mismatch"`) {
		t.Fatalf("expected rail_mismatch reason, got %s", body3)
	}
	if cardID3 != "" {
		t.Fatalf("no card should be returned on handshake failure")
	}

	// Assert zero state written
	currentBytes, _ := os.ReadFile(storePath)
	if !bytes.Equal(initialBytes, currentBytes) {
		t.Fatalf("devices.json mutated after misflagged handshakes! initial=%s, current=%s", initialBytes, currentBytes)
	}

	// Assert no card raised
	currentCards := len(w.hub.PendingTurnCards())
	if currentCards != initialCards {
		t.Fatalf("cards raised on misflag: initial=%d, current=%d", initialCards, currentCards)
	}

	// Assert no device registered
	if len(w.devMgr.List()) != 0 {
		t.Fatalf("devices registered despite misflagged handshakes: %+v", w.devMgr.List())
	}
}

// TestVT3i proves: legacy compat — pre-T3 GET /v1/mcp/connectors response shape
// unchanged with a device registered (additive rows only).
func TestVT3i(t *testing.T) {
	w := newVT3World(t)
	phone := w.newPhone(t)

	// 1. Pre-T3 client queries GET /v1/mcp/connectors before any device is registered
	preBody, preCode := w.getAdmin("/v1/mcp/connectors")
	if preCode != http.StatusOK {
		t.Fatalf("GET /v1/mcp/connectors before device = %d: %s", preCode, preBody)
	}

	var preDoc struct {
		Connectors []surface.ConnectorCard `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(preBody), &preDoc); err != nil {
		t.Fatalf("unmarshal pre-T3 GET /v1/mcp/connectors: %v", err)
	}

	// 2. Pair and register device
	phone.Pair(t, "pixel-i")
	rails := []surface.DeclaredRail{
		{Name: devices.RailCalendar, ReadOnly: true},
		{Name: devices.RailSMS, ReadOnly: false},
	}
	cardID, code, body := phone.Register(t, "pixel-i", "Pixel I Phone", rails)
	if code != http.StatusAccepted {
		t.Fatalf("register status = %d: %s", code, body)
	}
	w.resolveCard(t, cardID)
	phone.OpenStream(t)
	phone.Sync(t, devices.RailCalendar, []map[string]any{{"title": "Sprint"}}, nil, 1)

	// 3. Pre-T3 client queries GET /v1/mcp/connectors after device registered
	postBody, postCode := w.getAdmin("/v1/mcp/connectors")
	if postCode != http.StatusOK {
		t.Fatalf("GET /v1/mcp/connectors after device = %d: %s", postCode, postBody)
	}

	var postDoc struct {
		Connectors []surface.ConnectorCard `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(postBody), &postDoc); err != nil {
		t.Fatalf("unmarshal post-T3 GET /v1/mcp/connectors: %v", err)
	}

	// Legacy connector entries are preserved and additive
	if len(postDoc.Connectors) < len(preDoc.Connectors) {
		t.Fatalf("post-T3 connector count %d < pre-T3 connector count %d", len(postDoc.Connectors), len(preDoc.Connectors))
	}
	for i, preConn := range preDoc.Connectors {
		if postDoc.Connectors[i].Name != preConn.Name {
			t.Fatalf("connector row #%d re-ordered or re-typed: pre=%s post=%s", i, preConn.Name, postDoc.Connectors[i].Name)
		}
	}
}

func waitFor(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
