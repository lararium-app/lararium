package surface

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// pairCodeShapeRe enforces `pair1_` + exactly 20 base62 chars.
var pairCodeShapeRe = regexp.MustCompile(`^pair1_[0-9A-Za-z]{20}$`)

func validPairCodeShape(code string) bool {
	return pairCodeShapeRe.MatchString(code)
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
	if s.PairStore == nil {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

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

	var req struct {
		Code       string `json:"code"`
		DeviceName string `json:"device_name"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
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
		// Mint API token with label "paired:" + userID
		label := "paired:" + userID
		token, err := s.Store.Create(label)
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
