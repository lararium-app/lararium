package surface

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type attributedTestRegistry struct {
	mu       sync.Mutex
	lastID   string
	lastVerd string
	lastVia  string
	lastAttr CustosAttribution
	calls    int
	err      error
}

func (r *attributedTestRegistry) Snapshot() []CustosCard                          { return nil }
func (r *attributedTestRegistry) Subscribe(func(*CustosCard, *CustosGone)) func() { return func() {} }

func (r *attributedTestRegistry) Resolve(id, verdict string) (string, error) {
	return "", ErrNoSuchCard
}

func (r *attributedTestRegistry) Attach() ([]CustosCard, func(), <-chan CustosEvent) {
	return nil, func() {}, nil
}

func (r *attributedTestRegistry) ResolveAttributed(id, verdict, via string, attr CustosAttribution) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastID = id
	r.lastVerd = verdict
	r.lastVia = via
	r.lastAttr = attr
	if r.err != nil {
		return "", r.err
	}
	return registryAnswerState(verdict), nil
}

func (r *attributedTestRegistry) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type fakeAttrDeviceDoor struct {
	mu      sync.Mutex
	devices map[string]DeviceCard
	errByID map[string]error
}

func (d *fakeAttrDeviceDoor) ValidateID(string) error                       { return nil }
func (d *fakeAttrDeviceDoor) ValidateRails([]DeclaredRail) error            { return nil }
func (d *fakeAttrDeviceDoor) Register(string, string, []DeclaredRail) error { return nil }
func (d *fakeAttrDeviceDoor) List() []DeviceCard                            { return nil }

// Forget tombstones the device so subsequent Get calls fail, mirroring the
// real Manager semantics the amendment v16 gate depends on.
func (d *fakeAttrDeviceDoor) Forget(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.devices, id)
	d.errByID[id] = ErrDeviceNotFound
	return nil
}
func (d *fakeAttrDeviceDoor) SetRailEnabled(string, string, bool) error { return nil }
func (d *fakeAttrDeviceDoor) SetOnline(string, bool)                    {}
func (d *fakeAttrDeviceDoor) IsOnline(string) bool                      { return false }
func (d *fakeAttrDeviceDoor) LastSeen(string) time.Time                 { return time.Time{} }
func (d *fakeAttrDeviceDoor) ApplySync(string, string, []byte, []DeviceDelta, int64) error {
	return nil
}
func (d *fakeAttrDeviceDoor) SetActionBroker(ActionBroker)           {}
func (d *fakeAttrDeviceDoor) Audit(string, DeviceAuditPayload) error { return nil }
func (d *fakeAttrDeviceDoor) GetGracePeriod() time.Duration          { return 0 }
func (d *fakeAttrDeviceDoor) SetGracePeriod(time.Duration)           {}
func (d *fakeAttrDeviceDoor) Get(deviceID string) (DeviceCard, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err, ok := d.errByID[deviceID]; ok {
		return DeviceCard{}, err
	}
	if card, ok := d.devices[deviceID]; ok {
		return card, nil
	}
	return DeviceCard{}, ErrDeviceNotFound
}

func dialAuthenticatedWSWithUA(t *testing.T, tsURL, token, userAgent string) *testWSClient {
	t.Helper()
	u, err := url.Parse(tsURL + "/v1/ws")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", u.Host, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var nonceBytes [16]byte
	if _, err := io.ReadFull(rand.Reader, nonceBytes[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(nonceBytes[:])
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	reqStr := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"User-Agent: %s\r\n\r\n", u.RequestURI(), u.Host, key, userAgent)
	if _, err := bw.WriteString(reqStr); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush handshake: %v", err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("ws upgrade status = %d, want 101", resp.StatusCode)
	}

	client := &testWSClient{conn: conn, br: br, bw: bw}
	hello, err := EncodeRPCRequest(0, "auth.hello", map[string]string{"token": token})
	if err != nil {
		t.Fatalf("encode auth.hello: %v", err)
	}
	client.writeMaskedText(t, hello)
	op, payload := client.readServerFrame(t)
	if op != wsOpText {
		t.Fatalf("auth.hello opcode = %d", op)
	}
	helloResp, err := DecodeRPCResponse(payload)
	if err != nil || helloResp.Error != nil {
		t.Fatalf("auth.hello resp=%+v err=%v", helloResp, err)
	}
	return client
}

func TestHTTPCustosResolveAttribution(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := &attributedTestRegistry{}
	door := &fakeAttrDeviceDoor{
		devices: map[string]DeviceCard{
			"phone-alpha": {DeviceID: "phone-alpha", DisplayName: "Alpha Phone"},
		},
		errByID: map[string]error{},
	}
	srv.SetCustosRegistry(reg)
	srv.SetDeviceDoor(door)

	devTok, err := tokStore.CreateDeviceToken("dev-alpha", "phone-alpha", "Alpha Phone", "ios", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	ownerTok, err := tokStore.Create("owner-web")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("BoundDeviceToken", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000001/resolve",
			strings.NewReader(`{"verdict":"once","user_agent":"SpoofedBody/9.9"}`))
		req.Header.Set("Authorization", "Bearer "+devTok)
		req.Header.Set("User-Agent", "LarariumMobile/1.2")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if reg.lastVia != "device" || reg.lastAttr.DeviceID != "phone-alpha" ||
			reg.lastAttr.DisplayName != "Alpha Phone" || reg.lastAttr.UserAgent != "LarariumMobile/1.2" {
			t.Fatalf("got via=%q attr=%+v", reg.lastVia, reg.lastAttr)
		}
	})

	t.Run("UnboundOwnerTokenAndBodyUAIgnored", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000002/resolve",
			strings.NewReader(`{"verdict":"deny","user_agent":"SpoofedBody/9.9"}`))
		req.Header.Set("Authorization", "Bearer "+ownerTok)
		req.Header.Set("User-Agent", "TestBrowser/5.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if reg.lastVia != "web" || reg.lastAttr.DeviceID != "" ||
			reg.lastAttr.DisplayName != "" || reg.lastAttr.UserAgent != "TestBrowser/5.0" {
			t.Fatalf("got via=%q attr=%+v", reg.lastVia, reg.lastAttr)
		}
	})

	t.Run("ForgottenDeviceMidVerdictRefused", func(t *testing.T) {
		door.mu.Lock()
		door.errByID["phone-alpha"] = errors.New("device tombstoned")
		door.mu.Unlock()
		defer func() {
			door.mu.Lock()
			delete(door.errByID, "phone-alpha")
			door.mu.Unlock()
		}()

		before := reg.callCount()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000003/resolve",
			strings.NewReader(`{"verdict":"once"}`))
		req.Header.Set("Authorization", "Bearer "+devTok)
		req.Header.Set("User-Agent", "LarariumMobile/1.2")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// Amendment v16 (issue #344): a forgotten device has no verdict
		// authority — the liveness gate refuses before anything else runs.
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), "device_forgotten") {
			t.Fatalf("body = %q, want device_forgotten", body)
		}
		if reg.callCount() != before {
			t.Fatal("forgotten-device verdict must never reach the registry")
		}
	})

	t.Run("NilDevicesDoorFailsClosed", func(t *testing.T) {
		srv.SetDeviceDoor(nil)
		defer srv.SetDeviceDoor(door)

		before := reg.callCount()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000004/resolve",
			strings.NewReader(`{"verdict":"once"}`))
		req.Header.Set("Authorization", "Bearer "+devTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// Amendment v16 (issue #344): an unavailable devices door cannot
		// prove the bound device is still enrolled, so the verdict is
		// refused rather than degraded to unverifiable attribution.
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if reg.callCount() != before {
			t.Fatal("nil-door verdict must never reach the registry")
		}
	})

	t.Run("ErrBadRequestMapsTo400", func(t *testing.T) {
		reg.err = ErrBadRequest
		defer func() { reg.err = nil }()

		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000005/resolve",
			strings.NewReader(`{"verdict":"once"}`))
		req.Header.Set("Authorization", "Bearer "+ownerTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestWSCustosResolveAttributionAndLiveForget(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := &attributedTestRegistry{}
	door := &fakeAttrDeviceDoor{
		devices: map[string]DeviceCard{
			"phone-alpha": {DeviceID: "phone-alpha", DisplayName: "Alpha Phone"},
		},
		errByID: map[string]error{},
	}
	srv.SetCustosRegistry(reg)
	srv.SetDeviceDoor(door)

	devTok, err := tokStore.CreateDeviceToken("dev-alpha", "phone-alpha", "Alpha Phone", "android", "2.0")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWSWithUA(t, ts.URL, devTok, "LarariumWS/2.0")

	resp := client.callRPC(t, 1, "custos.cards.resolve", map[string]any{
		"id":         "a_0000000000000010",
		"verdict":    "once",
		"user_agent": "ClientParamSpoof/9.9",
	})
	if resp.Error != nil {
		t.Fatalf("resolve error: %+v", resp.Error)
	}
	if reg.lastVia != "device" || reg.lastAttr.DeviceID != "phone-alpha" ||
		reg.lastAttr.DisplayName != "Alpha Phone" || reg.lastAttr.UserAgent != "LarariumWS/2.0" {
		t.Fatalf("ws resolve got via=%q attr=%+v", reg.lastVia, reg.lastAttr)
	}

	// Amendment v16 (issue #344): once the device is forgotten, the stale
	// binding on the still-open socket carries no authority — the shared
	// core refuses the verdict with device_forgotten.
	door.mu.Lock()
	door.errByID["phone-alpha"] = errors.New("device tombstoned")
	door.mu.Unlock()

	resp2 := client.callRPC(t, 2, "custos.cards.resolve", map[string]any{
		"id":      "a_0000000000000011",
		"verdict": "deny",
	})
	if resp2.Error == nil || resp2.Error.Data["reason"] != "device_forgotten" {
		t.Fatalf("post-forget ws resolve = %+v, want error reason device_forgotten", resp2)
	}
	if reg.callCount() != 1 {
		t.Fatalf("registry calls = %d, want the post-forget verdict never to reach it", reg.callCount())
	}
}

func TestHubFallbackIgnoresAttribution(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := &attributedTestRegistry{err: ErrNoSuchCard}
	hub := NewApprovalHub(10 * time.Second)
	door := &fakeAttrDeviceDoor{
		devices: map[string]DeviceCard{
			"phone-alpha": {DeviceID: "phone-alpha", DisplayName: "Alpha Phone"},
		},
		errByID: map[string]error{},
	}
	srv.SetCustosRegistry(reg)
	srv.SetApprovalHub(hub)
	srv.SetDeviceDoor(door)

	var auditLines []string
	var auditMu sync.Mutex
	turnID, waitCh := hub.Register("s_MAIN", "web_fetch", "GET example.com", func(line string) {
		auditMu.Lock()
		auditLines = append(auditLines, line)
		auditMu.Unlock()
	})

	devTok, err := tokStore.CreateDeviceToken("dev-alpha", "phone-alpha", "Alpha Phone", "ios", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/"+turnID+"/resolve",
		strings.NewReader(`{"verdict":"once"}`))
	req.Header.Set("Authorization", "Bearer "+devTok)
	req.Header.Set("User-Agent", "LarariumMobile/1.2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hub fallback status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.State != "approved" {
		t.Fatalf("state = %q, want approved", out.State)
	}
	<-waitCh

	auditMu.Lock()
	defer auditMu.Unlock()
	for _, ln := range auditLines {
		if strings.Contains(ln, "phone-alpha") || strings.Contains(ln, "Alpha Phone") || strings.Contains(ln, "device_id") {
			t.Fatalf("hub fallback line must not carry device fields: %q", ln)
		}
	}

	// Amendment v16 (issue #344): the liveness gate precedes the hub
	// fallback — a forgotten device cannot settle a turn-approval card
	// either, and the card must stay pending for the fail-closed rules.
	turnID2, _ := hub.Register("s_MAIN", "web_fetch", "GET example.org", func(string) {})
	door.mu.Lock()
	door.errByID["phone-alpha"] = errors.New("device tombstoned")
	door.mu.Unlock()

	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/"+turnID2+"/resolve",
		strings.NewReader(`{"verdict":"once"}`))
	req2.Header.Set("Authorization", "Bearer "+devTok)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("forgotten-device hub resolve status = %d, want 403", resp2.StatusCode)
	}
	if _, pending := hub.SessionOf(turnID2); !pending {
		t.Fatal("forgotten device must not have settled the hub card")
	}
}

// TestDeviceForgetTerminatesBoundWSSession pins CUSTOS-SPEC amendment v16
// (issue #344): DELETE /v1/devices/<id> must terminate the live WS session
// bound to that device with close code 4003 (reason device_forgotten),
// while an unbound session on the same server keeps working.
func TestDeviceForgetTerminatesBoundWSSession(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := &attributedTestRegistry{}
	door := &fakeAttrDeviceDoor{
		devices: map[string]DeviceCard{
			"phone-a": {DeviceID: "phone-a", DisplayName: "A"},
		},
		errByID: map[string]error{},
	}
	srv.SetCustosRegistry(reg)
	srv.SetDeviceDoor(door)

	devTok, err := tokStore.CreateDeviceToken("dev-a", "phone-a", "A", "android", "2.0")
	if err != nil {
		t.Fatal(err)
	}
	adminTok, err := tokStore.Create("admin")
	if err != nil {
		t.Fatal(err)
	}

	bound := dialAuthenticatedWSWithUA(t, ts.URL, devTok, "LarariumWS/2.0")
	other := dialAuthenticatedWSWithUA(t, ts.URL, adminTok, "TestClient/1.0")

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/devices/phone-a", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE status = %d (%s), want 204", resp.StatusCode, body)
	}

	bound.expectCloseCode(t, WSCloseUnauthorized, WSReasonDeviceForgotten)

	// An unbound session survives the forget and still answers requests.
	r := other.callRPC(t, 5, "custos.cards.resolve", map[string]any{
		"id": "nope", "verdict": "once",
	})
	if r.Error == nil || r.Error.Data["reason"] != "not_found" {
		t.Fatalf("unbound session after forget = %+v, want not_found error", r)
	}

	// Once the dead socket's teardown defer has run, the registry must be
	// drained: a later forget of the same id finds nothing.
	deadline := time.Now().Add(2 * time.Second)
	for srv.closeDeviceConns("phone-a") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("device conn registry still holds entries after teardown")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
