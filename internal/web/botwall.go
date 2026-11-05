package web

import (
	"bytes"
	"net/http"
	"strings"
)

// BlockedPrefix is the required prefix for bot-wall challenge/block responses.
const BlockedPrefix = "[blocked] This site refused automated access."

const botWallHeadBytes = 8192

// challengePhrases are matched case-insensitively on HTTP 403 or 503 responses.
var challengePhrases = [][]byte{
	[]byte("just a moment"),
	[]byte("checking for viruses"),
	[]byte("attention required"),
	[]byte("access denied"),
	[]byte("are you a human"),
	[]byte("px-captcha"),
}

// unambiguousChallengePhrases appear almost exclusively on interstitial
// challenge pages. When the HTTP status is unknown (0 — e.g. Chrome's
// navigation timing unavailable), only these may trigger detection, so that
// ordinary content merely mentioning "access denied" cannot false-positive.
var unambiguousChallengePhrases = [][]byte{
	[]byte("just a moment"),
	[]byte("checking for viruses"),
	[]byte("are you a human"),
	[]byte("px-captcha"),
}

// DetectBotWall returns true if the HTTP status and response body indicate
// an automated-access challenge or block page.
//
// Rules:
//  1. HTTP 403 or 503 with body matching (case-insensitive) any of:
//     "just a moment", "checking for viruses", "attention required",
//     "access denied", "are you a human", "px-captcha".
//     When statusCode is 0 (status unknown) only the unambiguous
//     challenge phrases count.
//  2. HTTP 500 or 403 body containing "Reference #" (case-insensitive).
//  3. eBay-style "SORRY" first line (on 403/500/503 only).
func DetectBotWall(statusCode int, body []byte) bool {
	head := body
	if len(head) > botWallHeadBytes {
		head = head[:botWallHeadBytes]
	}
	lowerHead := bytes.ToLower(head)

	// 1. HTTP 403 or 503 with challenge phrases; status 0 with unambiguous ones only.
	phrases := challengePhrases
	if statusCode == 0 {
		phrases = unambiguousChallengePhrases
	}
	if statusCode == http.StatusForbidden || statusCode == http.StatusServiceUnavailable || statusCode == 0 {
		for _, phrase := range phrases {
			if bytes.Contains(lowerHead, phrase) {
				return true
			}
		}
	}

	// 2. HTTP 500 or 403 body containing "Reference #"
	if statusCode == http.StatusInternalServerError || statusCode == http.StatusForbidden {
		if bytes.Contains(lowerHead, []byte("reference #")) {
			return true
		}
	}

	// 3. eBay-style "SORRY" first line
	if statusCode == http.StatusForbidden || statusCode == http.StatusServiceUnavailable || statusCode == http.StatusInternalServerError {
		trimmed := bytes.TrimSpace(head)
		if len(trimmed) > 0 {
			firstLine := trimmed
			if idx := bytes.IndexAny(trimmed, "\r\n"); idx != -1 {
				firstLine = trimmed[:idx]
			}
			firstLine = bytes.TrimSpace(firstLine)
			if bytes.EqualFold(firstLine, []byte("sorry")) || bytes.HasPrefix(bytes.ToUpper(firstLine), []byte("SORRY")) {
				return true
			}
		}
	}

	return false
}

// FormatBlocked formats a blocked result starting with BlockedPrefix
// followed by up to 200 characters of the page body.
func FormatBlocked(body []byte) string {
	s := strings.TrimSpace(string(body))
	runes := []rune(s)
	if len(runes) > 200 {
		s = string(runes[:200])
	}
	if s == "" {
		return BlockedPrefix
	}
	return BlockedPrefix + "\n\n" + s
}
