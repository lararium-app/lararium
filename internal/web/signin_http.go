package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// SigninHTTPHandler serves guided sign-in viewer routes and done/cancel/status endpoints.
//
// Routing and Auth Architecture Decision:
// Routes for the guided sign-in window (/signin/v/<token> and /signin/api/<token>/*) are
// mounted outside /v1/ as auth-exempt from BearerAuth. This allows phone/tailnet browser
// viewers to navigate to the view URL directly from links without needing API tokens.
// Security is enforced via:
// 1. A cryptographically secure 256-bit one-time token per session with TTL.
// 2. Unknown or expired tokens return 404 with no info leak.
// 3. The surface server's HostGate DNS-rebinding defense and cleanPathGate still apply.
type SigninHTTPHandler struct {
	mgrProvider func() *SigninManager
	resolver    func(cardID string, allow bool) error
}

// NewSigninHTTPHandler creates a SigninHTTPHandler wrapping a static SigninManager.
func NewSigninHTTPHandler(mgr *SigninManager, resolver func(cardID string, allow bool) error) *SigninHTTPHandler {
	return NewSigninHTTPHandlerFunc(func() *SigninManager { return mgr }, resolver)
}

// NewSigninHTTPHandlerFunc creates a SigninHTTPHandler with a dynamic manager provider function.
func NewSigninHTTPHandlerFunc(mgrProvider func() *SigninManager, resolver func(cardID string, allow bool) error) *SigninHTTPHandler {
	return &SigninHTTPHandler{
		mgrProvider: mgrProvider,
		resolver:    resolver,
	}
}

func (h *SigninHTTPHandler) getManager() *SigninManager {
	if h.mgrProvider != nil {
		return h.mgrProvider()
	}
	return nil
}

func (h *SigninHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path

	switch {
	case strings.HasPrefix(p, "/signin/v/"):
		h.serveView(w, r, strings.TrimPrefix(p, "/signin/v/"))
	case strings.HasPrefix(p, "/signin/api/"):
		h.serveAPI(w, r, strings.TrimPrefix(p, "/signin/api/"))
	default:
		writeJSONNotFound(w)
	}
}

func (h *SigninHTTPHandler) serveView(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token = strings.TrimSpace(token)
	if token == "" || strings.Contains(token, "/") {
		writeJSONNotFound(w)
		return
	}

	mgr := h.getManager()
	if mgr == nil || !mgr.ValidateToken(token) {
		writeJSONNotFound(w)
		return
	}

	assetsDir := mgr.AssetsDir()
	if assetsDir == "" {
		writeJSONNotFound(w)
		return
	}

	indexPath := filepath.Join(assetsDir, "index.html")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		writeJSONNotFound(w)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *SigninHTTPHandler) serveAPI(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSONNotFound(w)
		return
	}

	token := parts[0]
	action := parts[1]

	mgr := h.getManager()
	if mgr == nil || !mgr.ValidateToken(token) {
		writeJSONNotFound(w)
		return
	}

	switch action {
	case "done":
		h.handleDone(w, r, mgr)
	case "cancel":
		h.handleCancel(w, r, mgr)
	case "status":
		h.handleStatus(w, r, mgr)
	default:
		writeJSONNotFound(w)
	}
}

func (h *SigninHTTPHandler) handleDone(w http.ResponseWriter, r *http.Request, mgr *SigninManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	st := mgr.State()
	if st != SigninLive && st != SigninSealing {
		writeJSONResponse(w, http.StatusConflict, map[string]string{
			"error": "manager not in live or sealing state",
			"state": string(st),
		})
		return
	}

	if err := mgr.Complete(); err != nil {
		writeJSONResponse(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}

	_ = mgr.ResolveCard(true)
	if h.resolver != nil && mgr.CardID() != "" {
		_ = h.resolver(mgr.CardID(), true)
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{
		"state": string(SigninDone),
	})
}

func (h *SigninHTTPHandler) handleCancel(w http.ResponseWriter, r *http.Request, mgr *SigninManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := mgr.Cancel(); err != nil {
		writeJSONResponse(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}

	_ = mgr.ResolveCard(false)
	if h.resolver != nil && mgr.CardID() != "" {
		_ = h.resolver(mgr.CardID(), false)
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{
		"state": string(SigninCanceled),
	})
}

func (h *SigninHTTPHandler) handleStatus(w http.ResponseWriter, r *http.Request, mgr *SigninManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{
		"state": string(mgr.State()),
	})
}

func writeJSONNotFound(w http.ResponseWriter) {
	writeJSONResponse(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func writeJSONResponse(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
