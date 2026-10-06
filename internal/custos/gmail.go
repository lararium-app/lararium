// v1 Gmail worker connector per CUSTOS-SPEC §5.1: typed tools `send` and
// `read` over the Gmail REST host set (*.googleapis.com validated per §6.3;
// loopback http allowed only so V13 runs against a local fake). The
// connector holds no plaintext beyond the moment of the HTTPS request and
// returns typed results only — never raw upstream bytes (§5.1).

package custos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gmail REST endpoint constants (production defaults; §12 Q2).
const (
	// GmailAPIHost is the Gmail REST host (host set *.googleapis.com, §6.3).
	GmailAPIHost = "gmail.googleapis.com"
	// gmailSendPath is the send-message endpoint (v1 tool `send`).
	gmailSendPath = "/gmail/v1/users/me/messages/send"
	// gmailMaxResponseBytes caps upstream response reads (2 MiB).
	gmailMaxResponseBytes = 2 << 20
)

// GmailConnector implements the v1 worker Connector for Gmail (§5.1).
type GmailConnector struct {
	vault *Vault
	// baseURL overrides the REST origin (loopback fake for V13 only).
	baseURL string
	client  *http.Client
}

// NewGmailConnector builds the connector; production uses default TLS.
func NewGmailConnector(vault *Vault) *GmailConnector {
	return &GmailConnector{vault: vault}
}

// SetBaseURLOverride points the connector at a local fake REST server
// (V13 seam; production wiring never calls it).
func (g *GmailConnector) SetBaseURLOverride(raw string) {
	g.baseURL = raw
}

// SetHTTPClient injects a client for the fake-upstream seam.
func (g *GmailConnector) SetHTTPClient(c *http.Client) {
	g.client = c
}

// Name implements Connector.
func (g *GmailConnector) Name() string { return "gmail" }

// Credential implements Connector: the v1 gmail connector uses the vault
// credential named "gmail" (§4.5).
func (g *GmailConnector) Credential() string { return "gmail" }

// Host implements Connector.
func (g *GmailConnector) Host() string {
	if g.baseURL != "" {
		if u, err := url.Parse(g.baseURL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return GmailAPIHost
}

// HasTool implements Connector.
func (g *GmailConnector) HasTool(name string) bool {
	return name == "send" || name == "read"
}

// sendArgs is the typed `send` tool schema: to []string, subject, body (§5.1).
type sendArgs struct {
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
}

// readArgs is the typed `read` tool schema: id (§5.1).
type readArgs struct {
	ID string `json:"id"`
}

// ValidateArgs implements the tool argument schemas (bad shape ⇒ bad_args).
func (g *GmailConnector) ValidateArgs(tool string, args json.RawMessage) error {
	switch tool {
	case "send":
		var a sendArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return err
		}
		if len(a.To) == 0 {
			return fmt.Errorf("to required")
		}
		for _, t := range a.To {
			if !validRFC2822Addr(t) {
				return fmt.Errorf("bad recipient")
			}
		}
		return nil
	case "read":
		var a readArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return err
		}
		if a.ID == "" || !pathSafeID(a.ID) {
			return fmt.Errorf("bad id")
		}
		return nil
	default:
		return fmt.Errorf("unknown tool")
	}
}

// ReviewFields implements §7 card review fields declared by the tool
// schema: gmail/send declares `to` and `subject`.
func (g *GmailConnector) ReviewFields(tool string, args json.RawMessage) string {
	switch tool {
	case "send":
		var a sendArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return "to=?, subject=?"
		}
		return fmt.Sprintf("to=%s, subject=%s", strings.Join(a.To, ","), a.Subject)
	case "read":
		var a readArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return "id=?"
		}
		return "id=" + a.ID
	default:
		return "?"
	}
}

// Execute runs one typed tool. Secrets live only in the Authorization header
// of the single outbound request; the typed result carries no upstream bytes
// beyond the fields declared per §5.1.
func (g *GmailConnector) Execute(ctx context.Context, tool string, args json.RawMessage, cred Credential) (json.RawMessage, error) {
	switch tool {
	case "send":
		var a sendArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, wErr(ErrCodeBadArgs, detailBadArgs)
		}
		return g.execSend(ctx, &a, cred)
	case "read":
		var a readArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, wErr(ErrCodeBadArgs, detailBadArgs)
		}
		return g.execRead(ctx, &a, cred)
	default:
		return nil, wErr(ErrCodeUnknownTool, detailUnknownTool)
	}
}

// accessToken resolves a fresh oauth2 access token via the vault refresh
// path (§4.5 AcquireToken); connector keeps it only for one request.
func (g *GmailConnector) accessToken(ctx context.Context, cred Credential) (string, error) {
	// Fresh token fast path (§4.5 skew).
	if credentialFresh(cred, time.Now().UTC()) {
		return cred.AccessToken, nil
	}
	if g.vault == nil {
		return "", wErr(ErrCodeInternal, detailInternal)
	}
	tok, err := g.vault.AcquireTokenCtx(ctx, g.Credential())
	if err != nil {
		if errors.Is(err, ErrCredentialGone) {
			return "", wErr(ErrCodeDenied, detailDeniedCredGone)
		}
		if errors.Is(err, ErrCustosLocked) {
			return "", wErr(ErrCodeLocked, detailLocked)
		}
		return "", classifyWorkerError(err)
	}
	return tok, nil
}

// assertURLHost enforces the §6.3 host set: *.googleapis.com over https, or
// loopback http for the injected fake (V13 seam).
func (g *GmailConnector) assertURLHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return wErr(ErrCodeTransport, detailTransport)
	}
	host := u.Hostname()
	if u.Scheme == "https" && (host == GmailAPIHost || strings.HasSuffix(host, ".googleapis.com")) {
		return nil
	}
	if g.baseURL != "" && u.Scheme == "http" {
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return wErr(ErrCodeTransport, detailTransport)
}

// execSend builds an RFC 2822 message, base64url-encodes it, and POSTs to
// the send endpoint (§5.1). Typed result: {"id": ...}.
func (g *GmailConnector) execSend(ctx context.Context, a *sendArgs, cred Credential) (json.RawMessage, error) {
	token, err := g.accessToken(ctx, cred)
	if err != nil {
		return nil, err
	}

	raw, err := buildRFC2822(a)
	if err != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}

	payload, perr := json.Marshal(map[string]string{"raw": raw})
	if perr != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.apiURL(gmailSendPath), bytes.NewReader(payload))
	if err != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}
	g.decorate(req, token)

	respBody, status, err := g.do(req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, upstreamStatusError(status)
	}

	// Typed result only (§5.1): message id.
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.ID == "" {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}
	out, mErr := json.Marshal(map[string]string{"id": parsed.ID})
	if mErr != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}
	return out, nil
}

// execRead GETs the message format=full, strips labelIds, and returns the
// typed {id, threadId, snippet, body} shape (§5.1). No labels, no raw parts.
func (g *GmailConnector) execRead(ctx context.Context, a *readArgs, cred Credential) (json.RawMessage, error) {
	token, err := g.accessToken(ctx, cred)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		g.apiURL("/gmail/v1/users/me/messages/"+url.PathEscape(a.ID)+"?format=full"), nil)
	if err != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}
	g.decorate(req, token)

	respBody, status, err := g.do(req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, upstreamStatusError(status)
	}

	var msg gmailMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}

	body := decodeGmailBody(msg.Payload)
	out, mErr := json.Marshal(map[string]string{
		"id":       msg.ID,
		"threadId": msg.ThreadID,
		"snippet":  msg.Snippet,
		"body":     body,
	})
	if mErr != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}
	return out, nil
}

// apiURL joins the override base or the production https origin.
func (g *GmailConnector) apiURL(path string) string {
	if g.baseURL != "" {
		return strings.TrimRight(g.baseURL, "/") + path
	}
	return "https://" + GmailAPIHost + path
}

// decorate sets the auth and content headers for a Gmail REST call.
func (g *GmailConnector) decorate(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}

// do performs one request with the response cap; every transport failure is
// classified to the §5.1a closed set before it can surface.
func (g *GmailConnector) do(req *http.Request) ([]byte, int, error) {
	if err := g.assertURLHost(req.URL.String()); err != nil {
		return nil, 0, err
	}
	client := g.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		we := classifyWorkerError(err)
		return nil, 0, we
	}
	defer func() { _ = resp.Body.Close() }()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, gmailMaxResponseBytes))
	if rerr != nil {
		// Mid-body transport failure: connection failed to deliver a
		// complete response; classified transport per §5.1a.
		return nil, 0, wErr(ErrCodeTransport, detailTransport)
	}
	return body, resp.StatusCode, nil
}

// upstreamStatusError maps an upstream status class to the §5.1a code with
// its frozen template (status class only, no body).
func upstreamStatusError(status int) *workerError {
	switch {
	case status == http.StatusTooManyRequests:
		return wErr(ErrCodeRateLimited, detailRateLimited)
	case status >= 500:
		return wErr(ErrCodeUpstream, detailUpstream5xx)
	default:
		return wErr(ErrCodeUpstream, detailUpstream4xx)
	}
}

// buildRFC2822 assembles the MIME message and base64url-encodes it (§5.1).
// The error is real: it carries the crypto/rand failure for the Message-ID.
func buildRFC2822(a *sendArgs) (string, error) {
	msgID, err := randMessageID()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("To: " + strings.Join(a.To, ", ") + "\r\n")
	b.WriteString("Subject: " + sanitizeHeaderLine(a.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Message-ID: <" + msgID + ">\r\n")
	b.WriteString("\r\n")
	b.WriteString(a.Body)

	enc := base64.RawURLEncoding.EncodeToString([]byte(b.String()))
	return enc, nil
}

// randMessageID mints a crypto/rand Message-ID local part.
func randMessageID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// sanitizeHeaderLine strips CR/LF from header values (no header injection).
func sanitizeHeaderLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// validRFC2822Addr is a pragmatic address check for typed schema validation:
// single local@domain with no spaces, control chars, or list/quote syntax.
func validRFC2822Addr(addr string) bool {
	if addr == "" || len(addr) > 254 {
		return false
	}
	at := strings.IndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 || strings.ContainsRune(addr[at+1:], '@') {
		return false
	}
	local, domain := addr[:at], addr[at+1:]
	if strings.ContainsAny(local, " 	\r\n<>@,;:\\\"[]()") {
		return false
	}
	return strings.Contains(domain, ".") && !strings.ContainsAny(domain, " 	\r\n<>@,;:\\\"[]()\x00")
}

// pathSafeID rejects traversal/path characters in message ids.
func pathSafeID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	return !strings.ContainsAny(id, "/\\?#&% \t\r\n")
}

// gmailMessage is the format=full REST reply the typed reader consumes.
type gmailMessage struct {
	ID       string        `json:"id"`
	ThreadID string        `json:"threadId"`
	Snippet  string        `json:"snippet"`
	Payload  *gmailPayload `json:"payload"`
}

// gmailPayload is the MIME payload of a full-format message.
type gmailPayload struct {
	MimeType string      `json:"mimeType"`
	Body     gmailData   `json:"body"`
	Parts    []gmailPart `json:"parts"`
}

// gmailPart is one MIME part of a payload.
type gmailPart struct {
	MimeType string      `json:"mimeType"`
	Body     gmailData   `json:"body"`
	Parts    []gmailPart `json:"parts"`
}

// gmailData carries base64url body data.
type gmailData struct {
	Data string `json:"data"`
}

// decodeGmailBody extracts plain text from a full-format payload: the top
// body when present, else the first text/plain part, base64url-decoded.
// labelIds never appear in the typed result (§5.1).
func decodeGmailBody(payload *gmailPayload) string {
	if payload == nil {
		return ""
	}
	if payload.MimeType == "text/plain" && payload.Body.Data != "" {
		return decodeBase64URL(payload.Body.Data)
	}
	if text := findTextPlain(payload.Parts); text != "" {
		return text
	}
	if payload.Body.Data != "" {
		return decodeBase64URL(payload.Body.Data)
	}
	return ""
}

// findTextPlain walks MIME parts depth-first for the first text/plain body.
func findTextPlain(parts []gmailPart) string {
	for _, p := range parts {
		if p.MimeType == "text/plain" && p.Body.Data != "" {
			return decodeBase64URL(p.Body.Data)
		}
		if nested := findTextPlain(p.Parts); nested != "" {
			return nested
		}
	}
	return ""
}

// decodeBase64URL decodes base64url data with or without padding; undecodable
// data yields "" (typed reader never echoes malformed upstream bytes).
func decodeBase64URL(s string) string {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return ""
}
