package surface

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// ModelPinSetter is implemented by session sources that can update a
// session's model pin. Sources that do not implement it answer 501.
type ModelPinSetter interface {
	SetModelPin(id string, pin *string) error
}

func (s *Server) patchSessionHandler(w http.ResponseWriter, r *http.Request, id string) {
	if !ValidSessionID(id) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	var req struct {
		ModelPin *string `json:"model_pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if s.Hub != nil && s.Hub.InFlight(id) {
		s.error(w, "turn_in_flight", http.StatusConflict)
		return
	}

	pin := req.ModelPin
	if pin != nil && *pin != "" {
		if ok, reason := s.validateModelPin(*pin); !ok {
			status := http.StatusBadRequest
			if reason == "model_catalog_unavailable" {
				status = http.StatusServiceUnavailable
			}
			s.error(w, reason, status)
			return
		}
	} else {
		pin = nil
	}

	setter, ok := s.Sessions.(ModelPinSetter)
	if !ok {
		s.error(w, "not implemented", http.StatusNotImplemented)
		return
	}

	if err := setter.SetModelPin(id, pin); err != nil {
		if errors.Is(err, ErrNoSession) {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("surface: patch session model pin failed: %v", err)
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSONAny(w, http.StatusOK, map[string]any{
		"id":        id,
		"model_pin": pin,
	})
}
