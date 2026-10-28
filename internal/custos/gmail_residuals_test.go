package custos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGmailResiduals_NilVaultAndLocked exercises the nil-vault internal branch
// (gmail.go ~182-183) and ErrCustosLocked -> locked branch (~190-192).
func TestGmailResiduals_NilVaultAndLocked(t *testing.T) {
	ctx := context.Background()
	expiredCred := Credential{
		AccessToken:  "old-token",
		AccessExpiry: time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
	}

	t.Run("nil_vault_returns_internal", func(t *testing.T) {
		g := NewGmailConnector(nil)
		tok, err := g.accessToken(ctx, expiredCred)
		if tok != "" {
			t.Errorf("expected empty token, got %q", tok)
		}
		var we *workerError
		if !errors.As(err, &we) || we.code != ErrCodeInternal || we.detail != detailInternal {
			t.Fatalf("expected internal worker error, got %v", err)
		}
	})

	t.Run("locked_vault_returns_locked", func(t *testing.T) {
		dir := t.TempDir()
		v := NewVault(dir, time.Second)
		if err := v.Init("test-pass-1234"); err != nil {
			t.Fatalf("v.Init: %v", err)
		}
		// Vault is initialized but locked (IsUnlocked() is false).
		g := NewGmailConnector(v)
		tok, err := g.accessToken(ctx, expiredCred)
		if tok != "" {
			t.Errorf("expected empty token, got %q", tok)
		}
		var we *workerError
		if !errors.As(err, &we) || we.code != ErrCodeLocked || we.detail != detailLocked {
			t.Fatalf("expected locked worker error, got %v", err)
		}
	})

	t.Run("credential_gone_returns_denied", func(t *testing.T) {
		dir := t.TempDir()
		v := NewVault(dir, time.Second)
		if err := v.Init("test-pass-1234"); err != nil {
			t.Fatalf("v.Init: %v", err)
		}
		if err := v.Unlock("test-pass-1234", false); err != nil {
			t.Fatalf("v.Unlock: %v", err)
		}
		// Vault is unlocked but has no "gmail" credential.
		g := NewGmailConnector(v)
		tok, err := g.accessToken(ctx, expiredCred)
		if tok != "" {
			t.Errorf("expected empty token, got %q", tok)
		}
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

// TestGmailResiduals_AssertURLHost exercises assertURLHost parse-error branch
// and non-loopback http refusal under override (~202-215).
func TestGmailResiduals_AssertURLHost(t *testing.T) {
	tests := []struct {
		name     string
		override string
		target   string
		wantErr  bool
	}{
		{
			name:     "parse_error_invalid_url",
			override: "",
			target:   "://not-a-valid-url",
			wantErr:  true,
		},
		{
			name:     "parse_error_bad_bracket",
			override: "",
			target:   "http://[::1:80",
			wantErr:  true,
		},
		{
			name:     "non_loopback_domain_under_override",
			override: "http://example.com",
			target:   "http://example.com/gmail/v1/users/me/messages/send",
			wantErr:  true,
		},
		{
			name:     "non_loopback_ip_under_override",
			override: "http://192.168.1.5",
			target:   "http://192.168.1.5:8080/send",
			wantErr:  true,
		},
		{
			name:     "loopback_ipv4_under_override",
			override: "http://127.0.0.1:8080",
			target:   "http://127.0.0.1:8080/send",
			wantErr:  false,
		},
		{
			name:     "loopback_ipv6_under_override",
			override: "http://[::1]:8080",
			target:   "http://[::1]:8080/send",
			wantErr:  false,
		},
		{
			name:     "production_https_api_host",
			override: "",
			target:   "https://gmail.googleapis.com/gmail/v1/users/me/messages/send",
			wantErr:  false,
		},
		{
			name:     "production_https_subdomain",
			override: "",
			target:   "https://content.googleapis.com/gmail/v1/users/me/messages/send",
			wantErr:  false,
		},
		{
			name:     "disallowed_https_host",
			override: "",
			target:   "https://evil.com/gmail/v1/users/me/messages/send",
			wantErr:  true,
		},
		{
			name:     "loopback_http_without_override",
			override: "",
			target:   "http://127.0.0.1:8080/send",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGmailConnector(nil)
			g.SetBaseURLOverride(tc.override)
			err := g.assertURLHost(tc.target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("assertURLHost(%q) err = %v, wantErr = %v", tc.target, err, tc.wantErr)
			}
			if tc.wantErr {
				var we *workerError
				if !errors.As(err, &we) || we.code != ErrCodeTransport || we.detail != detailTransport {
					t.Fatalf("expected transport worker error, got %v", err)
				}
			}
		})
	}
}

// TestGmailResiduals_SendAndReadMalformedJSON exercises send non-2xx-JSON
// and 2xx-JSON-missing-id -> internal (~250-258) and read non-JSON -> internal (~286-289).
func TestGmailResiduals_SendAndReadMalformedJSON(t *testing.T) {
	ctx := context.Background()
	freshCred := Credential{
		AccessToken:  "ya29.test-valid-access-token",
		AccessExpiry: time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}

	t.Run("send_non_json_response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-json-content"))
		}))
		defer srv.Close()

		g := NewGmailConnector(nil)
		g.SetBaseURLOverride(srv.URL)
		_, err := g.execSend(ctx, &sendArgs{To: []string{"alice@example.com"}, Subject: "sub", Body: "msg"}, freshCred)
		var we *workerError
		if !errors.As(err, &we) || we.code != ErrCodeInternal || we.detail != detailInternal {
			t.Fatalf("expected internal worker error, got %v", err)
		}
	})

	t.Run("send_json_missing_id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":""}`))
		}))
		defer srv.Close()

		g := NewGmailConnector(nil)
		g.SetBaseURLOverride(srv.URL)
		_, err := g.execSend(ctx, &sendArgs{To: []string{"alice@example.com"}, Subject: "sub", Body: "msg"}, freshCred)
		var we *workerError
		if !errors.As(err, &we) || we.code != ErrCodeInternal || we.detail != detailInternal {
			t.Fatalf("expected internal worker error on empty ID, got %v", err)
		}
	})

	t.Run("read_non_json_response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-json-message-data"))
		}))
		defer srv.Close()

		g := NewGmailConnector(nil)
		g.SetBaseURLOverride(srv.URL)
		_, err := g.execRead(ctx, &readArgs{ID: "msg-safe-123"}, freshCred)
		var we *workerError
		if !errors.As(err, &we) || we.code != ErrCodeInternal || we.detail != detailInternal {
			t.Fatalf("expected internal worker error, got %v", err)
		}
	})
}

// truncatingBody streams partial content and then truncates with an error.
type truncatingBody struct {
	readDone bool
}

func (tb *truncatingBody) Read(p []byte) (int, error) {
	if !tb.readDone {
		tb.readDone = true
		n := copy(p, []byte("HTTP header arrived ok, but body cuts off here"))
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (tb *truncatingBody) Close() error { return nil }

type truncatingTransport struct{}

func (tt *truncatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       &truncatingBody{},
		Header:     make(http.Header),
	}, nil
}

// TestGmailResiduals_MidBodyReadFailure exercises the mid-body read failure -> transport
// path (~335-346) via a fake that delivers headers then truncates mid-body.
func TestGmailResiduals_MidBodyReadFailure(t *testing.T) {
	g := NewGmailConnector(nil)
	g.SetBaseURLOverride("http://127.0.0.1:8080")
	g.SetHTTPClient(&http.Client{
		Transport: &truncatingTransport{},
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:8080/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	_, _, err = g.do(req)
	if err == nil {
		t.Fatal("expected error on mid-body truncation, got nil")
	}
	var we *workerError
	if !errors.As(err, &we) || we.code != ErrCodeTransport || we.detail != detailTransport {
		t.Fatalf("expected transport worker error (%s: %s), got %v", ErrCodeTransport, detailTransport, err)
	}
}

type failEntropyReader struct{}

func (f failEntropyReader) Read(p []byte) (int, error) {
	return 0, errors.New("entropy source failed: /dev/urandom exhausted")
}

// TestGmailResiduals_RandMessageIDFailure exercises crypto/rand failure path
// (~378-384) via the package-level randReader seam.
func TestGmailResiduals_RandMessageIDFailure(t *testing.T) {
	orig := randReader
	randReader = failEntropyReader{}
	defer func() { randReader = orig }()

	// 1. Direct randMessageID failure:
	mid, err := randMessageID()
	if mid != "" || err == nil {
		t.Fatalf("randMessageID expected failure, got mid=%q, err=%v", mid, err)
	}

	// 2. buildRFC2822 failure propagating randMessageID error:
	raw, err := buildRFC2822(&sendArgs{To: []string{"a@example.com"}, Subject: "s", Body: "b"})
	if raw != "" || err == nil {
		t.Fatalf("buildRFC2822 expected failure, got raw=%q, err=%v", raw, err)
	}

	// 3. Connector execSend path mapping to internal:
	ctx := context.Background()
	freshCred := Credential{
		AccessToken:  "ya29.valid-token",
		AccessExpiry: time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	g := NewGmailConnector(nil)
	_, execErr := g.execSend(ctx, &sendArgs{To: []string{"a@example.com"}, Subject: "s", Body: "b"}, freshCred)
	var we *workerError
	if !errors.As(execErr, &we) || we.code != ErrCodeInternal || we.detail != detailInternal {
		t.Fatalf("expected internal worker error on rand failure, got %v", execErr)
	}
}

// TestGmailResiduals_ValidRFC2822Addr exercises validRFC2822Addr edges and 254-byte cap (~391-406).
func TestGmailResiduals_ValidRFC2822Addr(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{name: "empty", addr: "", want: false},
		{name: "exact_254_bytes_valid", addr: strings.Repeat("a", 242) + "@example.com", want: true},
		{name: "exceeds_254_bytes", addr: strings.Repeat("a", 243) + "@example.com", want: false},
		{name: "no_at_symbol", addr: "user_at_example.com", want: false},
		{name: "leading_at", addr: "@example.com", want: false},
		{name: "trailing_at", addr: "user@", want: false},
		{name: "multiple_at_symbols", addr: "user@domain@extra.com", want: false},
		{name: "space_in_local", addr: "us er@example.com", want: false},
		{name: "tab_in_local", addr: "us\ter@example.com", want: false},
		{name: "cr_in_local", addr: "us\rer@example.com", want: false},
		{name: "lf_in_local", addr: "us\ner@example.com", want: false},
		{name: "angle_bracket_in_local", addr: "us<er>@example.com", want: false},
		{name: "semicolon_in_local", addr: "us;er@example.com", want: false},
		{name: "comma_in_local", addr: "us,er@example.com", want: false},
		{name: "colon_in_local", addr: "us:er@example.com", want: false},
		{name: "backslash_in_local", addr: "us\\er@example.com", want: false},
		{name: "quote_in_local", addr: "us\"er@example.com", want: false},
		{name: "bracket_in_local", addr: "us[er]@example.com", want: false},
		{name: "parenthesis_in_local", addr: "us(er)@example.com", want: false},
		{name: "no_dot_in_domain", addr: "user@localhost", want: false},
		{name: "space_in_domain", addr: "user@exam ple.com", want: false},
		{name: "tab_in_domain", addr: "user@exam\tple.com", want: false},
		{name: "cr_in_domain", addr: "user@exam\rple.com", want: false},
		{name: "lf_in_domain", addr: "user@exam\nple.com", want: false},
		{name: "null_byte_in_domain", addr: "user@exam\x00ple.com", want: false},
		{name: "angle_in_domain", addr: "user@<example.com>", want: false},
		{name: "valid_standard", addr: "alice@example.com", want: true},
		{name: "valid_subdomain", addr: "bob.smith+tag@sub.domain.org", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validRFC2822Addr(tc.addr); got != tc.want {
				t.Errorf("validRFC2822Addr(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestGmailResiduals_PathSafeID exercises pathSafeID 256-char cap and full reject class (~408-420).
func TestGmailResiduals_PathSafeID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "empty", id: "", want: false},
		{name: "exact_256_chars_valid", id: strings.Repeat("a", 256), want: true},
		{name: "exceeds_256_chars", id: strings.Repeat("a", 257), want: false},
		{name: "reject_slash", id: "msg/123", want: false},
		{name: "reject_backslash", id: "msg\\123", want: false},
		{name: "reject_question", id: "msg?123", want: false},
		{name: "reject_hash", id: "msg#123", want: false},
		{name: "reject_ampersand", id: "msg&123", want: false},
		{name: "reject_percent", id: "msg%20123", want: false},
		{name: "reject_space", id: "msg 123", want: false},
		{name: "reject_tab", id: "msg\t123", want: false},
		{name: "reject_cr", id: "msg\r123", want: false},
		{name: "reject_lf", id: "msg\n123", want: false},
		{name: "valid_simple", id: "18db4a9f", want: true},
		{name: "valid_alphanumeric_dash_underscore", id: "msg-id_ABC.123", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathSafeID(tc.id); got != tc.want {
				t.Errorf("pathSafeID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestGmailResiduals_DecodeBodyBranches exercises multipart / nested-parts / body-fallback
// decode branches (~446-473).
func TestGmailResiduals_DecodeBodyBranches(t *testing.T) {
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}

	tests := []struct {
		name    string
		payload *gmailPayload
		want    string
	}{
		{
			name:    "nil_payload",
			payload: nil,
			want:    "",
		},
		{
			name: "top_level_text_plain",
			payload: &gmailPayload{
				MimeType: "text/plain",
				Body:     gmailData{Data: enc("hello from top plain")},
			},
			want: "hello from top plain",
		},
		{
			name: "multipart_alternative_with_parts",
			payload: &gmailPayload{
				MimeType: "multipart/alternative",
				Parts: []gmailPart{
					{MimeType: "text/html", Body: gmailData{Data: enc("<p>html</p>")}},
					{MimeType: "text/plain", Body: gmailData{Data: enc("plain part body")}},
				},
			},
			want: "plain part body",
		},
		{
			name: "multipart_mixed_with_nested_parts",
			payload: &gmailPayload{
				MimeType: "multipart/mixed",
				Parts: []gmailPart{
					{
						MimeType: "multipart/alternative",
						Parts: []gmailPart{
							{MimeType: "text/plain", Body: gmailData{Data: enc("nested plain body")}},
						},
					},
				},
			},
			want: "nested plain body",
		},
		{
			name: "multipart_fallback_to_top_body",
			payload: &gmailPayload{
				MimeType: "multipart/mixed",
				Body:     gmailData{Data: enc("fallback top body")},
				Parts: []gmailPart{
					{MimeType: "application/pdf", Body: gmailData{Data: enc("binary data")}},
				},
			},
			want: "fallback top body",
		},
		{
			name: "multipart_no_text_plain_and_empty_top_body",
			payload: &gmailPayload{
				MimeType: "multipart/mixed",
				Parts: []gmailPart{
					{MimeType: "application/octet-stream", Body: gmailData{Data: enc("binary")}},
				},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeGmailBody(tc.payload)
			if got != tc.want {
				t.Errorf("decodeGmailBody() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGmailResiduals_DecodeBase64URL exercises raw-vs-padded fallback and garbage->"" (~477-484).
func TestGmailResiduals_DecodeBase64URL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "padded_base64url",
			input: base64.URLEncoding.EncodeToString([]byte("padded payload")),
			want:  "padded payload",
		},
		{
			name:  "raw_unpadded_base64url",
			input: base64.RawURLEncoding.EncodeToString([]byte("unpadded raw payload")),
			want:  "unpadded raw payload",
		},
		{
			name:  "garbage_string_yields_empty",
			input: "!!this is completely invalid base64url data!!",
			want:  "",
		},
		{
			name:  "corrupt_characters",
			input: "#@$%^&*()",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeBase64URL(tc.input)
			if got != tc.want {
				t.Errorf("decodeBase64URL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestGmailResiduals_ValidateArgs exercises ValidateArgs unknown-tool and schema branches (~102-130).
func TestGmailResiduals_ValidateArgs(t *testing.T) {
	g := NewGmailConnector(nil)

	tests := []struct {
		name    string
		tool    string
		args    string
		wantErr bool
	}{
		{
			name:    "unknown_tool_returns_error",
			tool:    "unknown_tool",
			args:    `{}`,
			wantErr: true,
		},
		{
			name:    "send_invalid_json",
			tool:    "send",
			args:    `not-json`,
			wantErr: true,
		},
		{
			name:    "send_missing_to",
			tool:    "send",
			args:    `{"to":[],"subject":"s","body":"b"}`,
			wantErr: true,
		},
		{
			name:    "send_bad_recipient",
			tool:    "send",
			args:    `{"to":["bad recipient address"],"subject":"s","body":"b"}`,
			wantErr: true,
		},
		{
			name:    "send_valid_recipient",
			tool:    "send",
			args:    `{"to":["alice@example.com"],"subject":"s","body":"b"}`,
			wantErr: false,
		},
		{
			name:    "read_invalid_json",
			tool:    "read",
			args:    `not-json`,
			wantErr: true,
		},
		{
			name:    "read_empty_id",
			tool:    "read",
			args:    `{"id":""}`,
			wantErr: true,
		},
		{
			name:    "read_unsafe_id",
			tool:    "read",
			args:    `{"id":"../../traversal"}`,
			wantErr: true,
		},
		{
			name:    "read_valid_id",
			tool:    "read",
			args:    `{"id":"msg-id-12345"}`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := g.ValidateArgs(tc.tool, json.RawMessage(tc.args))
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateArgs(%q, %s) err = %v, wantErr = %v", tc.tool, tc.args, err, tc.wantErr)
			}
		})
	}
}

type errorRoundTripper struct {
	err error
}

func (e errorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, e.err
}

// TestGmailResiduals_ConnectorEdgeBranches exercises connector helpers like
// Host, apiURL without override, ReviewFields fallback, and Execute bad_args.
func TestGmailResiduals_ConnectorEdgeBranches(t *testing.T) {
	ctx := context.Background()
	g := NewGmailConnector(nil)

	// apiURL with no override defaults to https://gmail.googleapis.com
	g.SetBaseURLOverride("")
	urlStr := g.apiURL("/gmail/v1/users/me/messages/send")
	if urlStr != "https://gmail.googleapis.com/gmail/v1/users/me/messages/send" {
		t.Errorf("apiURL with no override = %q", urlStr)
	}

	// Host without override:
	if host := g.Host(); host != GmailAPIHost {
		t.Errorf("Host() = %q, want %q", host, GmailAPIHost)
	}

	// ReviewFields:
	if f := g.ReviewFields("read", json.RawMessage("bad-json")); f != "id=?" {
		t.Errorf("ReviewFields(read, bad) = %q", f)
	}
	if f := g.ReviewFields("send", json.RawMessage("bad-json")); f != "to=?, subject=?" {
		t.Errorf("ReviewFields(send, bad) = %q", f)
	}
	if f := g.ReviewFields("unknown", json.RawMessage("{}")); f != "?" {
		t.Errorf("ReviewFields(unknown) = %q", f)
	}

	// Execute with bad args / unknown tool:
	freshCred := Credential{AccessToken: "ya29.test", AccessExpiry: time.Now().Add(time.Hour).Format(time.RFC3339)}
	if _, err := g.Execute(ctx, "send", json.RawMessage("bad-json"), freshCred); err == nil {
		t.Error("expected bad_args error on send malformed json")
	}
	if _, err := g.Execute(ctx, "read", json.RawMessage("bad-json"), freshCred); err == nil {
		t.Error("expected bad_args error on read malformed json")
	}
	if _, err := g.Execute(ctx, "unknown", json.RawMessage("{}"), freshCred); err == nil {
		t.Error("expected unknown_tool error on unknown tool")
	}

	// do() with disallowed URL hits line 323:
	badReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://evil.com/test", nil)
	if _, _, err := g.do(badReq); err == nil {
		t.Error("expected error from do() on disallowed host")
	}

	// do() with client.Do error hits line 337:
	g.SetBaseURLOverride("http://127.0.0.1:8080")
	g.SetHTTPClient(&http.Client{Transport: errorRoundTripper{err: errors.New("simulated dial failure")}})
	loopReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080/test", nil)
	if _, _, err := g.do(loopReq); err == nil {
		t.Error("expected error from do() on roundtrip failure")
	}

	// execSend with do() error hits line 243:
	if _, err := g.execSend(ctx, &sendArgs{To: []string{"a@example.com"}}, freshCred); err == nil {
		t.Error("expected error from execSend on network failure")
	}

	// execRead with token acquisition failure hits line 268:
	expiredCred := Credential{AccessToken: "old", AccessExpiry: time.Now().Add(-time.Hour).Format(time.RFC3339)}
	if _, err := g.execRead(ctx, &readArgs{ID: "msg-1"}, expiredCred); err == nil {
		t.Error("expected error from execRead on expired token with nil vault")
	}

	// execRead with do() error hits line 280:
	if _, err := g.execRead(ctx, &readArgs{ID: "msg-1"}, freshCred); err == nil {
		t.Error("expected error from execRead on network failure")
	}
}

// TestWorkerSendResiduals exercises worker_send.go nil-vault and invalid code branches.
func TestWorkerSendResiduals(t *testing.T) {
	if ValidWorkerErrorCode("non_existent_error_code") {
		t.Error("ValidWorkerErrorCode should return false for invalid code")
	}

	ws := &WorkerServer{vault: nil}
	cw := &cellWorker{cellID: "cell-test"}
	// Nil vault audit operations must be safe no-ops:
	ws.auditAllowed(cw, "cred", "host", "tool")
	ws.auditDenied(cw, "cred", "host", "tool", "reason")
	if needles := ws.scrubNeedles(); needles != nil {
		t.Errorf("scrubNeedles on nil vault = %v, want nil", needles)
	}
	if out := ws.ScrubDetail("test-detail"); out != "test-detail" {
		t.Errorf("ScrubDetail on nil vault = %q, want 'test-detail'", out)
	}
}
