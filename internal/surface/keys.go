package surface

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/lararium-app/lararium/internal/keystore"
)

// Provider key routes (KEYS-SPEC K4): GET/PUT/DELETE /v1/keys[/{name}]
// behind the existing bearer + Host gates. Registered only when the
// daemon attaches KeysDeps — servers without them answer 404 like any
// unknown path, which keeps every pre-keys test untouched.

// maxKeyBody caps the PUT body read (spec K4: 4 KiB cap; over => 413).
const maxKeyBody = 4096

// KeysDeps wires the key routes to the daemon's live store, registry,
// and config. All funcs must be safe for concurrent use.
type KeysDeps struct {
	Store     *keystore.Store
	Registry  *keystore.Registry
	Providers func() []keystore.ProviderInfo // config providers, request-time
	Reload    func() error                   // re-read store -> Registry.Swap + warnings
}

// AttachKeys enables the /v1/keys routes. Call from cmd/hearthd only;
// NewServer's signature is unchanged so existing call sites compile.
func (s *Server) AttachKeys(deps *KeysDeps) { s.keys = deps }

// keyRow is one GET row (spec K4): name + K2 status + display-only
// sha256 prefix. Never a value.
type keyRow struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	SHA256 string `json:"sha256_8"`
}

// statusOf renders the K2 status string for a resolution source.
func statusOf(src keystore.Source) string {
	switch src {
	case keystore.SourceEnv:
		return "set (env)"
	case keystore.SourceKeys:
		return "set (keys.json)"
	case keystore.SourceConfig:
		return "set (config)"
	case keystore.SourceMissing:
		return "missing"
	}
	return "missing"
}

// dispatchKeys routes /v1/keys and /v1/keys/{name}; returns false when
// the keys door is not attached (caller falls through to 404).
func (s *Server) dispatchKeys(w http.ResponseWriter, r *http.Request) bool {
	if s.keys == nil {
		return false
	}
	if r.URL.Path == "/v1/keys" {
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		writeJSONAny(w, http.StatusOK, s.keyRows())
		return true
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/keys/")
	if name == r.URL.Path || name == "" {
		return false // not a keys path at all
	}
	switch r.Method {
	case http.MethodPut:
		s.putKeyHandler(w, r, name)
	case http.MethodDelete:
		s.deleteKeyHandler(w, name)
	default:
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	return true
}

// keyNames returns the request-time union config ∪ keys.json (spec K4:
// no ghosts, no tombstones) and the config-side provider infos.
func (s *Server) keyNames() ([]string, []keystore.ProviderInfo) {
	deps := s.keys
	provs := deps.Providers()

	seen := map[string]bool{}
	names := make([]string, 0, len(provs))
	for _, p := range provs {
		if !seen[p.Name] {
			seen[p.Name] = true
			names = append(names, p.Name)
		}
	}
	if m, err := deps.Store.Read(); err == nil {
		for n := range m {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names, provs
}

// keyRows resolves the union through ResolveAll (values never leave).
func (s *Server) keyRows() []keyRow {
	names, provs := s.keyNames()
	byName := map[string]keystore.ProviderInfo{}
	for _, p := range provs {
		byName[p.Name] = p
	}
	infos := make([]keystore.ProviderInfo, 0, len(names))
	for _, n := range names {
		info := byName[n] // zero value for keys.json-only: no env, no literal
		info.Name = n
		infos = append(infos, info)
	}
	res, _ := keystore.ResolveAll(s.keys.Store, infos, nil)

	rows := make([]keyRow, 0, len(names))
	for _, n := range names {
		row := keyRow{Name: n, Status: statusOf(res[n].Source)}
		if res[n].Value != "" {
			sum := sha256.Sum256([]byte(res[n].Value))
			row.SHA256 = hex8(sum[:])
		}
		rows = append(rows, row)
	}
	return rows
}

// hex8 renders the first 4 bytes as 8 lowercase hex chars.
func hex8(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for i := range 4 {
		out[2*i] = digits[b[i]>>4]
		out[2*i+1] = digits[b[i]&0x0f]
	}
	return string(out)
}

func (s *Server) putKeyHandler(w http.ResponseWriter, r *http.Request, name string) {
	// Gate order (spec K4): name first — a bad name is 404 regardless
	// of body, matching the S4 doctrine for path params.
	if !keystore.ValidName(name) {
		writeJSONBody(w, http.StatusNotFound, `{"error":"invalid provider name"}`)
		return
	}

	// "Unknown provider" = in NEITHER config nor keys.json (spec K4).
	// A CLI-added key lives in keys.json, so the drawer re-saves it
	// without force; force is only for names the store has never seen.
	if !s.knownProvider(name) && r.URL.Query().Get("force") != "true" {
		writeJSONBody(w, http.StatusNotFound, `{"error":"unknown provider"}`)
		return
	}

	// Body: {"key": "..."} capped at 4 KiB read; over => 413. The body
	// must parse as JSON with exactly a string key field (K4).
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxKeyBody+1))
	if err != nil {
		s.error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(raw) > maxKeyBody {
		writeJSONBody(w, http.StatusRequestEntityTooLarge, `{"error":"key too large"}`)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONBody(w, http.StatusBadRequest, `{"error":"body must be {\"key\": \"...\"}"}`)
		return
	}

	if err := s.keys.Store.Set(name, body.Key); err != nil {
		switch {
		case errors.Is(err, keystore.ErrEmpty):
			writeJSONBody(w, http.StatusBadRequest, `{"error":"empty key"}`)
		case errors.Is(err, keystore.ErrFull):
			writeJSONBody(w, http.StatusConflict, `{"error":"key store full"}`)
		default:
			// Store errors are ours; they never carry key material.
			s.error(w, "key store write failed", http.StatusInternalServerError)
		}
		return
	}

	// In-process reload (spec K4/K6): the daemon is running by
	// definition, so the registry swap completes before the response —
	// no socket hop, and the next request already sees the new key.
	if err := s.keys.Reload(); err != nil {
		s.error(w, "key saved but reload failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// knownProvider reports whether name exists in config or keys.json.
func (s *Server) knownProvider(name string) bool {
	for _, p := range s.keys.Providers() {
		if p.Name == name {
			return true
		}
	}
	if m, err := s.keys.Store.Read(); err == nil {
		_, ok := m[name]
		return ok
	}
	return false
}

func (s *Server) deleteKeyHandler(w http.ResponseWriter, name string) {
	if !keystore.ValidName(name) {
		writeJSONBody(w, http.StatusNotFound, `{"error":"invalid provider name"}`)
		return
	}
	// Idempotent: removing an absent name is a success (spec K4).
	if _, err := s.keys.Store.Remove(name); err != nil {
		s.error(w, "key store write failed", http.StatusInternalServerError)
		return
	}
	if err := s.keys.Reload(); err != nil {
		s.error(w, "key removed but reload failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
