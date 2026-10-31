package surface

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Phone admin endpoints for credential handles (CREDHANDLE-SPEC R4 / D5).
//
// AUTH: every route here is served behind the same BearerAuth gate as the
// other mutating routes (gates.go; missing/invalid token → 401
// {"error":"unauthorized"} before dispatch, so no vault access happens).
// The codebase has no token-scope concept (TokenStore.Verify is a bool), so
// "admin" is today exactly "holds a valid bearer token". When a scope model
// lands (SURFACE-SPEC work), the hook point is dispatchCredentials: check
// the scope there, before any CredentialSource call.

// Credential-source sentinels. The adapter in cmd/hearthd maps custos errors
// onto these so this package never imports internal/custos.
var (
	// ErrCredentialNotFound is returned when a handle does not exist.
	ErrCredentialNotFound = errors.New("credential handle not found")
	// ErrCredentialExists is returned when the credential name is taken.
	ErrCredentialExists = errors.New("credential already exists")
	// ErrCredentialFull is returned when the credential name cap is reached.
	ErrCredentialFull = errors.New("credential store full")
)

// CredentialFieldError names a request field that failed validation (field
// name only — never the value).
type CredentialFieldError struct{ Field string }

func (e *CredentialFieldError) Error() string { return "invalid field: " + e.Field }

// CredentialRate is the wire shape of a per-handle rate limit.
type CredentialRate struct {
	N      int `json:"n"`
	Window int `json:"window_seconds"`
}

// CredentialInfo is the D5 wire shape. It deliberately has no secret field.
type CredentialInfo struct {
	Handle  string          `json:"handle"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Last4   string          `json:"last4"`
	Created string          `json:"created"`
	Expires *string         `json:"expires"`
	Rate    *CredentialRate `json:"rate"`
}

// CredentialCreate is the write-only create request (D5).
type CredentialCreate struct {
	Name       string
	Secret     string
	Scope      string
	TTLSeconds int64
	Rate       *CredentialRate
}

// CredentialSource is the storage view the credential handlers need,
// implemented over the custos vault in cmd/hearthd. List and Revoke return
// ErrPolicyLocked/ErrLocked-style "locked" errors while the vault is locked.
type CredentialSource interface {
	List() ([]CredentialInfo, error)
	Create(req CredentialCreate) (CredentialInfo, error)
	Revoke(handle string) error
}

// SetCredentialSource attaches a CredentialSource; nil-safe, concurrent-safe.
func (s *Server) SetCredentialSource(src CredentialSource) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	s.credSrc = src
}

func (s *Server) getCredentialSource() CredentialSource {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.credSrc
}

var credHandleRe = regexp.MustCompile(`^hdl_[0-9A-Za-z]{22}$`)

func isLockedErr(err error) bool {
	return errors.Is(err, ErrPolicyLocked) || errors.Is(err, ErrLocked) || err.Error() == "locked"
}

// dispatchCredentials routes /v1/credentials and /v1/credentials/{handle}.
func (s *Server) dispatchCredentials(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/credentials" {
		switch r.Method {
		case http.MethodGet:
			s.credentialsListHandler(w)
		case http.MethodPost:
			s.credentialsCreateHandler(w, r)
		default:
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	handle := strings.TrimPrefix(r.URL.Path, "/v1/credentials/")
	if r.Method != http.MethodDelete {
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !credHandleRe.MatchString(handle) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}
	s.credentialsRevokeHandler(w, handle)
}

// credentialsListHandler serves GET /v1/credentials (D5). Secret material is
// never part of CredentialInfo.
func (s *Server) credentialsListHandler(w http.ResponseWriter) {
	src := s.getCredentialSource()
	if src == nil {
		s.error(w, "locked", http.StatusLocked)
		return
	}
	infos, err := src.List()
	if err != nil {
		s.credentialsError(w, "list", err)
		return
	}
	if infos == nil {
		infos = []CredentialInfo{}
	}
	writeJSONAny(w, http.StatusOK, infos)
}

// credentialsCreateHandler serves POST /v1/credentials: WRITE-ONLY create
// (D5, V-T1b). The secret is consumed once; the 201 response, logs, audit and
// error text carry handle + last4 and field names only, never the value.
func (s *Server) credentialsCreateHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       *string         `json:"name"`
		Kind       *string         `json:"kind"`
		Secret     *string         `json:"secret"`
		Scope      *string         `json:"scope"`
		TTLSeconds *int64          `json:"ttl_seconds"`
		Rate       *CredentialRate `json:"rate"`
	}
	// Decode errors are reported with a fixed string: encoding/json errors
	// can quote input fragments.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Name == nil || *body.Name == "" {
		s.error(w, "invalid field: name", http.StatusBadRequest)
		return
	}
	if body.Secret == nil || *body.Secret == "" {
		s.error(w, "invalid field: secret", http.StatusBadRequest)
		return
	}
	if body.Kind != nil && *body.Kind != "" && *body.Kind != "api_key" {
		s.error(w, "invalid field: kind", http.StatusBadRequest)
		return
	}
	req := CredentialCreate{Name: *body.Name, Secret: *body.Secret, Rate: body.Rate}
	if body.Scope != nil {
		req.Scope = *body.Scope
	}
	if body.TTLSeconds != nil {
		if *body.TTLSeconds < 0 || *body.TTLSeconds > int64(100*365*24*time.Hour/time.Second) {
			s.error(w, "invalid field: ttl_seconds", http.StatusBadRequest)
			return
		}
		req.TTLSeconds = *body.TTLSeconds
	}
	if body.Rate != nil && (body.Rate.N <= 0 || body.Rate.Window <= 0) {
		s.error(w, "invalid field: rate", http.StatusBadRequest)
		return
	}
	src := s.getCredentialSource()
	if src == nil {
		s.error(w, "locked", http.StatusLocked)
		return
	}
	info, err := src.Create(req)
	if err != nil {
		s.credentialsError(w, "create", err)
		return
	}
	writeJSONAny(w, http.StatusCreated, info)
}

// credentialsRevokeHandler serves DELETE /v1/credentials/{handle}: it revokes
// the handle only. The underlying named credential stays in the vault, the
// same semantics as `custos handle revoke`.
func (s *Server) credentialsRevokeHandler(w http.ResponseWriter, handle string) {
	src := s.getCredentialSource()
	if src == nil {
		s.error(w, "locked", http.StatusLocked)
		return
	}
	if err := src.Revoke(handle); err != nil {
		s.credentialsError(w, "revoke", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// credentialsError maps source errors onto the frozen error shape. Only
// fixed strings and field names are written; raw err text is never echoed
// and is not logged for create (its path holds the secret).
func (s *Server) credentialsError(w http.ResponseWriter, op string, err error) {
	var fe *CredentialFieldError
	switch {
	case isLockedErr(err):
		s.error(w, "locked", http.StatusLocked)
	case errors.As(err, &fe):
		s.error(w, "invalid field: "+fe.Field, http.StatusBadRequest)
	case errors.Is(err, ErrCredentialNotFound):
		s.error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrCredentialExists):
		s.error(w, "credential already exists", http.StatusConflict)
	case errors.Is(err, ErrCredentialFull):
		s.error(w, "credential store full", http.StatusConflict)
	default:
		log.Printf("surface: credentials %s failed", op)
		s.error(w, "internal error", http.StatusInternalServerError)
	}
}
