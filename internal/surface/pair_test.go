package surface

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
)

type testPairStoreAdapter struct {
	store *nuntius.PairStore
}

func (a *testPairStoreAdapter) Redeem(userID, code string, now time.Time) (RedeemResult, error) {
	res, err := a.store.Redeem(userID, code, now)
	switch res {
	case nuntius.RedeemPaired:
		return RedeemPaired, err
	case nuntius.RedeemInvalid:
		return RedeemInvalid, err
	case nuntius.RedeemAlready:
		return RedeemAlready, err
	default:
		return RedeemInvalid, err
	}
}

type fakePairStore struct {
	redeemFunc func(userID, code string, now time.Time) (RedeemResult, error)
}

func (f *fakePairStore) Redeem(userID, code string, now time.Time) (RedeemResult, error) {
	if f.redeemFunc != nil {
		return f.redeemFunc(userID, code, now)
	}
	return RedeemInvalid, nil
}

func newPairRequest(method, body string) *http.Request {
	req := httptest.NewRequest(method, "/v1/pair", strings.NewReader(body))
	req.Host = "localhost:7717"
	req = req.WithContext(WithLocalAddr(req.Context(), &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7717}))
	return req
}

func TestPairHappyPath(t *testing.T) {
	dir := t.TempDir()
	tokenStore, err := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	nStore, err := nuntius.NewPairStore(filepath.Join(dir, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}

	srv := &Server{
		Store:     tokenStore,
		PairStore: &testPairStoreAdapter{store: nStore},
	}
	handler := NewServer(srv)

	code, err := nStore.Mint(time.Now(), 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	reqBody := fmt.Sprintf(`{"code":%q,"device_name":"Pixel 8"}`, code)
	req := newPairRequest(http.MethodPost, reqBody)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp struct {
		Token  string `json:"token"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.UserID != "app:pixel-8" {
		t.Errorf("user_id = %q, want app:pixel-8", resp.UserID)
	}
	if !strings.HasPrefix(resp.Token, "lar1_") {
		t.Errorf("token = %q, want lar1_ prefix", resp.Token)
	}

	if !srv.Store.Verify(resp.Token) {
		t.Errorf("Store.Verify(%q) returned false, want true", resp.Token)
	}

	list, err := srv.Store.List()
	if err != nil {
		t.Fatalf("Store.List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].Label != "paired:app:pixel-8" {
		t.Errorf("list[0].Label = %q, want paired:app:pixel-8", list[0].Label)
	}
}

func TestPairCodesAndState(t *testing.T) {
	dir := t.TempDir()
	tokenStore, err := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	nStore, err := nuntius.NewPairStore(filepath.Join(dir, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	srv := &Server{
		Store:     tokenStore,
		PairStore: &testPairStoreAdapter{store: nStore},
	}
	handler := NewServer(srv)

	t.Run("wrong code returns 401", func(t *testing.T) {
		req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000","device_name":"Test"}`)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
		if !strings.Contains(w.Body.String(), `"invalid or expired pairing code"`) {
			t.Errorf("body = %s, want invalid or expired pairing code", w.Body.String())
		}
	})

	t.Run("expired code returns 401", func(t *testing.T) {
		expCode, err := nStore.Mint(time.Now().Add(-20*time.Minute), -5*time.Minute)
		if err != nil {
			t.Fatalf("Mint expired: %v", err)
		}

		req := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q}`, expCode))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
		if !strings.Contains(w.Body.String(), `"invalid or expired pairing code"`) {
			t.Errorf("body = %s, want invalid or expired pairing code", w.Body.String())
		}
	})

	t.Run("same code twice returns second 401 on unpair or 409 after pairing", func(t *testing.T) {
		code, err := nStore.Mint(time.Now(), 15*time.Minute)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}

		// First pair succeeds
		req1 := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Device1"}`, code))
		w1 := httptest.NewRecorder()
		handler.ServeHTTP(w1, req1)
		if w1.Code != http.StatusOK {
			t.Fatalf("first pair status = %d, want 200", w1.Code)
		}

		// Second pair with same code returns 409 (already paired)
		req2 := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Device1"}`, code))
		w2 := httptest.NewRecorder()
		handler.ServeHTTP(w2, req2)
		if w2.Code != http.StatusConflict {
			t.Errorf("second pair status = %d, want 409", w2.Code)
		}
		if !strings.Contains(w2.Body.String(), `"this hearth is already paired"`) {
			t.Errorf("body = %s, want this hearth is already paired", w2.Body.String())
		}
	})

	t.Run("after a successful pair, another pair attempt returns 409", func(t *testing.T) {
		req := newPairRequest(http.MethodPost, `{"code":"pair1_11111111111111111111","device_name":"Device2"}`)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"this hearth is already paired"`) {
			t.Errorf("body = %s, want this hearth is already paired", w.Body.String())
		}
	})

	t.Run("same wrong code twice returns second 401", func(t *testing.T) {
		dir2 := t.TempDir()
		ts2, _ := OpenTokenStore(filepath.Join(dir2, "tokens.json"))
		ps2, _ := nuntius.NewPairStore(filepath.Join(dir2, "nuntius"))
		srv2 := &Server{Store: ts2, PairStore: &testPairStoreAdapter{store: ps2}}
		h2 := NewServer(srv2)

		wrongCode := "pair1_99999999999999999999"
		for i := 1; i <= 2; i++ {
			req := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q}`, wrongCode))
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("attempt %d status = %d, want 401", i, w.Code)
			}
		}
	})
}

func TestPairMalformed(t *testing.T) {
	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	srv := &Server{
		Store:     tokenStore,
		PairStore: &fakePairStore{},
	}
	handler := NewServer(srv)

	tests := []struct {
		name       string
		method     string
		body       string
		wantStatus int
		wantSub    string
	}{
		{"missing code", http.MethodPost, `{"device_name":"Pixel"}`, http.StatusBadRequest, "invalid pairing code"},
		{"empty code", http.MethodPost, `{"code":"","device_name":"Pixel"}`, http.StatusBadRequest, "invalid pairing code"},
		{"bad prefix", http.MethodPost, `{"code":"pair0_12345678901234567890"}`, http.StatusBadRequest, "invalid pairing code"},
		{"19 chars", http.MethodPost, `{"code":"pair1_1234567890123456789"}`, http.StatusBadRequest, "invalid pairing code"},
		{"21 chars", http.MethodPost, `{"code":"pair1_123456789012345678901"}`, http.StatusBadRequest, "invalid pairing code"},
		{"non-base62 exclamation", http.MethodPost, `{"code":"pair1_1234567890123456789!"}`, http.StatusBadRequest, "invalid pairing code"},
		{"non-base62 dash", http.MethodPost, `{"code":"pair1_1234567890123456789-"}`, http.StatusBadRequest, "invalid pairing code"},
		{"GET method not allowed", http.MethodGet, "", http.StatusMethodNotAllowed, "method not allowed"},
		{"PUT method not allowed", http.MethodPut, `{"code":"pair1_12345678901234567890"}`, http.StatusMethodNotAllowed, "method not allowed"},
		{"bad json", http.MethodPost, `{not json`, http.StatusBadRequest, "invalid request body"},
		{"oversized body via explicit cap", http.MethodPost, `{"code":"pair1_12345678901234567890","device_name":"` + strings.Repeat("x", 5000) + `"}`, http.StatusRequestEntityTooLarge, "body too large"},
		{"oversized body via 1MiB limit", http.MethodPost, strings.Repeat("x", (1<<20)+10), http.StatusRequestEntityTooLarge, "body too large"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := newPairRequest(tc.method, tc.body)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantSub != "" && !strings.Contains(w.Body.String(), tc.wantSub) {
				t.Errorf("body = %s, want substring %q", w.Body.String(), tc.wantSub)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestPairLimiter(t *testing.T) {
	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))

	srv := &Server{
		Store: tokenStore,
		PairStore: &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				return RedeemInvalid, nil
			},
		},
	}
	handler := NewServer(srv)

	ipA := "192.168.1.50:54321"
	ipB := "192.168.1.60:54321"

	// 10 invalid attempts from IP A -> each returns 401
	for i := 1; i <= 10; i++ {
		req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
		req.RemoteAddr = ipA
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i, w.Code)
		}
	}

	// 11th attempt from IP A -> returns 429
	{
		req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
		req.RemoteAddr = ipA
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("11th attempt status = %d, want 429", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"too many pairing attempts"`) {
			t.Errorf("body = %s, want too many pairing attempts", w.Body.String())
		}
	}

	// Different IP B still gets 401
	{
		req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
		req.RemoteAddr = ipB
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("different IP status = %d, want 401", w.Code)
		}
	}

	// Success responses and 409 responses do not increment limiter
	{
		ipC := "192.168.1.70:54321"
		srv.PairStore = &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				return RedeemAlready, nil
			},
		}
		for i := 1; i <= 15; i++ {
			req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
			req.RemoteAddr = ipC
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusConflict {
				t.Fatalf("409 attempt %d status = %d, want 409", i, w.Code)
			}
		}

		ipD := "192.168.1.80:54321"
		srv.PairStore = &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				return RedeemPaired, nil
			},
		}
		for i := 1; i <= 15; i++ {
			req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
			req.RemoteAddr = ipD
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("success attempt %d status = %d, want 200", i, w.Code)
			}
		}
	}
}

func TestPairNilDependency(t *testing.T) {
	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	srv := &Server{
		Store:     tokenStore,
		PairStore: nil,
	}
	handler := NewServer(srv)

	req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"not found"`) {
		t.Errorf("body = %s, want not found", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestPairUserIDShape(t *testing.T) {
	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))

	var capturedUserID string
	srv := &Server{
		Store: tokenStore,
		PairStore: &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				capturedUserID = userID
				return RedeemPaired, nil
			},
		},
	}
	handler := NewServer(srv)

	tests := []struct {
		name       string
		deviceName string
		wantUserID string
	}{
		{"Pixel 8", "Pixel 8", "app:pixel-8"},
		{"Pixel with junk chars", "Pixel !@# 8$$$", "app:pixel-8"},
		{"empty device_name", "", "app:device"},
		{"whitespace device_name", "   ", "app:device"},
		{"all junk chars fallback to device", "!@#$%^", "app:device"},
		{"spaces and underscores to single dashes", "My_Android   Phone__v2", "app:my-android-phone-v2"},
		{"leading and trailing dashes trimmed", "_-Phone-_", "app:phone"},
		{"cap at 64 runes", strings.Repeat("A", 100), "app:" + strings.Repeat("a", 64)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capturedUserID = ""
			reqBody := fmt.Sprintf(`{"code":"pair1_00000000000000000000","device_name":%q}`, tc.deviceName)
			req := newPairRequest(http.MethodPost, reqBody)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if capturedUserID != tc.wantUserID {
				t.Errorf("capturedUserID = %q, want %q", capturedUserID, tc.wantUserID)
			}
		})
	}

	t.Run("omitted device_name defaults to device", func(t *testing.T) {
		capturedUserID = ""
		req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000"}`)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if capturedUserID != "app:device" {
			t.Errorf("capturedUserID = %q, want app:device", capturedUserID)
		}
	})
}

func TestPairRedaction(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOutput)

	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	secretCode := "pair1_SECRETCODE1234567890"

	srv := &Server{
		Store: tokenStore,
		PairStore: &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				return RedeemPaired, nil
			},
		},
	}
	handler := NewServer(srv)

	req := newPairRequest(http.MethodPost, fmt.Sprintf(`{"code":%q,"device_name":"Pixel"}`, secretCode))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	logOutput := buf.String()
	if strings.Contains(logOutput, secretCode) {
		t.Errorf("secret pairing code found in log output:\n%s", logOutput)
	}
	if resp.Token != "" && strings.Contains(logOutput, resp.Token) {
		t.Errorf("secret token found in log output:\n%s", logOutput)
	}
}

func TestPairTokenMintFailure(t *testing.T) {
	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	_ = os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)

	srv := &Server{
		Store: tokenStore,
		PairStore: &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				return RedeemPaired, nil
			},
		},
	}
	handler := NewServer(srv)

	req := newPairRequest(http.MethodPost, `{"code":"pair1_00000000000000000000","device_name":"Pixel"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	wantRemedy := `{"error":"pairing succeeded but token mint failed; run: hearthd unpair app:pixel and create a new code"}`
	if w.Body.String() != wantRemedy {
		t.Errorf("body = %s, want %s", w.Body.String(), wantRemedy)
	}
}

func TestPairMiddlewareChain(t *testing.T) {
	dir := t.TempDir()
	tokenStore, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	srv := &Server{
		Cfg: ServeConfig{
			AllowedHosts: []string{"hearth.example.com"},
		},
		Store: tokenStore,
		PairStore: &fakePairStore{
			redeemFunc: func(userID, code string, now time.Time) (RedeemResult, error) {
				return RedeemPaired, nil
			},
		},
	}
	handler := NewServer(srv)

	t.Run("host gate rejects evil host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/pair", strings.NewReader(`{"code":"pair1_00000000000000000000"}`))
		req.Host = "evil.example.com"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if w.Body.String() != `{"error":"forbidden"}` {
			t.Errorf("body = %s, want forbidden", w.Body.String())
		}
	})

	t.Run("clean path gate rejects double slash", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "//v1/pair", strings.NewReader(`{"code":"pair1_00000000000000000000"}`))
		req.Host = "hearth.example.com"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
		if w.Body.String() != `{"error":"not found"}` {
			t.Errorf("body = %s, want not found", w.Body.String())
		}
	})
}
