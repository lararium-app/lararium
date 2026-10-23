package custos

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// TestM15_RedactURL verifies that errors containing URLs with sensitive query parameters
// (such as refresh_token) have the query string stripped while preserving host, path,
// status codes, and status text (M15, CUSTOS-SPEC §4.5).
func TestM15_RedactURL(t *testing.T) {
	origErr := errors.New("request failed: https://accounts.google.com/token?refresh_token=SECRET (status 400 Bad Request)")
	redacted := redactURL(origErr)
	if redacted == nil {
		t.Fatal("redactURL returned nil")
	}

	msg := redacted.Error()
	if strings.Contains(msg, "refresh_token") {
		t.Errorf("expected no 'refresh_token' substring, got: %q", msg)
	}
	if strings.Contains(msg, "SECRET") {
		t.Errorf("expected sensitive parameter value stripped, got: %q", msg)
	}
	if !strings.Contains(msg, "https://accounts.google.com/token") {
		t.Errorf("expected host and path preserved, got: %q", msg)
	}
	if !strings.Contains(msg, "400 Bad Request") {
		t.Errorf("expected status text preserved, got: %q", msg)
	}

	// Verify net/url.Error handling
	u, _ := url.Parse("https://accounts.google.com/token?refresh_token=SECRET")
	urlErr := &url.Error{
		Op:  "Post",
		URL: u.String(),
		Err: errors.New("connection reset"),
	}
	redactedURLErr := redactURL(urlErr)
	urlMsg := redactedURLErr.Error()
	if strings.Contains(urlMsg, "refresh_token") {
		t.Errorf("url.Error contains refresh_token: %q", urlMsg)
	}
	if !strings.Contains(urlMsg, "https://accounts.google.com/token") {
		t.Errorf("url.Error missing base URL: %q", urlMsg)
	}
	if !strings.Contains(urlMsg, "connection reset") {
		t.Errorf("url.Error underlying error lost: %q", urlMsg)
	}

	// Nil error returns nil
	if redactURL(nil) != nil {
		t.Error("redactURL(nil) must return nil")
	}

	// Error with no URL is unchanged
	plainErr := errors.New("plain error with no url")
	if redactURL(plainErr).Error() != plainErr.Error() {
		t.Errorf("plain error modified: %v", redactURL(plainErr))
	}
}
