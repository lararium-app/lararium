package surface

import "net/http"

// ModelCapabilities describes the cached capability flags for a model entry
// returned by GET /v1/models (ASSISTANT-PHASE-SPEC §2.1).
type ModelCapabilities struct {
	Thinking      bool `json:"thinking"`
	Vision        bool `json:"vision"`
	Tools         bool `json:"tools"`
	Streaming     bool `json:"streaming"`
	ContextWindow int  `json:"context_window"`
}

// ModelEntry is one configured model row in GET /v1/models.
// Status is one of "online", "no_key", or "offline".
type ModelEntry struct {
	Ref          string            `json:"ref"`
	Provider     string            `json:"provider"`
	ModelID      string            `json:"model_id"`
	DisplayName  string            `json:"display_name"`
	Capabilities ModelCapabilities `json:"capabilities"`
	Status       string            `json:"status"`
	IsDefault    bool              `json:"is_default"`
	IsCompact    bool              `json:"is_compact"`
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
