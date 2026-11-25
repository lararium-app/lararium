package surface

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
)

type fakeEnrollStore struct {
	mu    sync.Mutex
	codes map[string]time.Time
}

func newFakeEnrollStore() *fakeEnrollStore {
	return &fakeEnrollStore{codes: make(map[string]time.Time)}
}

func (f *fakeEnrollStore) add(code string, expires time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = expires
}

func (f *fakeEnrollStore) Redeem(code string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, ok := f.codes[code]
	if !ok || !now.Before(exp) {
		return false, nil
	}
	delete(f.codes, code)
	return true, nil
}

func TestTokenStoreBindingAndBackCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	st, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	// Legacy entry without device_id / device_name / platform / app_version.
	opTok, err := st.Create("admin-web")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pairedTok, err := st.CreateKind("paired:app:old", TokenKindDevice)
	if err != nil {
		t.Fatalf("CreateKind: %v", err)
	}

	// Strip any new keys to prove older tokens.json files load cleanly.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var rawEntries []map[string]any
	if err := json.Unmarshal(raw, &rawEntries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, e := range rawEntries {
		delete(e, "device_id")
		delete(e, "device_name")
		delete(e, "platform")
		delete(e, "app_version")
	}
	clean, err := json.Marshal(rawEntries)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, clean, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	callerOp, kindOp, devOp, okOp := st.VerifyBinding(opTok)
	if !okOp || callerOp == "" || kindOp != "" || devOp != "" {
		t.Fatalf("VerifyBinding(op) = (%q,%q,%q,%v), want non-empty caller, empty kind/dev, ok=true", callerOp, kindOp, devOp, okOp)
	}
	cKindOp, kOp, okKindOp := st.VerifyKind(opTok)
	if !okKindOp || cKindOp != callerOp || kOp != "" {
		t.Fatalf("VerifyKind(op) = (%q,%q,%v), want (%q,\"\",true)", cKindOp, kOp, okKindOp, callerOp)
	}

	callerP, kindP, devP, okP := st.VerifyBinding(pairedTok)
	if !okP || callerP == "" || kindP != TokenKindDevice || devP != "" {
		t.Fatalf("VerifyBinding(paired) = (%q,%q,%q,%v), want device kind, empty dev, ok=true", callerP, kindP, devP, okP)
	}
	cKindP, kP, okKindP := st.VerifyKind(pairedTok)
	if !okKindP || cKindP != callerP || kP != TokenKindDevice {
		t.Fatalf("VerifyKind(paired) = (%q,%q,%v), want (%q,%q,true)", cKindP, kP, okKindP, callerP, TokenKindDevice)
	}

	// Mint device-bound token and verify fields, List, and RevokeDevice.
	devTok, err := st.CreateDeviceToken("device:pixel-9", "pixel-9", "My Pixel 9", "android", "1.2.3")
	if err != nil {
		t.Fatalf("CreateDeviceToken: %v", err)
	}
	callerD, kindD, devD, okD := st.VerifyBinding(devTok)
	if !okD || callerD == "" || kindD != TokenKindDevice || devD != "pixel-9" {
		t.Fatalf("VerifyBinding(dev) = (%q,%q,%q,%v), want device/pixel-9/true", callerD, kindD, devD, okD)
	}

	infos, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var foundBound bool
	for _, info := range infos {
		if info.Label == "device:pixel-9" && info.DeviceID == "pixel-9" {
			foundBound = true
		}
	}
	if !foundBound {
		t.Fatalf("TokenInfo list missing DeviceID binding: %+v", infos)
	}

	// RevokeDevice drops only the bound device token.
	n, err := st.RevokeDevice("pixel-9")
	if err != nil || n != 1 {
		t.Fatalf("RevokeDevice(pixel-9) = (%d, %v), want (1, nil)", n, err)
	}
	if st.Verify(devTok) {
		t.Fatal("revoked device token still verifies")
	}
	if !st.Verify(opTok) || !st.Verify(pairedTok) {
		t.Fatal("unrelated tokens were revoked by RevokeDevice")
	}
}

type bindingVerifierDouble struct {
	kind     string
	deviceID string
}

func (b bindingVerifierDouble) Verify(token string) bool { return token == "good" }
func (b bindingVerifierDouble) VerifyIdentity(token string) (string, bool) {
	return "t_bind", token == "good"
}

func (b bindingVerifierDouble) VerifyKind(token string) (string, string, bool) {
	return "t_bind", b.kind, token == "good"
}

func (b bindingVerifierDouble) VerifyBinding(token string) (string, string, string, bool) {
	return "t_bind", b.kind, b.deviceID, token == "good"
}

func TestBearerAuthBindingVerifier(t *testing.T) {
	t.Run("BindingVerifier binds caller, kind, and deviceID", func(t *testing.T) {
		var gotCaller, gotKind, gotDev string
		h := BearerAuth(bindingVerifierDouble{kind: TokenKindDevice, deviceID: "pixel-9"})(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCaller = callerFrom(r.Context())
				gotKind = callerKindFrom(r.Context())
				gotDev = callerDeviceIDFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}),
		)
		req := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
		req.Header.Set("Authorization", "Bearer good")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if gotCaller != "t_bind" || gotKind != TokenKindDevice || gotDev != "pixel-9" {
			t.Fatalf("got (%q,%q,%q), want (t_bind,device,pixel-9)", gotCaller, gotKind, gotDev)
		}
	})

	t.Run("only KindVerifier leaves deviceID empty", func(t *testing.T) {
		var gotKind, gotDev string
		h := BearerAuth(kindVerifierDouble{kind: TokenKindDevice})(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotKind = callerKindFrom(r.Context())
				gotDev = callerDeviceIDFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}),
		)
		req := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
		req.Header.Set("Authorization", "Bearer good")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if gotKind != TokenKindDevice || gotDev != "" {
			t.Fatalf("got (kind=%q,dev=%q), want (device,\"\")", gotKind, gotDev)
		}
	})

	t.Run("invalid token with BindingVerifier returns 401", func(t *testing.T) {
		called := false
		h := BearerAuth(bindingVerifierDouble{kind: TokenKindDevice, deviceID: "pixel-9"})(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}),
		)
		req := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
		req.Header.Set("Authorization", "Bearer bad")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if called {
			t.Fatal("next handler ran for invalid token")
		}
	})
}

func TestPairDeviceEnrollmentFlow(t *testing.T) {
	dir := t.TempDir()
	ts, err := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	nStore, err := nuntius.NewPairStore(filepath.Join(dir, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	eStore := newFakeEnrollStore()
	door := newFakeDeviceDoor()

	srv := &Server{
		Store:             ts,
		PairStore:         &testPairStoreAdapter{store: nStore},
		Enroll:            eStore,
		DeviceAutoApprove: true,
	}
	srv.SetDeviceDoor(door)
	handler := NewServer(srv)

	// 1. Happy path: mint dev1_ code, redeem -> 200 {token, device_id}, token bound to device_id.
	code1 := "dev1_ABCDEF1234567890abcd"
	eStore.add(code1, time.Now().Add(24*time.Hour))
	req1 := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Pixel 9 Pro","platform":"android","app_version":"2.1.0"}`, code1))
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("enroll 1 status = %d, want 200; body = %s", w1.Code, w1.Body.String())
	}

	var respMap map[string]any
	if err := json.Unmarshal(w1.Body.Bytes(), &respMap); err != nil {
		t.Fatalf("unmarshal resp 1: %v", err)
	}
	if len(respMap) != 2 {
		t.Fatalf("response keys = %v, want exactly token and device_id", respMap)
	}
	if _, hasUser := respMap["user_id"]; hasUser {
		t.Fatalf("device enroll response must not include user_id: %v", respMap)
	}
	tok1, _ := respMap["token"].(string)
	devID1, _ := respMap["device_id"].(string)
	if !strings.HasPrefix(tok1, "lar1_") {
		t.Fatalf("token = %q, want lar1_ prefix", tok1)
	}
	if devID1 != "pixel-9-pro" {
		t.Fatalf("device_id = %q, want pixel-9-pro", devID1)
	}
	caller1, kind1, bound1, ok1 := ts.VerifyBinding(tok1)
	if !ok1 || caller1 == "" || kind1 != TokenKindDevice || bound1 != devID1 {
		t.Fatalf("VerifyBinding(tok1) = (%q,%q,%q,%v), want device/%s/true", caller1, kind1, bound1, ok1, devID1)
	}

	// Verify audit event recorded.
	foundAudit := false
	for i, k := range door.auditKinds {
		if k == AuditKindDeviceEnrolled && door.audited[i].DeviceID == devID1 && door.audited[i].Platform == "android" && door.audited[i].AppVersion == "2.1.0" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("missing device_enrolled audit record: kinds=%v payloads=%+v", door.auditKinds, door.audited)
	}

	// 2. Second enroll with same device_name gets distinct collision-suffixed device_id, unknown platform -> "other".
	code2 := "dev1_ZZZZZZ1234567890abcd"
	eStore.add(code2, time.Now().Add(24*time.Hour))
	req2 := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Pixel 9 Pro","platform":"symbian","app_version":"0.1"}`, code2))
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("enroll 2 status = %d, want 200; body = %s", w2.Code, w2.Body.String())
	}
	var resp2 struct {
		Token    string `json:"token"`
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal resp 2: %v", err)
	}
	if resp2.DeviceID == devID1 || !strings.HasPrefix(resp2.DeviceID, "pixel-9-pro-") {
		t.Fatalf("second device_id = %q, want collision-suffixed distinct from %q", resp2.DeviceID, devID1)
	}
	if err := door.ValidateID(resp2.DeviceID); err != nil {
		t.Fatalf("suffixed device_id %q failed ValidateID: %v", resp2.DeviceID, err)
	}
	entries, err := ts.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}
	var foundOther bool
	for _, e := range entries {
		if e.DeviceID == resp2.DeviceID && e.Platform == "other" {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("unknown platform was not normalized to 'other': %+v", entries)
	}

	// 3. Regression: existing pair1_ owner pairing still works on the same server.
	ownerCode, err := nStore.Mint(time.Now(), 15*time.Minute)
	if err != nil {
		t.Fatalf("nStore.Mint: %v", err)
	}
	reqOwner := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Owner Phone"}`, ownerCode))
	wOwner := httptest.NewRecorder()
	handler.ServeHTTP(wOwner, reqOwner)
	if wOwner.Code != http.StatusOK {
		t.Fatalf("owner pair regression status = %d, want 200; body = %s", wOwner.Code, wOwner.Body.String())
	}
	if !strings.Contains(wOwner.Body.String(), `"user_id":"app:owner-phone"`) {
		t.Fatalf("owner pair body = %s, want app:owner-phone", wOwner.Body.String())
	}
}

func TestPairDeviceEnrollmentErrorsAndLimiter(t *testing.T) {
	dir := t.TempDir()
	ts, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	eStore := newFakeEnrollStore()
	door := newFakeDeviceDoor()

	srv := &Server{
		Store:  ts,
		Enroll: eStore,
	}
	srv.SetDeviceDoor(door)
	handler := NewServer(srv)

	// Bad shape -> 401 generic (identical wording as owner lane), increments limiter.
	ipA := "198.51.100.10:12345"
	for i := 1; i <= 10; i++ {
		req := newPairRequest(http.MethodPost, `{"code":"dev1_short"}`)
		req.RemoteAddr = ipA
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad shape attempt %d status = %d, want 401; body=%s", i, w.Code, w.Body.String())
		}
		if w.Body.String() != `{"error":"invalid or expired pairing code"}` {
			t.Fatalf("bad shape body = %s, want generic invalid or expired pairing code", w.Body.String())
		}
	}

	// 11th attempt from same IP -> 429 rate limit.
	req429 := newPairRequest(http.MethodPost, `{"code":"dev1_00000000000000000000"}`)
	req429.RemoteAddr = ipA
	w429 := httptest.NewRecorder()
	handler.ServeHTTP(w429, req429)
	if w429.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt status = %d, want 429; body=%s", w429.Code, w429.Body.String())
	}

	// Oversized body -> 413 Request Entity Too Large (pinned by pair.go maxPairBody).
	reqBig := newPairRequest(http.MethodPost, `{"code":"dev1_00000000000000000000","device_name":"`+strings.Repeat("a", 5000)+`"}`)
	reqBig.RemoteAddr = "198.51.100.20:12345"
	wBig := httptest.NewRecorder()
	handler.ServeHTTP(wBig, reqBig)
	if wBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413; body=%s", wBig.Code, wBig.Body.String())
	}

	// Nil EnrollStore + dev1_ -> 503.
	srvNilEnroll := &Server{Store: ts, Enroll: nil}
	srvNilEnroll.SetDeviceDoor(door)
	hNilEnroll := NewServer(srvNilEnroll)
	reqNilE := newPairRequest(http.MethodPost, `{"code":"dev1_00000000000000000000"}`)
	wNilE := httptest.NewRecorder()
	hNilEnroll.ServeHTTP(wNilE, reqNilE)
	if wNilE.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil EnrollStore status = %d, want 503; body=%s", wNilE.Code, wNilE.Body.String())
	}

	// Nil DeviceDoor + valid dev1_ -> 503 AND code is NOT consumed!
	srvNilDoor := &Server{Store: ts, Enroll: eStore}
	hNilDoor := NewServer(srvNilDoor)
	validCode := "dev1_11111111112222222222"
	eStore.add(validCode, time.Now().Add(24*time.Hour))
	reqNilD := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q}`, validCode))
	wNilD := httptest.NewRecorder()
	hNilDoor.ServeHTTP(wNilD, reqNilD)
	if wNilD.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil DeviceDoor status = %d, want 503; body=%s", wNilD.Code, wNilD.Body.String())
	}
	stillValid, err := eStore.Redeem(validCode, time.Now())
	if err != nil || !stillValid {
		t.Fatalf("code was consumed despite unavailable DeviceDoor: stillValid=%v err=%v", stillValid, err)
	}
}

func TestDeviceRegisterMergeAndDeleteSweep(t *testing.T) {
	dir := t.TempDir()
	ts, err := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	eStore := newFakeEnrollStore()
	door := newFakeDeviceDoor()
	srv := &Server{
		Cfg:               ServeConfig{AllowedHosts: []string{"localhost"}},
		Store:             ts,
		Enroll:            eStore,
		DeviceAutoApprove: true,
	}
	srv.SetDeviceDoor(door)
	handler := NewServer(srv)

	// Mint an unrelated unbound admin token.
	adminTok, err := ts.Create("admin-cli")
	if err != nil {
		t.Fatalf("Create admin: %v", err)
	}

	// Enroll a device via POST /v1/pair.
	code := "dev1_33333333334444444444"
	eStore.add(code, time.Now().Add(24*time.Hour))
	wEnroll := httptest.NewRecorder()
	handler.ServeHTTP(wEnroll, newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Phone A","platform":"android"}`, code)))
	if wEnroll.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200; body=%s", wEnroll.Code, wEnroll.Body.String())
	}
	var enr struct {
		Token    string `json:"token"`
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(wEnroll.Body.Bytes(), &enr); err != nil {
		t.Fatalf("unmarshal enroll: %v", err)
	}

	doAuthed := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "localhost:7717"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// 1. Bound token re-registering its own id -> 200 (no 409).
	regOwn := fmt.Sprintf(`{"device_id":%q,"display_name":"Phone A","rails_declared":[{"name":"calendar","read_only":true}]}`, enr.DeviceID)
	wOwn := doAuthed(http.MethodPost, "/v1/devices/register", enr.Token, regOwn)
	if wOwn.Code != http.StatusOK {
		t.Fatalf("bound self-register status = %d, want 200; body=%s", wOwn.Code, wOwn.Body.String())
	}

	// 2. Bound token trying to register a foreign device_id -> 403 forbidden.
	regForeign := `{"device_id":"someone-else","display_name":"Evil","rails_declared":[{"name":"calendar","read_only":true}]}`
	wForeign := doAuthed(http.MethodPost, "/v1/devices/register", enr.Token, regForeign)
	if wForeign.Code != http.StatusForbidden {
		t.Fatalf("bound foreign-register status = %d, want 403; body=%s", wForeign.Code, wForeign.Body.String())
	}
	if !strings.Contains(wForeign.Body.String(), `"reason":"forbidden"`) {
		t.Fatalf("foreign-register body = %s, want reason forbidden", wForeign.Body.String())
	}

	// 3. Unbound token registering an existing device_id -> 409 duplicate_name (regression).
	wUnbound := doAuthed(http.MethodPost, "/v1/devices/register", adminTok, regOwn)
	if wUnbound.Code != http.StatusConflict {
		t.Fatalf("unbound duplicate register status = %d, want 409; body=%s", wUnbound.Code, wUnbound.Body.String())
	}
	if !strings.Contains(wUnbound.Body.String(), `"duplicate_name"`) {
		t.Fatalf("unbound duplicate body = %s, want duplicate_name", wUnbound.Body.String())
	}

	// 4. DELETE /v1/devices/<id> revokes the device token while unrelated token survives.
	wDel := doAuthed(http.MethodDelete, "/v1/devices/"+enr.DeviceID, adminTok, "")
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body=%s", wDel.Code, wDel.Body.String())
	}
	if ts.Verify(enr.Token) {
		t.Fatal("device token still verifies after DELETE /v1/devices/<id>")
	}
	if !ts.Verify(adminTok) {
		t.Fatal("unrelated admin token stopped verifying after device delete")
	}
}
