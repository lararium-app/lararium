package surface

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestL15_InternalErrorsGeneric verifies that 500-class errors return generic category
// strings and do not echo internal error strings to clients (L-15, SURFACE-SPEC §4).
func TestL15_InternalErrorsGeneric(t *testing.T) {
	// 1. Custos card resolve failure
	reg := NewFakeCustosRegistry()
	cardID := "a_internalerr0123"
	reg.PublishCard(CustosCard{ID: cardID})
	reg.SetResolveErr(cardID, errors.New("flock failed: /var/lib/custos.lock: permission denied"))

	tok, handler := setupV16Server(t, reg)

	req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(`{"verdict":"once"}`))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("resolve error status = %d, want 500", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "flock") || strings.Contains(body, "permission denied") {
		t.Errorf("500 response echoed internal error detail: %q (L-15)", body)
	}
	if !strings.Contains(body, `"error":"internal error"`) {
		t.Errorf("500 response missing generic category reason: %q (L-15)", body)
	}

	// 2. Policy ListRules unexpected error
	fakePolicy := &fakePolicyErrorSource{err: errors.New("yaml: unmarshal error at line 42: invalid syntax")}
	pTok, polHandler := setupPolicyTestServer(t, fakePolicy)

	pReq := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
	pReq.Host = "127.0.0.1:7717"
	pReq.Header.Set("Authorization", "Bearer "+pTok)
	pW := httptest.NewRecorder()
	polHandler.ServeHTTP(pW, pReq)

	if pW.Code != http.StatusInternalServerError {
		t.Fatalf("policy error status = %d, want 500", pW.Code)
	}
	pBody := pW.Body.String()
	if strings.Contains(pBody, "yaml") || strings.Contains(pBody, "syntax") {
		t.Errorf("policy 500 response echoed internal details: %q (L-15)", pBody)
	}
	if !strings.Contains(pBody, `"error":"internal error"`) {
		t.Errorf("policy 500 response missing generic category reason: %q (L-15)", pBody)
	}
}

type fakePolicyErrorSource struct {
	err error
}

func (f *fakePolicyErrorSource) ListRules() ([]PolicyRule, error) {
	return nil, f.err
}

func (f *fakePolicyErrorSource) AddRule(lane, pattern, verdict string) ([]string, error) {
	return nil, f.err
}

func (f *fakePolicyErrorSource) RemoveRule(lane, pattern string) (bool, error) {
	return false, f.err
}

func (f *fakePolicyErrorSource) EgressStrict() bool {
	return false
}
