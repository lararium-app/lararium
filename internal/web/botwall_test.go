package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestDetectBotWall(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		// 1. HTTP 403 or 503 with challenge phrases (case-insensitive)
		{
			name:       "403 Cloudflare Just a moment",
			statusCode: http.StatusForbidden,
			body:       []byte("<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>Just a moment...</body></html>"),
			want:       true,
		},
		{
			name:       "503 Cloudflare Just a moment uppercase",
			statusCode: http.StatusServiceUnavailable,
			body:       []byte("JUST A MOMENT... Please wait while we verify your browser."),
			want:       true,
		},
		{
			name:       "403 checking for viruses",
			statusCode: http.StatusForbidden,
			body:       []byte("Checking for viruses before granting access."),
			want:       true,
		},
		{
			name:       "503 attention required",
			statusCode: http.StatusServiceUnavailable,
			body:       []byte("Attention Required! | Cloudflare"),
			want:       true,
		},
		{
			name:       "403 access denied",
			statusCode: http.StatusForbidden,
			body:       []byte("Access Denied: You do not have permission to access this server."),
			want:       true,
		},
		{
			name:       "503 are you a human",
			statusCode: http.StatusServiceUnavailable,
			body:       []byte("Are You A Human? Please complete the puzzle below."),
			want:       true,
		},
		{
			name:       "403 px-captcha PerimeterX",
			statusCode: http.StatusForbidden,
			body:       []byte(`<div id="px-captcha">Please solve captcha</div>`),
			want:       true,
		},

		// 2. HTTP 500 or 403 body containing "Reference #"
		{
			name:       "500 Amazon/Akamai Reference #",
			statusCode: http.StatusInternalServerError,
			body:       []byte("Internal Server Error\n\nReference #18.823412.1699999999.0"),
			want:       true,
		},
		{
			name:       "403 Akamai Reference # lowercase",
			statusCode: http.StatusForbidden,
			body:       []byte("<html><body>Error: reference #9.abc.123</body></html>"),
			want:       true,
		},

		// 3. eBay-style "SORRY" first line
		{
			name:       "403 eBay SORRY with newline and details",
			statusCode: http.StatusForbidden,
			body:       []byte("SORRY\nWe could not process your request."),
			want:       true,
		},
		{
			name:       "403 eBay SORRY first line CRLF",
			statusCode: http.StatusForbidden,
			body:       []byte("SORRY\r\nAccess blocked."),
			want:       true,
		},
		{
			name:       "403 eBay SORRY single word trimmed",
			statusCode: http.StatusForbidden,
			body:       []byte("  SORRY  "),
			want:       true,
		},
		{
			name:       "403 eBay lowercase sorry first line",
			statusCode: http.StatusForbidden,
			body:       []byte("sorry\nautomated access refused"),
			want:       true,
		},
		{
			name:       "0 status (browser) with challenge phrase",
			statusCode: 0,
			body:       []byte("Just a moment... verifying"),
			want:       true,
		},
		{
			name:       "0 status with generic 'access denied' text is NOT blocked",
			statusCode: 0,
			body:       []byte("<html><body>Tutorial: servers sometimes reply Access Denied and Attention Required errors.</body></html>"),
			want:       false,
		},
		{
			name:       "0 status with sorry first line is NOT blocked",
			statusCode: 0,
			body:       []byte("SORRY we could not process your order"),
			want:       false,
		},

		// Negative test cases (NOT-blocked pages)
		{
			name:       "200 OK standard HTML page",
			statusCode: http.StatusOK,
			body:       []byte("<!DOCTYPE html><html><head><title>Welcome</title></head><body><h1>Hello World</h1><p>Normal page content.</p></body></html>"),
			want:       false,
		},
		{
			name:       "200 OK API JSON response",
			statusCode: http.StatusOK,
			body:       []byte(`{"status":"success","data":{"items":[1,2,3]}}`),
			want:       false,
		},
		{
			name:       "200 OK plain text notes",
			statusCode: http.StatusOK,
			body:       []byte("Meeting notes for team sprint planning."),
			want:       false,
		},
		{
			name:       "200 OK containing sorry in paragraph text",
			statusCode: http.StatusOK,
			body:       []byte("We are sorry for any inconvenience caused by our scheduled maintenance."),
			want:       false,
		},
		{
			name:       "200 OK containing access denied text",
			statusCode: http.StatusOK,
			body:       []byte("Article: How to handle access denied errors in Linux permissions."),
			want:       false,
		},
		{
			name:       "200 OK containing reference # in body",
			statusCode: http.StatusOK,
			body:       []byte("Your order confirmation reference #123456 has been processed."),
			want:       false,
		},
		{
			name:       "404 Not Found standard page",
			statusCode: http.StatusNotFound,
			body:       []byte("404 Not Found: The requested resource does not exist."),
			want:       false,
		},
		{
			name:       "500 Internal Server Error without Reference #",
			statusCode: http.StatusInternalServerError,
			body:       []byte("500 Internal Server Error: database connection timed out."),
			want:       false,
		},
		{
			name:       "403 Forbidden without bot challenge phrase",
			statusCode: http.StatusForbidden,
			body:       []byte("Directory listing forbidden by server configuration."),
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectBotWall(tc.statusCode, tc.body)
			if got != tc.want {
				t.Errorf("DetectBotWall(%d, %q) = %v, want %v", tc.statusCode, string(tc.body), got, tc.want)
			}
		})
	}
}

func TestFormatBlocked(t *testing.T) {
	t.Run("empty body returns prefix only", func(t *testing.T) {
		got := FormatBlocked(nil)
		if got != BlockedPrefix {
			t.Errorf("got %q, want %q", got, BlockedPrefix)
		}
	})

	t.Run("whitespace body returns prefix only", func(t *testing.T) {
		got := FormatBlocked([]byte("   \n\t  "))
		if got != BlockedPrefix {
			t.Errorf("got %q, want %q", got, BlockedPrefix)
		}
	})

	t.Run("short body under 200 chars included in full", func(t *testing.T) {
		body := []byte("Cloudflare Ray ID: 123456. Attention required.")
		got := FormatBlocked(body)
		want := BlockedPrefix + "\n\n" + string(body)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !strings.HasPrefix(got, BlockedPrefix) {
			t.Errorf("expected prefix %q", BlockedPrefix)
		}
	})

	t.Run("body over 200 chars truncated to 200 chars", func(t *testing.T) {
		longBody := strings.Repeat("A", 300)
		got := FormatBlocked([]byte(longBody))
		wantPrefix := BlockedPrefix + "\n\n" + strings.Repeat("A", 200)
		if got != wantPrefix {
			t.Errorf("expected 200 characters truncated, got length %d", len(got))
		}
	})
}
