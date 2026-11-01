package surface

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDeviceDoor struct {
	mu           sync.Mutex
	devices      map[string]DeviceCard
	tombstones   map[string]bool
	online       map[string]bool
	lastSeen     map[string]time.Time
	railSeqs     map[string]map[string]int64
	gracePeriod  time.Duration
	actionBroker ActionBroker
	audited      []DeviceAuditPayload
	auditKinds   []string
}

func newFakeDeviceDoor() *fakeDeviceDoor {
	return &fakeDeviceDoor{
		devices:     make(map[string]DeviceCard),
		tombstones:  make(map[string]bool),
		online:      make(map[string]bool),
		lastSeen:    make(map[string]time.Time),
		railSeqs:    make(map[string]map[string]int64),
		gracePeriod: 50 * time.Millisecond,
	}
}

var _ DeviceDoor = (*fakeDeviceDoor)(nil)

var fakeSlugRe = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,31}$")

func (d *fakeDeviceDoor) ValidateID(id string) error {
	if !fakeSlugRe.MatchString(id) {
		return errors.New("invalid slug")
	}
	return nil
}

var fakeClosedRails = map[string]bool{
	"calendar":      true,
	"contacts":      true,
	"notifications": true,
	"calllog":       true,
	"health":        true,
	"dialer":        false,
	"sms":           false,
}

func (d *fakeDeviceDoor) ValidateRails(rails []DeclaredRail) error {
	if len(rails) == 0 {
		return &ErrRailMismatch{Reason: "rail list must not be empty"}
	}
	seen := make(map[string]bool)
	for _, r := range rails {
		if seen[r.Name] {
			return &ErrRailMismatch{Reason: "duplicate rail " + r.Name}
		}
		seen[r.Name] = true
		expRO, ok := fakeClosedRails[r.Name]
		if !ok {
			return &ErrRailMismatch{Reason: "undeclared rail " + r.Name}
		}
		if expRO && !r.ReadOnly {
			return &ErrRailMismatch{Reason: "read rail " + r.Name + " must be read-only"}
		}
		if !expRO && r.ReadOnly {
			return &ErrRailMismatch{Reason: "action rail " + r.Name + " must not be read-only"}
		}
	}
	return nil
}

func (d *fakeDeviceDoor) Register(id, displayName string, rails []DeclaredRail) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.tombstones, id)
	now := time.Now().UTC()
	lsStr := now.Format(time.RFC3339)
	railsCopy := append([]DeclaredRail(nil), rails...)
	d.devices[id] = DeviceCard{
		DeviceID:     id,
		DisplayName:  displayName,
		Rails:        railsCopy,
		RegisteredAt: now.Format(time.RFC3339),
		LastSeen:     &lsStr,
	}
	d.lastSeen[id] = now
	return nil
}

func (d *fakeDeviceDoor) List() []DeviceCard {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DeviceCard, 0, len(d.devices))
	for _, card := range d.devices {
		out = append(out, d.deriveCardLocked(card))
	}
	return out
}

func (d *fakeDeviceDoor) Get(id string) (DeviceCard, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tombstones[id] {
		return DeviceCard{}, ErrDeviceNotFound
	}
	card, ok := d.devices[id]
	if !ok {
		return DeviceCard{}, ErrDeviceNotFound
	}
	return d.deriveCardLocked(card), nil
}

func (d *fakeDeviceDoor) deriveCardLocked(card DeviceCard) DeviceCard {
	card.Rails = append([]DeclaredRail(nil), card.Rails...)
	online := d.online[card.DeviceID]
	hasAction := false
	for _, r := range card.Rails {
		if r.IsEnabled() && !r.ReadOnly {
			hasAction = true
			break
		}
	}
	if online {
		if hasAction {
			card.State = AppStateReadAct
		} else {
			card.State = AppStateReadOnly
		}
		card.Reason = ""
	} else {
		card.State = AppStateNotConnected
		card.Reason = "offline"
	}
	if ls, ok := d.lastSeen[card.DeviceID]; ok && !ls.IsZero() {
		s := ls.UTC().Format(time.RFC3339)
		card.LastSeen = &s
	}
	return card
}

func (d *fakeDeviceDoor) Forget(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.devices, id)
	d.tombstones[id] = true
	delete(d.online, id)
	delete(d.lastSeen, id)
	delete(d.railSeqs, id)
	return nil
}

func (d *fakeDeviceDoor) SetRailEnabled(id, rail string, enabled bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	card, ok := d.devices[id]
	if !ok || d.tombstones[id] {
		return ErrDeviceNotFound
	}
	found := false
	newRails := make([]DeclaredRail, len(card.Rails))
	copy(newRails, card.Rails)
	for i, r := range newRails {
		if r.Name == rail {
			en := enabled
			newRails[i].Enabled = &en
			found = true
			break
		}
	}
	if !found {
		return ErrRailNotFound
	}
	card.Rails = newRails
	d.devices[id] = card
	return nil
}

func (d *fakeDeviceDoor) SetOnline(id string, online bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if online {
		d.online[id] = true
		d.lastSeen[id] = time.Now().UTC()
	} else {
		// Simulate grace window
		time.AfterFunc(d.gracePeriod, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.online[id] = false
		})
	}
}

func (d *fakeDeviceDoor) IsOnline(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.online[id]
}

func (d *fakeDeviceDoor) LastSeen(id string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastSeen[id]
}

func (d *fakeDeviceDoor) ApplySync(id, rail string, snapshot []byte, deltas []DeviceDelta, seq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	card, ok := d.devices[id]
	if !ok || d.tombstones[id] {
		return ErrDeviceNotFound
	}
	var target *DeclaredRail
	for _, r := range card.Rails {
		if r.Name == rail {
			target = &r
			break
		}
	}
	if target == nil {
		return ErrRailNotFound
	}
	if !target.IsEnabled() {
		return ErrRailDisabled
	}

	if d.railSeqs[id] == nil {
		d.railSeqs[id] = make(map[string]int64)
	}
	cur := d.railSeqs[id][rail]
	if seq <= cur {
		return &ErrStaleSeq{DeviceID: id, Rail: rail, Current: cur, Proposed: seq}
	}
	d.railSeqs[id][rail] = seq
	d.lastSeen[id] = time.Now().UTC()
	return nil
}

func (d *fakeDeviceDoor) SetActionBroker(broker ActionBroker) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.actionBroker = broker
}

func (d *fakeDeviceDoor) Audit(kind string, payload DeviceAuditPayload) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.auditKinds = append(d.auditKinds, kind)
	d.audited = append(d.audited, payload)
	return nil
}

func (d *fakeDeviceDoor) GetGracePeriod() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gracePeriod
}

func (d *fakeDeviceDoor) SetGracePeriod(dura time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gracePeriod = dura
}

type testDeviceEnv struct {
	srv    *Server
	h      http.Handler
	tok    string
	door   *fakeDeviceDoor
	hub    *ApprovalHub
	logBuf *bytes.Buffer
}

func setupDeviceEnv(t *testing.T, withDoor bool) *testDeviceEnv {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("phone-bearer-token")
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	hub := NewApprovalHub(time.Minute)
	srv.SetApprovalHub(hub)

	door := newFakeDeviceDoor()
	if withDoor {
		srv.SetDeviceDoor(door)
	}
	h := NewServer(srv)

	var mu sync.Mutex
	buf := &lockedBuf{mu: &mu}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	return &testDeviceEnv{
		srv:    srv,
		h:      h,
		tok:    tok,
		door:   door,
		hub:    hub,
		logBuf: &buf.Buffer,
	}
}

func (e *testDeviceEnv) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+e.tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func (e *testDeviceEnv) doNoAuth(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
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

func assertNoNeedleText(t *testing.T, label, data, needle string) {
	t.Helper()
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
// 1. POST /v1/devices/register tests
// ---------------------------------------------------------------------------

func TestDevicesRegister_SuccessApproval(t *testing.T) {
	e := setupDeviceEnv(t, true)

	body := `{"device_id":"pixel-9","display_name":"Pixel Nine","rails_declared":[{"name":"calendar","read_only":true},{"name":"sms","read_only":false}]}`
	w := e.do(http.MethodPost, "/v1/devices/register", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("register status = %d, want 202: %s", w.Code, w.Body.String())
	}

	var res struct {
		DeviceID string `json:"device_id"`
		CardID   string `json:"card_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if res.DeviceID != "pixel-9" || res.CardID == "" {
		t.Fatalf("unexpected response: %+v", res)
	}

	// Verify custody card raised with correct review text
	cards := e.hub.PendingTurnCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	card := cards[0]
	if card.ID != res.CardID {
		t.Fatalf("card ID mismatch: %q != %q", card.ID, res.CardID)
	}
	if !strings.Contains(card.Review, "Pixel Nine") ||
		!strings.Contains(card.Review, "pixel-9") ||
		!strings.Contains(card.Review, "calendar [read_only]") ||
		!strings.Contains(card.Review, "sms [action]") ||
		!strings.Contains(card.Review, "fingerprint:") {
		t.Fatalf("card review text missing required review items:\n%s", card.Review)
	}

	// Approve card
	code := e.hub.Resolve("", res.CardID, true)
	if code != http.StatusOK {
		t.Fatalf("resolve status = %d", code)
	}

	// Wait for async persistence
	waitFor(t, "device registered", func() bool {
		dev, err := e.door.Get("pixel-9")
		return err == nil && dev.DisplayName == "Pixel Nine"
	})

	// Verify audit
	e.door.mu.Lock()
	foundAudit := false
	for i, k := range e.door.auditKinds {
		if k == AuditKindDeviceRegistered && e.door.audited[i].DeviceID == "pixel-9" {
			foundAudit = true
			if e.door.audited[i].DisplayName != "Pixel Nine" {
				t.Fatalf("audit DisplayName = %q", e.door.audited[i].DisplayName)
			}
			if len(e.door.audited[i].RailNames) != 2 {
				t.Fatalf("audit RailNames count = %d", len(e.door.audited[i].RailNames))
			}
		}
	}
	e.door.mu.Unlock()
	if !foundAudit {
		t.Fatalf("expected device_registered audit entry")
	}
}

func TestDevicesRegister_DenyLeavesZeroResidue(t *testing.T) {
	e := setupDeviceEnv(t, true)

	body := `{"device_id":"pixel-deny","display_name":"Deny Me","rails_declared":[{"name":"calendar","read_only":true}]}`
	w := e.do(http.MethodPost, "/v1/devices/register", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("register status = %d", w.Code)
	}

	var res struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)

	// Deny card
	code := e.hub.Resolve("", res.CardID, false)
	if code != http.StatusOK {
		t.Fatalf("resolve status = %d", code)
	}

	time.Sleep(20 * time.Millisecond)

	// Zero residue
	if _, err := e.door.Get("pixel-deny"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound after denial, got %v", err)
	}

	// Verify audit
	e.door.mu.Lock()
	foundDenyAudit := false
	for i, k := range e.door.auditKinds {
		if k == AuditKindDeviceRegistrationDenied && e.door.audited[i].DeviceID == "pixel-deny" {
			foundDenyAudit = true
		}
	}
	e.door.mu.Unlock()
	if !foundDenyAudit {
		t.Fatalf("expected device_registration_denied audit")
	}
}

func TestDevicesRegister_ValidationErrors(t *testing.T) {
	e := setupDeviceEnv(t, true)

	cases := []struct {
		name       string
		body       string
		wantCode   int
		wantReason string
	}{
		{
			name:       "invalid slug upper",
			body:       `{"device_id":"BAD_SLUG","rails_declared":[{"name":"calendar","read_only":true}]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "bad_device_id",
		},
		{
			name:       "invalid slug hyphen start",
			body:       `{"device_id":"-pixel","rails_declared":[{"name":"calendar","read_only":true}]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "bad_device_id",
		},
		{
			name:       "empty rails",
			body:       `{"device_id":"pixel","rails_declared":[]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "rail_mismatch",
		},
		{
			name:       "undeclared rail",
			body:       `{"device_id":"pixel","rails_declared":[{"name":"ssh_tunnel","read_only":true}]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "rail_mismatch",
		},
		{
			name:       "misflagged read rail",
			body:       `{"device_id":"pixel","rails_declared":[{"name":"calendar","read_only":false}]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "rail_mismatch",
		},
		{
			name:       "misflagged action rail",
			body:       `{"device_id":"pixel","rails_declared":[{"name":"sms","read_only":true}]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "rail_mismatch",
		},
		{
			name:       "duplicate rail",
			body:       `{"device_id":"pixel","rails_declared":[{"name":"calendar","read_only":true},{"name":"calendar","read_only":true}]}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "rail_mismatch",
		},
		{
			name:       "invalid json",
			body:       `{not json}`,
			wantCode:   http.StatusBadRequest,
			wantReason: "bad_request",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do(http.MethodPost, "/v1/devices/register", tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["reason"] != tc.wantReason {
				t.Fatalf("reason = %q, want %q", resp["reason"], tc.wantReason)
			}
		})
	}
}

func TestDevicesRegister_DuplicateAndPending(t *testing.T) {
	e := setupDeviceEnv(t, true)

	// Register device A
	bodyA := `{"device_id":"dev-dup","display_name":"Dev Dup","rails_declared":[{"name":"calendar","read_only":true}]}`
	w1 := e.do(http.MethodPost, "/v1/devices/register", bodyA)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first register failed: %d", w1.Code)
	}

	// Immediate second register while pending -> 409 pending_add
	w2 := e.do(http.MethodPost, "/v1/devices/register", bodyA)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w2.Code)
	}
	var errResp map[string]string
	_ = json.Unmarshal(w2.Body.Bytes(), &errResp)
	if errResp["reason"] != "pending_add" {
		t.Fatalf("reason = %q, want pending_add", errResp["reason"])
	}

	// Approve first card
	var cardRes struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &cardRes)
	e.hub.Resolve("", cardRes.CardID, true)

	waitFor(t, "first device approved", func() bool {
		_, err := e.door.Get("dev-dup")
		return err == nil
	})

	// Third register after committed -> 409 duplicate_name
	w3 := e.do(http.MethodPost, "/v1/devices/register", bodyA)
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w3.Code)
	}
	_ = json.Unmarshal(w3.Body.Bytes(), &errResp)
	if errResp["reason"] != "duplicate_name" {
		t.Fatalf("reason = %q, want duplicate_name", errResp["reason"])
	}

	// Delete device -> tombstoned
	wDel := e.do(http.MethodDelete, "/v1/devices/dev-dup", "")
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", wDel.Code)
	}

	// Re-registration of tombstoned device succeeds with fresh card per spec D3
	wRe := e.do(http.MethodPost, "/v1/devices/register", bodyA)
	if wRe.Code != http.StatusAccepted {
		t.Fatalf("re-registration code = %d, want 202: %s", wRe.Code, wRe.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 2. GET /v1/devices/<id>/stream & Action Dispatch / Receipt tests
// ---------------------------------------------------------------------------

func TestDevicesStream_LivenessAndConcurrentTakeover(t *testing.T) {
	e := setupDeviceEnv(t, true)

	// Register device
	rails := []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
		{Name: "sms", ReadOnly: false},
	}
	_ = e.door.Register("stream-dev", "Stream Phone", rails)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	req1 := httptest.NewRequest(http.MethodGet, "/v1/devices/stream-dev/stream", nil).WithContext(ctx1)
	req1.Host = "127.0.0.1:7717"
	req1.Header.Set("Authorization", "Bearer "+e.tok)
	w1 := newSafeRecorder()

	streamDone1 := make(chan struct{})
	go func() {
		defer close(streamDone1)
		e.h.ServeHTTP(w1, req1)
	}()

	// Verify online
	waitFor(t, "stream 1 online", func() bool {
		return e.door.IsOnline("stream-dev")
	})

	// Check GET /v1/devices row shows read+act
	listW := e.do(http.MethodGet, "/v1/devices", "")
	if !strings.Contains(listW.Body.String(), `"state":"read+act"`) {
		t.Fatalf("expected state read+act in row: %s", listW.Body.String())
	}

	// Connect second concurrent stream -> takes over, old stream closes
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	req2 := httptest.NewRequest(http.MethodGet, "/v1/devices/stream-dev/stream", nil).WithContext(ctx2)
	req2.Host = "127.0.0.1:7717"
	req2.Header.Set("Authorization", "Bearer "+e.tok)
	w2 := newSafeRecorder()

	streamDone2 := make(chan struct{})
	go func() {
		defer close(streamDone2)
		e.h.ServeHTTP(w2, req2)
	}()

	// Stream 1 should exit
	select {
	case <-streamDone1:
		// OK! stream 1 closed because stream 2 took over
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for stream 1 to close on takeover")
	}

	// Device should remain online
	if !e.door.IsOnline("stream-dev") {
		t.Fatalf("expected device to remain online after stream 2 takeover")
	}

	// Disconnect stream 2
	cancel2()
	select {
	case <-streamDone2:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for stream 2 to close")
	}

	// After grace period, device transitions to offline
	waitFor(t, "device offline after grace", func() bool {
		return !e.door.IsOnline("stream-dev")
	})

	// Check row state is not-connected with reason offline
	listW2 := e.do(http.MethodGet, "/v1/devices", "")
	if !strings.Contains(listW2.Body.String(), `"state":"not-connected"`) ||
		!strings.Contains(listW2.Body.String(), `"reason":"offline"`) {
		t.Fatalf("expected not-connected/offline: %s", listW2.Body.String())
	}
}

func TestDevicesStream_ActionDispatchAndReceipt(t *testing.T) {
	e := setupDeviceEnv(t, true)

	rails := []DeclaredRail{
		{Name: "sms", ReadOnly: false},
	}
	_ = e.door.Register("act-dev", "Action Phone", rails)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/v1/devices/act-dev/stream", nil).WithContext(ctx)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+e.tok)
	w := newSafeRecorder()

	go e.h.ServeHTTP(w, req)

	waitFor(t, "act-dev online", func() bool {
		return e.door.IsOnline("act-dev")
	})

	broker := e.srv.DeviceBroker()
	if broker == nil {
		t.Fatalf("broker is nil")
	}

	// Run Dispatch in background
	type dispatchOutcome struct {
		res json.RawMessage
		err error
	}
	outCh := make(chan dispatchOutcome, 1)
	go func() {
		args := json.RawMessage(`{"recipient":"+15551234567","message":"Hi there"}`)
		res, err := broker.Dispatch(context.Background(), "act-dev", "sms_send", args)
		outCh <- dispatchOutcome{res: res, err: err}
	}()

	// Wait for frame to appear in w.Body
	waitFor(t, "action_dispatch frame in stream", func() bool {
		return strings.Contains(w.BodyString(), "event: action_dispatch")
	})

	frameText := w.BodyString()
	// Extract action_id from data: {"action_id":"act_..."}
	actRe := regexp.MustCompile(`"action_id":"([^"]+)"`)
	matches := actRe.FindStringSubmatch(frameText)
	if len(matches) < 2 {
		t.Fatalf("did not find action_id in stream frame:\n%s", frameText)
	}
	actionID := matches[1]

	// Submit receipt
	receiptBody := `{"ok":true,"result":{"message_id":"msg_999"}}`
	receiptW := e.do(http.MethodPost, "/v1/devices/act-dev/actions/"+actionID+"/receipt", receiptBody)
	if receiptW.Code != http.StatusOK {
		t.Fatalf("receipt status = %d: %s", receiptW.Code, receiptW.Body.String())
	}

	// Dispatch should resolve with success
	select {
	case out := <-outCh:
		if out.err != nil {
			t.Fatalf("dispatch error: %v", out.err)
		}
		if !strings.Contains(string(out.res), "msg_999") {
			t.Fatalf("unexpected dispatch result: %s", string(out.res))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatch did not resolve after receipt")
	}

	// Submitting same receipt again -> 410 stale
	repeatW := e.do(http.MethodPost, "/v1/devices/act-dev/actions/"+actionID+"/receipt", receiptBody)
	if repeatW.Code != http.StatusGone {
		t.Fatalf("repeated receipt status = %d, want 410", repeatW.Code)
	}

	// Submitting receipt for unknown action_id -> 410
	badW := e.do(http.MethodPost, "/v1/devices/act-dev/actions/act_unknown/receipt", receiptBody)
	if badW.Code != http.StatusGone {
		t.Fatalf("unknown receipt status = %d, want 410", badW.Code)
	}
}

func TestDevicesActionBroker_OfflineFastFails(t *testing.T) {
	e := setupDeviceEnv(t, true)

	broker := e.srv.DeviceBroker()
	if broker == nil {
		t.Fatalf("broker is nil")
	}

	// No stream connected -> fast-fails immediately
	_, err := broker.Dispatch(context.Background(), "no-such-dev", "sms_send", json.RawMessage(`{}`))
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("expected ErrDeviceOffline, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. POST /v1/devices/<id>/sync tests
// ---------------------------------------------------------------------------

func TestDevicesSync_Matrix(t *testing.T) {
	e := setupDeviceEnv(t, true)

	rails := []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
		{Name: "contacts", ReadOnly: true},
	}
	_ = e.door.Register("sync-phone", "Sync Phone", rails)

	// 1. Happy path: seq 1
	sync1 := `{"rail":"calendar","snapshot":[{"id":"ev1","title":"Lunch"}],"seq":1}`
	w1 := e.do(http.MethodPost, "/v1/devices/sync-phone/sync", sync1)
	if w1.Code != http.StatusOK {
		t.Fatalf("sync 1 failed: %d %s", w1.Code, w1.Body.String())
	}

	// 2. Stale seq (seq 1 again) -> 409 stale_seq
	wStale := e.do(http.MethodPost, "/v1/devices/sync-phone/sync", sync1)
	if wStale.Code != http.StatusConflict {
		t.Fatalf("stale sync code = %d, want 409", wStale.Code)
	}
	var res map[string]string
	_ = json.Unmarshal(wStale.Body.Bytes(), &res)
	if res["reason"] != "stale_seq" {
		t.Fatalf("reason = %q, want stale_seq", res["reason"])
	}

	// 3. Unknown rail -> 400 unknown_rail
	syncUnknown := `{"rail":"telemetry","snapshot":[],"seq":1}`
	wUnk := e.do(http.MethodPost, "/v1/devices/sync-phone/sync", syncUnknown)
	if wUnk.Code != http.StatusBadRequest {
		t.Fatalf("unknown rail code = %d, want 400", wUnk.Code)
	}
	_ = json.Unmarshal(wUnk.Body.Bytes(), &res)
	if res["reason"] != "unknown_rail" {
		t.Fatalf("reason = %q, want unknown_rail", res["reason"])
	}

	// 4. Disabled rail -> 409 rail_disabled per V-T3e
	_ = e.door.SetRailEnabled("sync-phone", "contacts", false)
	syncDisabled := `{"rail":"contacts","snapshot":[],"seq":1}`
	wDis := e.do(http.MethodPost, "/v1/devices/sync-phone/sync", syncDisabled)
	if wDis.Code != http.StatusConflict {
		t.Fatalf("disabled rail code = %d, want 409", wDis.Code)
	}
	_ = json.Unmarshal(wDis.Body.Bytes(), &res)
	if res["reason"] != "rail_disabled" {
		t.Fatalf("reason = %q, want rail_disabled", res["reason"])
	}

	// 5. Unknown device -> 404 not_found
	wDev404 := e.do(http.MethodPost, "/v1/devices/nonexistent/sync", sync1)
	if wDev404.Code != http.StatusNotFound {
		t.Fatalf("unknown dev code = %d, want 404", wDev404.Code)
	}
}

// ---------------------------------------------------------------------------
// 4. Admin routes: GET /v1/devices, PATCH rails, DELETE forget
// ---------------------------------------------------------------------------

func TestDevicesAdmin_GetRowsStateDerivation(t *testing.T) {
	e := setupDeviceEnv(t, true)

	// Device 1: online with action rail -> read+act
	_ = e.door.Register("dev-act", "Act Dev", []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
		{Name: "sms", ReadOnly: false},
	})
	e.door.SetOnline("dev-act", true)

	// Device 2: online with only read rails -> read-only
	_ = e.door.Register("dev-ro", "RO Dev", []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
	})
	e.door.SetOnline("dev-ro", true)

	// Device 3: offline -> not-connected
	_ = e.door.Register("dev-off", "Off Dev", []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
	})
	// not marked online

	// Pending device -> not-connected / pending_approval
	e.devicePending("dev-pend", "Pending Dev")

	w := e.do(http.MethodGet, "/v1/devices", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		Devices []DeviceCard `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(res.Devices) != 4 {
		t.Fatalf("expected 4 devices, got %d", len(res.Devices))
	}

	byID := map[string]DeviceCard{}
	for _, dev := range res.Devices {
		byID[dev.DeviceID] = dev
	}

	if byID["dev-act"].State != AppStateReadAct {
		t.Fatalf("dev-act State = %q, want read+act", byID["dev-act"].State)
	}
	if byID["dev-ro"].State != AppStateReadOnly {
		t.Fatalf("dev-ro State = %q, want read-only", byID["dev-ro"].State)
	}
	if byID["dev-off"].State != AppStateNotConnected || byID["dev-off"].Reason != "offline" {
		t.Fatalf("dev-off = %+v, want not-connected/offline", byID["dev-off"])
	}
	if byID["dev-pend"].State != AppStateNotConnected || byID["dev-pend"].Reason != connReasonPendingAppvl {
		t.Fatalf("dev-pend = %+v, want not-connected/pending_approval", byID["dev-pend"])
	}

	// Single device GET /v1/devices/<id>
	wSingle := e.do(http.MethodGet, "/v1/devices/dev-act", "")
	if wSingle.Code != http.StatusOK {
		t.Fatalf("single GET status = %d", wSingle.Code)
	}
	var card DeviceCard
	_ = json.Unmarshal(wSingle.Body.Bytes(), &card)
	if card.DeviceID != "dev-act" || card.State != AppStateReadAct {
		t.Fatalf("single card = %+v", card)
	}
}

func (e *testDeviceEnv) devicePending(id, name string) {
	e.srv.devicePendingMu.Lock()
	if e.srv.devicePending == nil {
		e.srv.devicePending = make(map[string]*pendingDevice)
	}
	e.srv.devicePending[id] = &pendingDevice{
		deviceID:     id,
		displayName:  name,
		rails:        []DeclaredRail{{Name: "calendar", ReadOnly: true}},
		registeredAt: time.Now().UTC(),
	}
	e.srv.devicePendingMu.Unlock()
}

func TestDevicesAdmin_PatchRailsToggle(t *testing.T) {
	e := setupDeviceEnv(t, true)

	_ = e.door.Register("patch-dev", "Patch Dev", []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
		{Name: "sms", ReadOnly: false},
	})

	// Toggle disable calendar
	patchBody := `{"rail":"calendar","enabled":false}`
	w := e.do(http.MethodPatch, "/v1/devices/patch-dev/rails", patchBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("patch status = %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)

	// Review text check
	cards := e.hub.PendingTurnCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if !strings.Contains(cards[0].Review, "TOGGLE rail") || !strings.Contains(cards[0].Review, "calendar") {
		t.Fatalf("review text mismatch:\n%s", cards[0].Review)
	}

	// Approve toggle
	e.hub.Resolve("", res.CardID, true)

	waitFor(t, "rail disabled", func() bool {
		dev, err := e.door.Get("patch-dev")
		if err != nil {
			return false
		}
		for _, r := range dev.Rails {
			if r.Name == "calendar" {
				return !r.IsEnabled()
			}
		}
		return false
	})

	// Verify audit device_rail_toggled
	e.door.mu.Lock()
	foundAudit := false
	for i, k := range e.door.auditKinds {
		if k == AuditKindDeviceRailToggled && e.door.audited[i].DeviceID == "patch-dev" {
			foundAudit = true
		}
	}
	e.door.mu.Unlock()
	if !foundAudit {
		t.Fatalf("expected device_rail_toggled audit")
	}

	// Unknown rail -> 400
	wUnk := e.do(http.MethodPatch, "/v1/devices/patch-dev/rails", `{"rail":"ghost","enabled":false}`)
	if wUnk.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on unknown rail, got %d", wUnk.Code)
	}

	// Unknown device -> 404
	wDev := e.do(http.MethodPatch, "/v1/devices/ghost/rails", `{"rail":"calendar","enabled":false}`)
	if wDev.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on unknown dev, got %d", wDev.Code)
	}
}

func TestDevicesAdmin_DeleteForget(t *testing.T) {
	e := setupDeviceEnv(t, true)

	_ = e.door.Register("del-dev", "Del Dev", []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
	})

	w := e.do(http.MethodDelete, "/v1/devices/del-dev", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}

	// Gone from GET
	_, err := e.door.Get("del-dev")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound, got %v", err)
	}

	// Audit device_forgotten
	e.door.mu.Lock()
	foundAudit := false
	for i, k := range e.door.auditKinds {
		if k == AuditKindDeviceForgotten && e.door.audited[i].DeviceID == "del-dev" {
			foundAudit = true
		}
	}
	e.door.mu.Unlock()
	if !foundAudit {
		t.Fatalf("expected device_forgotten audit")
	}
}

// ---------------------------------------------------------------------------
// 5. SSE Feeds: GET /v1/devices/events & GET /v1/devices/<id>/events
// ---------------------------------------------------------------------------

func TestDevicesSSE_FeedsAndEvents(t *testing.T) {
	e := setupDeviceEnv(t, true)

	_ = e.door.Register("sse-dev", "SSE Phone", []DeclaredRail{
		{Name: "calendar", ReadOnly: true},
	})

	// Start global SSE listener
	ctxGlobal, cancelGlobal := context.WithCancel(context.Background())
	defer cancelGlobal()

	reqGlobal := httptest.NewRequest(http.MethodGet, "/v1/devices/events", nil).WithContext(ctxGlobal)
	reqGlobal.Host = "127.0.0.1:7717"
	reqGlobal.Header.Set("Authorization", "Bearer "+e.tok)
	wGlobal := newSafeRecorder()

	go e.h.ServeHTTP(wGlobal, reqGlobal)

	// Start per-device SSE listener
	ctxDev, cancelDev := context.WithCancel(context.Background())
	defer cancelDev()

	reqDev := httptest.NewRequest(http.MethodGet, "/v1/devices/sse-dev/events", nil).WithContext(ctxDev)
	reqDev.Host = "127.0.0.1:7717"
	reqDev.Header.Set("Authorization", "Bearer "+e.tok)
	wDev := newSafeRecorder()

	go e.h.ServeHTTP(wDev, reqDev)

	// Both should start silent (no initial burst)
	time.Sleep(50 * time.Millisecond)

	// Toggle state online -> triggers device_state
	e.door.SetOnline("sse-dev", true)
	e.srv.pokeDeviceRail()

	waitFor(t, "device_state in global SSE", func() bool {
		return strings.Contains(wGlobal.BodyString(), "event: device_state")
	})
	waitFor(t, "device_state in per-device SSE", func() bool {
		return strings.Contains(wDev.BodyString(), "event: device_state")
	})

	// Delete device -> triggers device_gone
	delW := e.do(http.MethodDelete, "/v1/devices/sse-dev", "")
	if delW.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", delW.Code)
	}

	waitFor(t, "device_gone in global SSE", func() bool {
		return strings.Contains(wGlobal.BodyString(), "event: device_gone")
	})
	waitFor(t, "device_gone in per-device SSE", func() bool {
		return strings.Contains(wDev.BodyString(), "event: device_gone")
	})

	if !strings.Contains(wGlobal.BodyString(), `"device_id":"sse-dev"`) {
		t.Fatalf("global SSE missing device_id in gone event:\n%s", wGlobal.BodyString())
	}
	if !strings.Contains(wDev.BodyString(), `"device_id":"sse-dev"`) {
		t.Fatalf("per-device SSE missing device_id in gone event:\n%s", wDev.BodyString())
	}
}

// ---------------------------------------------------------------------------
// 6. Never-leak law tests (Deliverable 4)
// ---------------------------------------------------------------------------

func TestDevices_NeverLeakDoctrine(t *testing.T) {
	e := setupDeviceEnv(t, true)

	// Inject distinctive needle into a paired bearer token
	needle := "CANARY-DEVICE-NEEDLE-SECRET-9876543210"
	store := e.srv.Store
	// Create token with needle in plaintext
	tokWithNeedle := "lar1_" + needle
	// We manually insert the hash into the store to simulate a paired phone holding this token
	h := sha256.Sum256([]byte(tokWithNeedle))
	store.writeEntries([]tokenEntry{
		{Hash: hex.EncodeToString(h[:]), Label: "paired-phone-canary", Created: time.Now().UTC().Format(time.RFC3339)},
	})

	// Use this needle token for all interactions
	e.tok = tokWithNeedle

	// 1. Register device
	regBody := `{"device_id":"needle-dev","display_name":"Needle Phone","rails_declared":[{"name":"calendar","read_only":true},{"name":"sms","read_only":false}]}`
	regW := e.do(http.MethodPost, "/v1/devices/register", regBody)
	assertNoNeedleText(t, "register response", regW.Body.String(), needle)

	var regRes struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal(regW.Body.Bytes(), &regRes)

	// Check custody card review text
	cards := e.hub.PendingTurnCards()
	for _, c := range cards {
		assertNoNeedleText(t, "custody card review", c.Review, needle)
	}

	// Approve
	e.hub.Resolve("", regRes.CardID, true)
	waitFor(t, "needle device registered", func() bool {
		_, err := e.door.Get("needle-dev")
		return err == nil
	})

	// 2. List devices
	listW := e.do(http.MethodGet, "/v1/devices", "")
	assertNoNeedleText(t, "list response", listW.Body.String(), needle)

	// 3. Connect stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamReq := httptest.NewRequest(http.MethodGet, "/v1/devices/needle-dev/stream", nil).WithContext(ctx)
	streamReq.Host = "127.0.0.1:7717"
	streamReq.Header.Set("Authorization", "Bearer "+e.tok)
	streamW := newSafeRecorder()
	go e.h.ServeHTTP(streamW, streamReq)

	waitFor(t, "stream connected", func() bool {
		return e.door.IsOnline("needle-dev")
	})

	// Dispatch action
	go func() {
		broker := e.srv.DeviceBroker()
		_, _ = broker.Dispatch(context.Background(), "needle-dev", "sms_send", json.RawMessage(`{"recipient":"123","message":"hi"}`))
	}()

	waitFor(t, "stream frame received", func() bool {
		return strings.Contains(streamW.BodyString(), "event: action_dispatch")
	})
	assertNoNeedleText(t, "stream body", streamW.BodyString(), needle)

	// Extract action_id
	actRe := regexp.MustCompile(`"action_id":"([^"]+)"`)
	m := actRe.FindStringSubmatch(streamW.BodyString())
	if len(m) >= 2 {
		actionID := m[1]
		// Submit receipt
		recW := e.do(http.MethodPost, "/v1/devices/needle-dev/actions/"+actionID+"/receipt", `{"ok":true}`)
		assertNoNeedleText(t, "receipt response", recW.Body.String(), needle)
	}

	// 4. Sync
	syncW := e.do(http.MethodPost, "/v1/devices/needle-dev/sync", `{"rail":"calendar","snapshot":[],"seq":1}`)
	assertNoNeedleText(t, "sync response", syncW.Body.String(), needle)

	// 5. Patch toggle
	patchW := e.do(http.MethodPatch, "/v1/devices/needle-dev/rails", `{"rail":"calendar","enabled":false}`)
	assertNoNeedleText(t, "patch response", patchW.Body.String(), needle)

	// 6. Delete
	delW := e.do(http.MethodDelete, "/v1/devices/needle-dev", "")
	assertNoNeedleText(t, "delete response", delW.Body.String(), needle)

	// 7. Check server logs
	assertNoNeedleText(t, "server logs", e.logBuf.String(), needle)
	needleSecondary := "CANARY-DEVICE-NEEDLE-SECOND-1122334455"
	assertNoNeedleText(t, "server logs secondary", e.logBuf.String(), needleSecondary)

	// 8. Check audit records
	e.door.mu.Lock()
	for _, a := range e.door.audited {
		assertNoNeedleText(t, "audit display_name", a.DisplayName, needle)
		assertNoNeedleText(t, "audit cred_name", a.CredName, needle)
		assertNoNeedleText(t, "audit reason", a.Reason, needle)
		assertNoNeedleText(t, "audit device_id", a.DeviceID, needle)
		assertNoNeedleText(t, "audit fingerprint", a.Fingerprint, needle)
	}
	e.door.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 7. Bearer Auth Gating tests
// ---------------------------------------------------------------------------

func TestDevicesAuth_AllRoutesBearerGated(t *testing.T) {
	e := setupDeviceEnv(t, true)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/devices"},
		{http.MethodPost, "/v1/devices/register"},
		{http.MethodGet, "/v1/devices/events"},
		{http.MethodGet, "/v1/devices/dev-1/stream"},
		{http.MethodPost, "/v1/devices/dev-1/sync"},
		{http.MethodGet, "/v1/devices/dev-1/events"},
		{http.MethodPatch, "/v1/devices/dev-1/rails"},
		{http.MethodDelete, "/v1/devices/dev-1"},
		{http.MethodPost, "/v1/devices/dev-1/actions/act-1/receipt"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := e.doNoAuth(rt.method, rt.path, `{}`)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized without bearer token, got %d for %s %s", w.Code, rt.method, rt.path)
			}
		})
	}
}

func TestDevices_NoDoorServiceUnavailable(t *testing.T) {
	e := setupDeviceEnv(t, false)
	// GET /v1/devices returns 200 with empty list when door is nil (UI polling).
	listW := e.do(http.MethodGet, "/v1/devices", "")
	if listW.Code != http.StatusOK {
		t.Fatalf("expected 200 when door is nil for list, got %d", listW.Code)
	}

	// Mutating operations requiring door return 503 Service Unavailable.
	regW := e.do(http.MethodPost, "/v1/devices/register", `{"device_id":"dev1","display_name":"Dev","rails_declared":[{"name":"sms","read_only":false}]}`)
	if regW.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when door is nil for register, got %d", regW.Code)
	}
}
