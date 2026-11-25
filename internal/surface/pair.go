package surface

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// RedeemResult classifies the result of a pairing code redemption.
type RedeemResult int

const (
	// RedeemPaired means a correct code — sender is now the owner.
	RedeemPaired RedeemResult = iota
	// RedeemInvalid means a wrong or expired code.
	RedeemInvalid
	// RedeemAlready means a code was presented while already paired.
	RedeemAlready
)

// PairStore is the storage interface required to redeem pairing codes.
type PairStore interface {
	Redeem(userID, code string, now time.Time) (RedeemResult, error)
}

// EnrollStore is the storage interface required to redeem per-device enrollment codes.
// *devices.EnrollStore satisfies this interface directly.
type EnrollStore interface {
	Redeem(code string, now time.Time) (bool, error)
}

// pairCodeShapeRe enforces `pair1_` + exactly 20 base62 chars.
var pairCodeShapeRe = regexp.MustCompile(`^pair1_[0-9A-Za-z]{20}$`)

// enrollCodeShapeRe enforces `dev1_` + exactly 20 base62 chars.
var enrollCodeShapeRe = regexp.MustCompile(`^dev1_[0-9A-Za-z]{20}$`)

func validPairCodeShape(code string) bool {
	return pairCodeShapeRe.MatchString(code)
}

func validEnrollCodeShape(code string) bool {
	return enrollCodeShapeRe.MatchString(code)
}

func stripControlAndCap(raw string, maxRunes int) string {
	var sb strings.Builder
	for _, r := range raw {
		if unicode.IsControl(r) {
			continue
		}
		sb.WriteRune(r)
	}
	trimmed := strings.TrimSpace(sb.String())
	runes := []rune(trimmed)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return strings.TrimSpace(string(runes))
}

func sanitizeEnrollDisplayName(raw string) string {
	cleaned := stripControlAndCap(raw, 64)
	if cleaned == "" {
		return "device"
	}
	return cleaned
}

func sanitizeEnrollAppVersion(raw string) string {
	return stripControlAndCap(raw, 32)
}

func normalizeEnrollPlatform(raw string) string {
	switch raw {
	case "android", "ios":
		return raw
	default:
		return "other"
	}
}

// deriveDeviceID derives a daemon-side device_id slug from displayName:
// lowercase, runs of non-[a-z0-9] collapsed to '-', trimmed of leading/trailing '-',
// truncated to 28 chars, fallback "device". Always satisfies ValidateDeviceID.
func deriveDeviceID(displayName string) string {
	var sb strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(displayName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			lastDash = false
		} else if sb.Len() > 0 && !lastDash {
			sb.WriteByte('-')
			lastDash = true
		}
	}
	res := strings.Trim(sb.String(), "-")
	if len(res) > 28 {
		res = strings.TrimRight(res[:28], "-")
	}
	if res == "" {
		return "device"
	}
	return res
}

type pairRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version"`
}

// sanitizeDeviceName sanitizes a raw device name into a safe printable subset,
// lowercase, spaces and underscores converted to single dashes, other characters
// stripped, capped at 64 runes. If empty or sanitized away entirely, it falls
// back to "device".
func sanitizeDeviceName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "device"
	}

	runes := []rune(trimmed)
	if len(runes) > 64 {
		runes = runes[:64]
	}

	var sb strings.Builder
	lastWasDash := false
	for _, r := range runes {
		switch {
		case unicode.IsSpace(r) || r == '_' || r == '-':
			if sb.Len() > 0 && !lastWasDash {
				sb.WriteByte('-')
				lastWasDash = true
			}
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			sb.WriteRune(r)
			lastWasDash = false
		case r >= 'A' && r <= 'Z':
			sb.WriteRune(unicode.ToLower(r))
			lastWasDash = false
		}
	}

	res := strings.Trim(sb.String(), "-")
	if res == "" {
		return "device"
	}
	return res
}

type pairLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newPairLimiter() *pairLimiter {
	return &pairLimiter{
		failures: make(map[string][]time.Time),
	}
}

func (l *pairLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	return len(l.failures[ip]) < 10
}

func (l *pairLimiter) recordFailure(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	l.failures[ip] = append(l.failures[ip], now)
}

func (l *pairLimiter) prune(now time.Time) {
	cutoff := now.Add(-1 * time.Minute)
	for ip, times := range l.failures {
		var kept []time.Time
		for _, t := range times {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.failures, ip)
		} else {
			l.failures[ip] = kept
		}
	}
}

func (s *Server) getPairLimiter() *pairLimiter {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pairLimiter == nil {
		s.pairLimiter = newPairLimiter()
	}
	return s.pairLimiter
}

// pairHandler handles POST /v1/pair: redeem a pairing code and return an API token.
func (s *Server) pairHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := stripHostPort(r.RemoteAddr)
	if clientIP == "" {
		clientIP = "unknown"
	}

	limiter := s.getPairLimiter()
	now := time.Now()
	if !limiter.allow(clientIP, now) {
		writeJSONBody(w, http.StatusTooManyRequests, `{"error":"too many pairing attempts"}`)
		return
	}

	const maxPairBody = 4096 // 4 KiB
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPairBody+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONBody(w, http.StatusRequestEntityTooLarge, `{"error":"body too large"}`)
			return
		}
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(raw) > maxPairBody {
		writeJSONBody(w, http.StatusRequestEntityTooLarge, `{"error":"body too large"}`)
		return
	}

	var req pairRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.HasPrefix(req.Code, "dev1_") {
		s.deviceEnroll(w, r, req, clientIP, limiter, now)
		return
	}

	if s.PairStore == nil {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	if !validPairCodeShape(req.Code) {
		writeJSONBody(w, http.StatusBadRequest, `{"error":"invalid pairing code"}`)
		return
	}

	userID := "app:" + sanitizeDeviceName(req.DeviceName)

	res, err := s.PairStore.Redeem(userID, req.Code, now)
	if err != nil {
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch res {
	case RedeemPaired:
		// Mint API token with label "paired:" + userID, marked as a
		// device credential so a verdict from this device is attributed
		// to the device channel rather than to the web dashboard
		// (CUSTOS-SPEC §6.4b amendment v14). The marker is set here, by
		// the daemon, precisely because the label is partly caller-
		// supplied and so must never be the thing attribution keys on.
		label := "paired:" + userID
		token, err := s.Store.CreateKind(label, TokenKindDevice)
		if err != nil {
			writeJSONBody(w, http.StatusInternalServerError, fmt.Sprintf(`{"error":"pairing succeeded but token mint failed; run: hearthd unpair %s and create a new code"}`, userID))
			return
		}
		writeJSONAny(w, http.StatusOK, map[string]string{
			"token":   token,
			"user_id": userID,
		})
	case RedeemInvalid:
		limiter.recordFailure(clientIP, now)
		writeJSONBody(w, http.StatusUnauthorized, `{"error":"invalid or expired pairing code"}`)
	case RedeemAlready:
		writeJSONBody(w, http.StatusConflict, `{"error":"this hearth is already paired"}`)
	default:
		s.error(w, "internal error", http.StatusInternalServerError)
	}
}

// deviceEnroll handles the dev1_ enrollment branch of POST /v1/pair (DEVICE-RAILS-SPEC D-ENROLL).
// Strict order: availability checks -> shape check -> limiter -> one-time code redeem ->
// daemon-side device_id derivation -> empty-rails device registration -> bound token mint ->
// device_enrolled audit -> 200 {token, device_id}.
func (s *Server) deviceEnroll(w http.ResponseWriter, _ *http.Request, req pairRequest, clientIP string, limiter *pairLimiter, now time.Time) {
	if s.Enroll == nil {
		s.error(w, "device enrollment unavailable", http.StatusServiceUnavailable)
		return
	}

	// 1. Shape check: generic 401 + limiter failure, no hints.
	if !validEnrollCodeShape(req.Code) {
		limiter.recordFailure(clientIP, now)
		writeJSONBody(w, http.StatusUnauthorized, `{"error":"invalid or expired pairing code"}`)
		return
	}

	// 2. Shared limiter check.
	if !limiter.allow(clientIP, now) {
		writeJSONBody(w, http.StatusTooManyRequests, `{"error":"too many pairing attempts"}`)
		return
	}

	// Check door and token store availability BEFORE consuming the one-time code.
	door := s.getDeviceDoor()
	if door == nil {
		s.error(w, "devices door unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.Store == nil {
		s.error(w, "token store unavailable", http.StatusServiceUnavailable)
		return
	}

	// 3. Consume one-time code FIRST so concurrent redeems can never mint two devices.
	ok, err := s.Enroll.Redeem(req.Code, now)
	if err != nil {
		log.Printf("surface: device enroll redeem error: %v", err)
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		limiter.recordFailure(clientIP, now)
		writeJSONBody(w, http.StatusUnauthorized, `{"error":"invalid or expired pairing code"}`)
		return
	}

	const consumedCure = `{"error":"device enrollment failed after pairing code was consumed; mint a new code with: hearthd pair create --device","reason":"store_error"}`

	// 4. Derive device_id daemon-side from device_name; resolve collisions with -<4hex>.
	displayName := sanitizeEnrollDisplayName(req.DeviceName)
	platform := normalizeEnrollPlatform(req.Platform)
	appVersion := sanitizeEnrollAppVersion(req.AppVersion)

	baseID := deriveDeviceID(displayName)
	if err := door.ValidateID(baseID); err != nil {
		baseID = "device"
	}

	deviceID := baseID
	if _, err := door.Get(deviceID); err == nil {
		// Cap prefix at 27 chars so prefix + "-" + 4 hex <= 32 (ValidateDeviceID max).
		prefix := baseID
		if len(prefix) > 27 {
			prefix = strings.TrimRight(prefix[:27], "-")
		}
		if prefix == "" {
			prefix = "device"
		}
		resolved := false
		for range 5 {
			var b [2]byte
			if _, rerr := rand.Read(b[:]); rerr != nil {
				log.Printf("surface: device enroll rand failed: %v", rerr)
				writeJSONBody(w, http.StatusInternalServerError, consumedCure)
				return
			}
			cand := prefix + "-" + hex.EncodeToString(b[:])
			if _, gerr := door.Get(cand); gerr != nil {
				deviceID = cand
				resolved = true
				break
			}
		}
		if !resolved {
			writeJSONBody(w, http.StatusInternalServerError, consumedCure)
			return
		}
	}

	// 5. Persist empty-rails device record; later /v1/devices/register upserts rails.
	if err := door.Register(deviceID, displayName, nil); err != nil {
		log.Printf("surface: device enroll register failed for %s: %v", logSanitize(deviceID), err)
		writeJSONBody(w, http.StatusInternalServerError, consumedCure)
		return
	}
	s.pokeDeviceRail()

	// 6. Mint per-device bearer token bound to deviceID.
	token, err := s.Store.CreateDeviceToken("device:"+deviceID, deviceID, displayName, platform, appVersion)
	if err != nil {
		log.Printf("surface: device enroll token mint failed for %s: %v", logSanitize(deviceID), err)
		writeJSONBody(w, http.StatusInternalServerError, consumedCure)
		return
	}

	// 7. Record device_enrolled audit event.
	_ = door.Audit(AuditKindDeviceEnrolled, DeviceAuditPayload{
		DeviceID:    deviceID,
		DisplayName: displayName,
		Platform:    platform,
		AppVersion:  appVersion,
		Actor:       "surface",
	})

	// 8. Return exactly {token, device_id}.
	writeJSONAny(w, http.StatusOK, map[string]string{
		"token":     token,
		"device_id": deviceID,
	})
}
