package surface

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// dispatchDevices routes all /v1/devices endpoints.
func (s *Server) dispatchDevices(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch p {
	case "/v1/devices":
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.devicesListHandler(w, r)
	case "/v1/devices/register":
		if r.Method != http.MethodPost {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.deviceRegisterHandler(w, r)
	case "/v1/devices/events":
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.devicesEventsHandler(w, r, "")
	default:
		rest := strings.TrimPrefix(p, "/v1/devices/")
		if rest == "" {
			s.error(w, "not found", http.StatusNotFound)
			return
		}

		if !strings.Contains(rest, "/") {
			switch r.Method {
			case http.MethodDelete:
				s.deviceDeleteHandler(w, r, rest)
			case http.MethodGet:
				s.deviceGetHandler(w, r, rest)
			default:
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		id, action, ok := strings.Cut(rest, "/")
		if !ok {
			s.error(w, "not found", http.StatusNotFound)
			return
		}

		door := s.getDeviceDoor()
		if door != nil {
			if err := door.ValidateID(id); err != nil {
				s.error(w, "not found", http.StatusNotFound)
				return
			}
		}

		switch {
		case action == "stream":
			if r.Method != http.MethodGet {
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.deviceStreamHandler(w, r, id)
		case action == "sync":
			if r.Method != http.MethodPost {
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.deviceSyncHandler(w, r, id)
		case action == "events":
			if r.Method != http.MethodGet {
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.devicesEventsHandler(w, r, id)
		case action == "rails":
			if r.Method != http.MethodPatch {
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.deviceRailsPatchHandler(w, r, id)
		case strings.HasPrefix(action, "actions/") && strings.HasSuffix(action, "/receipt"):
			sub := strings.TrimPrefix(action, "actions/")
			actionID := strings.TrimSuffix(sub, "/receipt")
			if actionID == "" || strings.Contains(actionID, "/") {
				s.error(w, "not found", http.StatusNotFound)
				return
			}
			if r.Method != http.MethodPost {
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.deviceActionReceiptHandler(w, r, id, actionID)
		default:
			s.error(w, "not found", http.StatusNotFound)
		}
	}
}

// devicesListHandler serves GET /v1/devices per DEVICE-RAILS-SPEC line 132.
func (s *Server) devicesListHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONAny(w, http.StatusOK, map[string]any{"devices": s.deviceRows()})
}

// deviceGetHandler serves GET /v1/devices/<id>.
func (s *Server) deviceGetHandler(w http.ResponseWriter, _ *http.Request, id string) {
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := door.ValidateID(id); err != nil {
		s.error(w, "not found", http.StatusNotFound)
		return
	}
	rows := s.deviceRows()
	for _, row := range rows {
		if row.DeviceID == id {
			writeJSONAny(w, http.StatusOK, row)
			return
		}
	}
	s.error(w, "not found", http.StatusNotFound)
}

// deviceRegisterHandler serves POST /v1/devices/register.
func (s *Server) deviceRegisterHandler(w http.ResponseWriter, r *http.Request) {
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxConnectorBody+1))
	if err != nil || len(body) > maxConnectorBody {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": "bad_request"})
		return
	}

	var req struct {
		DeviceID      string         `json:"device_id"`
		DisplayName   string         `json:"display_name"`
		RailsDeclared []DeclaredRail `json:"rails_declared"`
		Fingerprint   string         `json:"fingerprint,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": "bad_request"})
		return
	}

	// 1. Validate device_id slug
	if err := door.ValidateID(req.DeviceID); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid device_id", "reason": "bad_device_id"})
		return
	}

	// 2. Validate declared rails per DEVICE-RAILS-SPEC D4 / spec R4
	if err := door.ValidateRails(req.RailsDeclared); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "reason": "rail_mismatch"})
		return
	}

	if strings.TrimSpace(req.DisplayName) == "" {
		req.DisplayName = req.DeviceID
	}

	// 3. Duplicate check against current committed store
	if _, err := door.Get(req.DeviceID); err == nil {
		writeJSONAny(w, http.StatusConflict, map[string]string{"error": "device already exists", "reason": "duplicate_name"})
		return
	}

	// 4. Pending registration reservation
	s.devicePendingMu.Lock()
	if s.devicePending == nil {
		s.devicePending = make(map[string]*pendingDevice)
	}
	if _, pending := s.devicePending[req.DeviceID]; pending {
		s.devicePendingMu.Unlock()
		writeJSONAny(w, http.StatusConflict, map[string]string{"error": "an add of this device is already pending approval", "reason": "pending_add"})
		return
	}

	hub := s.approvalsViaRegistry()
	if hub == nil {
		s.devicePendingMu.Unlock()
		writeJSONAny(w, http.StatusServiceUnavailable, map[string]string{"error": "approval cards unavailable", "reason": "approvals_unavailable"})
		return
	}

	fp, credName := s.bearerFingerprintAndCred(r)
	if req.Fingerprint != "" {
		cleanFP := cleanFingerprint(req.Fingerprint)
		if cleanFP != "" {
			fp = cleanFP
		}
	}
	if fp == "" {
		fp = "fp-" + req.DeviceID
	}

	s.devicePending[req.DeviceID] = &pendingDevice{
		deviceID:     req.DeviceID,
		displayName:  req.DisplayName,
		rails:        req.RailsDeclared,
		registeredAt: time.Now().UTC(),
		fingerprint:  fp,
	}
	s.devicePendingMu.Unlock()
	s.pokeDeviceRail()

	// 5. Raise custody card via ApprovalHub.registerCustosNamed
	review := deviceRegisterReviewText(req.DeviceID, req.DisplayName, fp, req.RailsDeclared)
	label := "device-register:" + req.DeviceID
	hold := s.connectorCardHold()
	cardID, decision := hub.registerCustosNamed("", "", label, "", review, hold, nil)

	s.devicePendingMu.Lock()
	if p, ok := s.devicePending[req.DeviceID]; ok {
		p.cardID = cardID
	}
	s.devicePendingMu.Unlock()

	// 6. Await verdict in background
	go func() {
		allow := false
		select {
		case allow = <-decision:
		case <-time.After(hold + 10*time.Second):
			hub.CancelCard(cardID, "timeout", "custos")
		}

		s.devicePendingMu.Lock()
		delete(s.devicePending, req.DeviceID)
		s.devicePendingMu.Unlock()
		s.pokeDeviceRail()

		var railNames []string
		for _, r := range req.RailsDeclared {
			railNames = append(railNames, r.Name)
		}

		if allow {
			if err := door.Register(req.DeviceID, req.DisplayName, req.RailsDeclared); err != nil {
				log.Printf("surface: failed to persist device %s: %v", logSanitize(req.DeviceID), err)
			}
			s.pokeDeviceRail()

			payload := DeviceAuditPayload{
				DeviceID:    req.DeviceID,
				DisplayName: req.DisplayName,
				Fingerprint: fp,
				CredName:    credName,
				RailNames:   railNames,
				Actor:       "surface",
			}
			_ = door.Audit(AuditKindDeviceRegistered, payload)
		} else {
			payload := DeviceAuditPayload{
				DeviceID:    req.DeviceID,
				DisplayName: req.DisplayName,
				Fingerprint: fp,
				CredName:    credName,
				RailNames:   railNames,
				Actor:       "surface",
				Reason:      "denied",
			}
			_ = door.Audit(AuditKindDeviceRegistrationDenied, payload)
		}
	}()

	// 7. Response 202 {device_id, card_id}
	writeJSONAny(w, http.StatusAccepted, map[string]string{"device_id": req.DeviceID, "card_id": cardID})
}

// deviceStreamHandler serves GET /v1/devices/<id>/stream.
func (s *Server) deviceStreamHandler(w http.ResponseWriter, r *http.Request, id string) {
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.devicesBroker == nil {
		s.error(w, "broker unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := door.Get(id); err != nil {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"streaming unsupported"}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	sess, cleanup := s.devicesBroker.RegisterStream(id)
	defer cleanup()

	ticker := time.NewTicker(custosHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.doneCh:
			// Second concurrent stream took over
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case frame := <-sess.dispatchCh:
			data, err := json.Marshal(frame)
			if err == nil {
				_, _ = fmt.Fprintf(w, "event: action_dispatch\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}

// deviceActionReceiptHandler serves POST /v1/devices/<id>/actions/<action_id>/receipt.
func (s *Server) deviceActionReceiptHandler(w http.ResponseWriter, r *http.Request, id, actionID string) {
	if s.devicesBroker == nil {
		s.error(w, "broker unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  string          `json:"error,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": "bad_request"})
		return
	}

	resolved := s.devicesBroker.ResolveAction(id, actionID, req.OK, req.Result, req.Error)
	if !resolved {
		writeJSONAny(w, http.StatusGone, map[string]string{"error": "stale action receipt", "reason": "stale_action"})
		return
	}

	writeJSONAny(w, http.StatusOK, map[string]bool{"ok": true})
}

// deviceSyncHandler serves POST /v1/devices/<id>/sync.
func (s *Server) deviceSyncHandler(w http.ResponseWriter, r *http.Request, id string) {
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Rail     string          `json:"rail"`
		Snapshot json.RawMessage `json:"snapshot,omitempty"`
		Deltas   []DeviceDelta   `json:"deltas,omitempty"`
		Seq      int64           `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": "bad_request"})
		return
	}
	if req.Rail == "" {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "rail is required", "reason": "bad_request"})
		return
	}

	err := door.ApplySync(id, req.Rail, req.Snapshot, req.Deltas, req.Seq)
	if err != nil {
		var staleSeq *ErrStaleSeq
		switch {
		case errors.Is(err, ErrDeviceNotFound):
			writeJSONAny(w, http.StatusNotFound, map[string]string{"error": "device not found", "reason": "not_found"})
		case errors.Is(err, ErrRailNotFound):
			writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "reason": "unknown_rail"})
		case errors.Is(err, ErrRailDisabled):
			writeJSONAny(w, http.StatusConflict, map[string]string{"error": err.Error(), "reason": "rail_disabled"})
		case errors.As(err, &staleSeq):
			writeJSONAny(w, http.StatusConflict, map[string]string{"error": err.Error(), "reason": "stale_seq"})
		default:
			writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "reason": "sync_failed"})
		}
		return
	}

	s.pokeDeviceRail()
	writeJSONAny(w, http.StatusOK, map[string]bool{"ok": true})
}

// deviceRailsPatchHandler serves PATCH /v1/devices/<id>/rails.
func (s *Server) deviceRailsPatchHandler(w http.ResponseWriter, r *http.Request, id string) {
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}

	dev, err := door.Get(id)
	if err != nil {
		writeJSONAny(w, http.StatusNotFound, map[string]string{"error": "device not found", "reason": "not_found"})
		return
	}

	var req struct {
		Rail    string `json:"rail"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": "bad_request"})
		return
	}

	if req.Rail == "" {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "rail is required", "reason": "bad_request"})
		return
	}
	if req.Enabled == nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "enabled is required", "reason": "bad_request"})
		return
	}

	found := false
	for _, rail := range dev.Rails {
		if rail.Name == req.Rail {
			found = true
			break
		}
	}
	if !found {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("rail %q not declared on device", req.Rail), "reason": "unknown_rail"})
		return
	}

	hub := s.approvalsViaRegistry()
	if hub == nil {
		writeJSONAny(w, http.StatusServiceUnavailable, map[string]string{"error": "approval cards unavailable", "reason": "approvals_unavailable"})
		return
	}

	review := deviceRailToggleReviewText(id, dev.DisplayName, req.Rail, *req.Enabled)
	label := "device-rail-toggle:" + id + "/" + req.Rail
	hold := s.connectorCardHold()
	cardID, decision := hub.registerCustosNamed("", "", label, "", review, hold, nil)

	go func() {
		allow := false
		select {
		case allow = <-decision:
		case <-time.After(hold + 10*time.Second):
			hub.CancelCard(cardID, "timeout", "custos")
		}

		if allow {
			if err := door.SetRailEnabled(id, req.Rail, *req.Enabled); err != nil {
				log.Printf("surface: failed to toggle rail %s on device %s: %v", logSanitize(req.Rail), logSanitize(id), err) //nolint:gosec // logSanitize neutralizes control chars
				return
			}
			s.pokeDeviceRail()

			payload := DeviceAuditPayload{
				DeviceID:    id,
				DisplayName: dev.DisplayName,
				RailNames:   []string{req.Rail},
				Actor:       "surface",
				Reason:      fmt.Sprintf("enabled=%v", *req.Enabled),
			}
			_ = door.Audit(AuditKindDeviceRailToggled, payload)
		}
	}()

	writeJSONAny(w, http.StatusAccepted, map[string]string{"device_id": id, "rail": req.Rail, "card_id": cardID})
}

// deviceDeleteHandler serves DELETE /v1/devices/<id>.
func (s *Server) deviceDeleteHandler(w http.ResponseWriter, _ *http.Request, id string) {
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := door.ValidateID(id); err != nil {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	dev, err := door.Get(id)
	displayName := id
	if err == nil {
		displayName = dev.DisplayName
	}

	if err := door.Forget(id); err != nil {
		s.error(w, "forget failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if s.devicesBroker != nil {
		s.devicesBroker.CloseStream(id)
	}

	s.devicePendingMu.Lock()
	delete(s.devicePending, id)
	s.devicePendingMu.Unlock()

	s.pokeDeviceRail()

	payload := DeviceAuditPayload{
		DeviceID:    id,
		DisplayName: displayName,
		Actor:       "surface",
	}
	_ = door.Audit(AuditKindDeviceForgotten, payload)

	w.WriteHeader(http.StatusNoContent)
}

func deviceRegisterReviewText(devID, displayName, fp string, rails []DeclaredRail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REGISTER device %q (id: %s)\n", displayName, devID)
	fmt.Fprintf(&b, "fingerprint: %s\n", fp)
	fmt.Fprintf(&b, "declared rails (%d):\n", len(rails))
	for _, r := range rails {
		flag := "read_only"
		if !r.ReadOnly {
			flag = "action"
		}
		fmt.Fprintf(&b, "  - %s [%s]\n", r.Name, flag)
	}
	return b.String()
}

func deviceRailToggleReviewText(devID, displayName, rail string, enabled bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TOGGLE rail %q on device %q (id: %s) -> enabled: %v\n", rail, displayName, devID, enabled)
	return b.String()
}

func (s *Server) bearerFingerprintAndCred(r *http.Request) (string, string) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", "bearer"
	}
	rawToken := auth[len(prefix):]
	h := sha256.Sum256([]byte(rawToken))
	fp := hex.EncodeToString(h[:8])

	credName := "pairing-token"
	if s.Store != nil {
		entries, err := s.Store.readEntries()
		if err == nil {
			for _, e := range entries {
				stored, err := hex.DecodeString(e.Hash)
				if err == nil && len(stored) == 32 {
					if subtle.ConstantTimeCompare(h[:], stored) == 1 {
						if e.Label != "" {
							credName = e.Label
						}
						break
					}
				}
			}
		}
	}
	return fp, credName
}
