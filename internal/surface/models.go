package surface

import (
	"context"
	"net/http"
)

// ModelCapabilities describes the cached capability flags for a model entry
// returned by GET /v1/models (ASSISTANT-PHASE-SPEC §2.1).
type ModelCapabilities struct {
	Thinking      bool `json:"thinking"`
	Vision        bool `json:"vision"`
	Tools         bool `json:"tools"`
	Streaming     bool `json:"streaming"`
	ContextWindow int  `json:"context_window"`
}

// ModelEntry is one configured model row in GET /v1/models and models.list.
// Status is one of "online", "no_key", or "offline".
// ProbedAt is the RFC3339 timestamp of the last probe attempt, or nil if never probed.
type ModelEntry struct {
	Ref          string            `json:"ref"`
	Provider     string            `json:"provider"`
	ModelID      string            `json:"model_id"`
	DisplayName  string            `json:"display_name"`
	Capabilities ModelCapabilities `json:"capabilities"`
	Status       string            `json:"status"`
	IsDefault    bool              `json:"is_default"`
	IsCompact    bool              `json:"is_compact"`
	ProbedAt     *string           `json:"probed_at"`
}

// ModelDefaults reports the active default fallback chains.
type ModelDefaults struct {
	ChatChain    []string `json:"chat_chain"`
	CompactChain []string `json:"compact_chain"`
}

// ModelsResponse is the wire payload served by GET /v1/models.
type ModelsResponse struct {
	Models   []ModelEntry  `json:"models"`
	Defaults ModelDefaults `json:"defaults"`
}

// ModelCatalog is the cached model view served by GET /v1/models.
// Implementations MUST NOT probe providers on the request path; they
// serve the last cached probe record only.
type ModelCatalog interface {
	Catalog() ModelsResponse
}

func (s *Server) modelsHandler(w http.ResponseWriter) {
	if s.Models == nil {
		s.error(w, "model_catalog_unavailable", http.StatusServiceUnavailable)
		return
	}
	resp := s.Models.Catalog()
	if resp.Models == nil {
		resp.Models = []ModelEntry{}
	}
	writeJSONAny(w, http.StatusOK, resp)
}

// rpcModelsList implements models.list (RPC-SPEC §4a). It serves only the
// cached catalog view and performs no network I/O on the request path.
func (s *Server) rpcModelsList(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.Models == nil {
		_ = conn.SendError(*req.ID, "model catalog unavailable", "model_catalog_unavailable", nil)
		return
	}
	resp := s.Models.Catalog()
	if resp.Models == nil {
		resp.Models = []ModelEntry{}
	}
	if resp.Defaults.ChatChain == nil {
		resp.Defaults.ChatChain = []string{}
	}
	if resp.Defaults.CompactChain == nil {
		resp.Defaults.CompactChain = []string{}
	}
	_ = conn.SendResult(*req.ID, resp)
}

// validateModelPin resolves ref against the cached catalog. It performs no
// network I/O. ok is false when the catalog has no such ref or the ref has
// no usable key.
func (s *Server) validateModelPin(ref string) (ok bool, reason string) {
	if s.Models == nil {
		return false, "model_catalog_unavailable"
	}
	for _, m := range s.Models.Catalog().Models {
		if m.Ref == ref {
			if m.Status == "no_key" {
				return false, "invalid_model_pin"
			}
			return true, ""
		}
	}
	return false, "invalid_model_pin"
}
