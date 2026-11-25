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

type fakeAttrDeviceDoor struct {
	mu      sync.Mutex
	devices map[string]DeviceCard
	errByID map[string]error
}

func (d *fakeAttrDeviceDoor) ValidateID(string) error                       { return nil }
func (d *fakeAttrDeviceDoor) ValidateRails([]DeclaredRail) error            { return nil }
func (d *fakeAttrDeviceDoor) Register(string, string, []DeclaredRail) error { return nil }
func (d *fakeAttrDeviceDoor) List() []DeviceCard                            { return nil }
func (d *fakeAttrDeviceDoor) Forget(string) error                           { return nil }
func (d *fakeAttrDeviceDoor) SetRailEnabled(string, string, bool) error     { return nil }
func (d *fakeAttrDeviceDoor) SetOnline(string, bool)                        {}
func (d *fakeAttrDeviceDoor) IsOnline(string) bool                          { return false }
func (d *fakeAttrDeviceDoor) LastSeen(string) time.Time                     { return time.Time{} }
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

	t.Run("ForgottenDeviceMidVerdict", func(t *testing.T) {
		door.mu.Lock()
		door.errByID["phone-alpha"] = errors.New("device tombstoned")
		door.mu.Unlock()
		defer func() {
			door.mu.Lock()
			delete(door.errByID, "phone-alpha")
			door.mu.Unlock()
		}()

		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000003/resolve",
			strings.NewReader(`{"verdict":"once"}`))
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
		if reg.lastAttr.DeviceID != "phone-alpha" || reg.lastAttr.DisplayName != "" {
			t.Fatalf("forgotten device attr = %+v, want DeviceID kept and DisplayName empty", reg.lastAttr)
		}
	})

	t.Run("NilDevicesDoorNoPanic", func(t *testing.T) {
		srv.SetDeviceDoor(nil)
		defer srv.SetDeviceDoor(door)

		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/custos/cards/a_0000000000000004/resolve",
			strings.NewReader(`{"verdict":"once"}`))
		req.Header.Set("Authorization", "Bearer "+devTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if reg.lastAttr.DeviceID != "phone-alpha" || reg.lastAttr.DisplayName != "" {
			t.Fatalf("nil door attr = %+v, want DeviceID kept and DisplayName empty", reg.lastAttr)
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

	// Declared known limit (§6): device forgotten while WS session is live still
	// approves until reconnect, stamping DeviceID with an empty DisplayName.
	door.mu.Lock()
	door.errByID["phone-alpha"] = errors.New("device tombstoned")
	door.mu.Unlock()

	resp2 := client.callRPC(t, 2, "custos.cards.resolve", map[string]any{
		"id":      "a_0000000000000011",
		"verdict": "deny",
	})
	if resp2.Error != nil {
		t.Fatalf("post-forget ws resolve error: %+v", resp2.Error)
	}
	if reg.lastAttr.DeviceID != "phone-alpha" || reg.lastAttr.DisplayName != "" || reg.lastAttr.UserAgent != "LarariumWS/2.0" {
		t.Fatalf("post-forget ws attr = %+v", reg.lastAttr)
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
}
