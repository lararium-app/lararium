package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/lararium-app/lararium/internal/keystore"
)

// rpcKeysList implements keys.list (port of dispatchKeys GET + keyRows, keys.go).
func (s *Server) rpcKeysList(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.keys == nil {
		_ = conn.SendError(*req.ID, "method not found", "method_not_found", nil)
		return
	}
	_ = conn.SendResult(*req.ID, s.keyRows())
}

// rpcKeysSet implements keys.set (port of putKeyHandler, keys.go).
// Preserves the v1 gate order: name, then unknown provider (unless force),
// then key size (4 KiB), then strict param shape (DisallowUnknownFields),
// then Store.Set, then Reload. Key values never appear in logs or errors.
func (s *Server) rpcKeysSet(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.keys == nil {
		_ = conn.SendError(*req.ID, "method not found", "method_not_found", nil)
		return
	}

	var raw map[string]json.RawMessage
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &raw) != nil || raw == nil {
		_ = conn.SendError(*req.ID, "invalid request", "invalid_request", nil)
		return
	}

	// Gate 1: provider name.
	var name string
	if rawName, ok := raw["name"]; ok && string(rawName) != "null" {
		if err := json.Unmarshal(rawName, &name); err != nil {
			_ = conn.SendError(*req.ID, "invalid request", "invalid_request", nil)
			return
		}
	}
	if !keystore.ValidName(name) {
		_ = conn.SendError(*req.ID, "invalid provider name", "invalid_provider_name", nil)
		return
	}

	// Gate 2: unknown provider (unless force is true).
	var force bool
	if rawForce, ok := raw["force"]; ok && string(rawForce) != "null" {
		if err := json.Unmarshal(rawForce, &force); err != nil {
			_ = conn.SendError(*req.ID, "invalid request", "invalid_request", nil)
			return
		}
	}
	if !s.knownProvider(name) && !force {
		_ = conn.SendError(*req.ID, "unknown provider", "unknown_provider", nil)
		return
	}

	// Gate 3: key value length cap (4 KiB).
	if rawKey, ok := raw["key"]; ok && string(rawKey) != "null" {
		var keyVal string
		if err := json.Unmarshal(rawKey, &keyVal); err == nil && len(keyVal) > maxKeyBody {
			_ = conn.SendError(*req.ID, "key too large", "key_too_large", nil)
			return
		}
	}

	// Gate 4: strict param shape (reject unknown fields and mistyped values).
	// Never include decode error text so key material can never leak.
	var params struct {
		Name  string `json:"name"`
		Key   string `json:"key"`
		Force bool   `json:"force"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&params); err != nil {
		_ = conn.SendError(*req.ID, "invalid request", "invalid_request", nil)
		return
	}
	if len(params.Key) > maxKeyBody {
		_ = conn.SendError(*req.ID, "key too large", "key_too_large", nil)
		return
	}

	// Gate 5: store write.
	if err := s.keys.Store.Set(params.Name, params.Key); err != nil {
		switch {
		case errors.Is(err, keystore.ErrEmpty):
			_ = conn.SendError(*req.ID, "empty key", "empty_key", nil)
		case errors.Is(err, keystore.ErrFull):
			_ = conn.SendError(*req.ID, "key store full", "key_store_full", nil)
		default:
			// Store errors are ours; they never carry key material.
			log.Printf("surface: key store write failed: %v", err)
			_ = conn.SendError(*req.ID, "key store write failed", "key_store_write_failed", nil)
		}
		return
	}

	// Gate 6: in-process registry reload.
	if err := s.keys.Reload(); err != nil {
		log.Printf("surface: key reload failed: %v", err)
		_ = conn.SendError(*req.ID, "key saved but reload failed", "key_reload_failed", nil)
		return
	}

	_ = conn.SendResult(*req.ID, map[string]any{"ok": true})
}

// rpcKeysRemove implements keys.remove (port of deleteKeyHandler, keys.go).
func (s *Server) rpcKeysRemove(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.keys == nil {
		_ = conn.SendError(*req.ID, "method not found", "method_not_found", nil)
		return
	}

	var params struct {
		Name string `json:"name"`
	}
	trimmed := bytes.TrimSpace(req.Params)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || json.Unmarshal(trimmed, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request", "invalid_request", nil)
		return
	}

	if !keystore.ValidName(params.Name) {
		_ = conn.SendError(*req.ID, "invalid provider name", "invalid_provider_name", nil)
		return
	}

	// Idempotent: removing an absent name is a success (spec K4).
	if _, err := s.keys.Store.Remove(params.Name); err != nil {
		log.Printf("surface: key store remove failed: %v", err)
		_ = conn.SendError(*req.ID, "key store write failed", "key_store_write_failed", nil)
		return
	}
	if err := s.keys.Reload(); err != nil {
		log.Printf("surface: key reload failed: %v", err)
		_ = conn.SendError(*req.ID, "key removed but reload failed", "key_reload_failed", nil)
		return
	}

	_ = conn.SendResult(*req.ID, map[string]any{"ok": true})
}
