package surface

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/web"
)

// SessionInfo is the wire shape of one session in GET /v1/sessions
// (spec §5: id, created, kind, optional title/parent/model_pin).
type SessionInfo struct {
	ID       string  `json:"id"`
	Created  string  `json:"created"`
	Kind     string  `json:"kind"`
	Title    *string `json:"title,omitempty"`
	Parent   *string `json:"parent,omitempty"`
	ModelPin *string `json:"model_pin,omitempty"`
}

// SessionSource is the storage view the handlers need: list, create,
// and replay session logs. PenatusSource implements it over the frozen
// Penatus file layer.
type SessionSource interface {
	List() ([]SessionInfo, error)
	Create(title string, modelPin string) (SessionInfo, error)
	Events(id string, afterSeq int64, limit int) (events []json.RawMessage, inFlight bool, err error)
	Delete(id string) error
}

// TurnHub is the live-turn view the handlers need: the in_flight flag
// for catch-up (spec §5) and POST /cancel.
type TurnHub interface {
	InFlight(sessionID string) bool
	Cancel(sessionID string) bool
	// Shutdown resolves every pending approval denied:shutdown and
	// cancels every running turn (V12).
	Shutdown()
}

// Server wires the HTTP surface: config, token store, session storage,
// and the turn hub. NewServer builds the handler chain.
type Server struct {
	Cfg      ServeConfig
	Store    *TokenStore
	Sessions SessionSource
	Hub      TurnHub
	Fasti    *fasti.Engine
	// OnShutdown is called on shutdown signals before Hub.Shutdown()
	// (NUNTIUS-SPEC §2: bridge stops before hub drops pending approvals).
	OnShutdown func()
	// keys is nil until AttachKeys; routes answer 404 without it.
	keys *KeysDeps

	custosMu  sync.RWMutex
	custosReg CustosRegistry

	// approvals bridges turn-approval cards into the custody registry
	// surface (issue #128). nil-safe: routes behave registry-only.
	approvals *ApprovalHub

	policyMu  sync.RWMutex
	policySrc PolicySource

	// connLife is the connector lifecycle state (CONNECTOR-LIFE-SPEC D3-D6):
	// door, committed-shape record, pending add/edit gates, rail pokes.
	connLife connectorLife

	// credSrc backs the /v1/credentials admin routes (CREDHANDLE-SPEC D5);
	// nil answers locked.
	credMu  sync.RWMutex
	credSrc CredentialSource

	// alwaysHook mints the card-minted always-rule when a turn-approval
	// card settles with "always" (issue #132; SURFACE-SPEC §4: always-
	// rules are card-minted). Set by wiring; nil-safe: the verdict then
	// degrades to once, loudly logged.
	alwaysMu           sync.RWMutex
	alwaysHook         func(toolName string) error
	connectorTrustHook func(toolName string) error

	Backup      BackupFunc
	BackupCheck func() error

	// PairStore is the optional pairing storage dependency (issue #184).
	PairStore   PairStore
	pairMu      sync.Mutex
	pairLimiter *pairLimiter

	// DeviceAutoApprove bypasses custody cards on device registration (DEVICE-RAILS-SPEC Rev 3).
	DeviceAutoApprove bool

	devicesMu       sync.RWMutex
	devicesDoor     DeviceDoor
	devicesBroker   *deviceBroker
	devicePendingMu sync.Mutex
	devicePending   map[string]*pendingDevice
	devicePokeMu    sync.RWMutex
	devicePokes     map[int64]func()
	devicePokeSeq   int64

	webMu       sync.RWMutex
	webWallet   *web.Wallet
	webIndex    *web.WalletIndex
	webActivity *web.ActivityLogger
	webHome     string
}

// SetWebWallet attaches the session wallet, index, and activity logger to the Server.
func (s *Server) SetWebWallet(w *web.Wallet, idx *web.WalletIndex, act *web.ActivityLogger) {
	s.webMu.Lock()
	defer s.webMu.Unlock()
	s.webWallet = w
	s.webIndex = idx
	s.webActivity = act
}

// SetWebHome sets the base directory for lazily creating web wallet dependencies.
func (s *Server) SetWebHome(home string) {
	s.webMu.Lock()
	defer s.webMu.Unlock()
	s.webHome = home
}

func (s *Server) getWebDeps() (*web.Wallet, *web.WalletIndex, *web.ActivityLogger) {
	s.webMu.Lock()
	defer s.webMu.Unlock()
	if s.webIndex == nil && s.webHome != "" {
		s.webWallet = web.NewWallet(s.webHome, nil)
		s.webIndex = web.NewWalletIndex(s.webHome)
		s.webActivity = web.NewActivityLogger(s.webHome)
	}
	return s.webWallet, s.webIndex, s.webActivity
}

// SetDeviceDoor attaches the device control plane.
func (s *Server) SetDeviceDoor(door DeviceDoor) {
	s.devicesMu.Lock()
	defer s.devicesMu.Unlock()
	s.devicesDoor = door
	broker := newDeviceBroker(s)
	s.devicesBroker = broker
	if door != nil {
		door.SetActionBroker(broker)
	}
}

func (s *Server) getDeviceDoor() DeviceDoor {
	s.devicesMu.RLock()
	defer s.devicesMu.RUnlock()
	return s.devicesDoor
}

// DeviceBroker returns the currently attached ActionBroker, if any.
func (s *Server) DeviceBroker() ActionBroker {
	s.devicesMu.RLock()
	defer s.devicesMu.RUnlock()
	return s.devicesBroker
}

// SetDeviceAutoApprove configures whether device registration is auto-approved without consent cards.
func (s *Server) SetDeviceAutoApprove(auto bool) {
	s.DeviceAutoApprove = auto
}

func (s *Server) deviceAutoApprove() bool {
	return s.DeviceAutoApprove || s.Cfg.DeviceAutoApprove
}

// BackupFunc handles control-socket backup verbs (PENATUS §4.5.3.1).
type BackupFunc func(outAbs string, noConfig bool, progress func(string)) error

func (s *Server) healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONBody(w, http.StatusOK, `{"ok":true,"version":"1.0.0-beta8"}`)
}

func (s *Server) listSessionsHandler(w http.ResponseWriter, _ *http.Request) {
	sessions, err := s.Sessions.List()
	if err != nil {
		log.Printf("surface: list sessions failed: %v", err)
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSONAny(w, http.StatusOK, sessions)
}

func (s *Server) createSessionHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title    string `json:"title,omitempty"`
		ModelPin string `json:"model_pin,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	session, err := s.Sessions.Create(req.Title, req.ModelPin)
	if err != nil {
		log.Printf("surface: create session failed: %v", err)
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSONAny(w, http.StatusCreated, session)
}

// deleteSessionHandler removes a side chat (DELETE /v1/sessions/{id}).
// A turn in flight answers 409 busy; the main session is protected;
// unknown ids are a plain 404.
func (s *Server) deleteSessionHandler(w http.ResponseWriter, id string) {
	if !ValidSessionID(id) {
		s.error(w, "no such session", http.StatusNotFound)
		return
	}
	if s.Hub != nil && s.Hub.InFlight(id) {
		s.error(w, "session busy", http.StatusConflict)
		return
	}
	err := s.Sessions.Delete(id)
	switch {
	case errors.Is(err, ErrNoSession):
		s.error(w, "no such session", http.StatusNotFound)
	case errors.Is(err, ErrProtectedSession):
		s.error(w, "session is protected", http.StatusForbidden)
	case err != nil:
		log.Printf("surface: delete session failed: %v", err)
		s.error(w, "delete failed", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) eventsHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	id = strings.TrimSuffix(id, "/events")

	if !ValidSessionID(id) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	afterSeq := int64(0)
	if v := r.URL.Query().Get("after_seq"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			s.error(w, "invalid after_seq", http.StatusBadRequest)
			return
		}
		afterSeq = parsed
	}

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			s.error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = parsed
	}

	if limit > 1000 {
		limit = 1000
	}

	events, inFlight, err := s.Sessions.Events(id, afterSeq, limit)
	if err != nil {
		if errors.Is(err, ErrNoSession) {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("surface: get session events failed: %v", err)
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSONAny(w, http.StatusOK, map[string]any{
		"events":    events,
		"in_flight": inFlight,
	})
}

func (s *Server) cancelHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	id = strings.TrimSuffix(id, "/cancel")

	if !ValidSessionID(id) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	if s.Hub.Cancel(id) {
		writeJSONBody(w, http.StatusOK, `{"cancelled":true}`)
		return
	}

	s.error(w, "no turn in flight", http.StatusNotFound)
}

func (s *Server) error(w http.ResponseWriter, msg string, code int) {
	writeJSONAny(w, code, map[string]string{"error": msg})
}

// writeJSONAny marshals v first (a marshal failure is a server bug, not
// a client error) and answers with the bytes; a write failure means the
// client is gone and there is nobody left to tell.
func writeJSONAny(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("surface: json marshal failed: %v", err)
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"internal error"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func bodyLimitMiddleware(next http.Handler) http.Handler {
	const maxBody = 1 << 20 // 1 MiB
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBody {
			writeJSONBody(w, http.StatusRequestEntityTooLarge, `{"error":"body too large"}`)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}
