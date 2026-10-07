// OAuth2 credential kind, consent dance, and refresh per CUSTOS-SPEC §4.5.
// The consent listener is one-shot on 127.0.0.1:0; state is crypto/rand,
// held in memory only, and enforced on callback. Token endpoints are
// injectable seams (like the proxy dial/resolve seams) so V13 runs against
// a local fake OAuth server; production wiring refuses non-loopback http://
// token endpoints.

package custos

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Frozen production OAuth endpoints for Google/Gmail per CUSTOS §4.5, §12 Q2.
const (
	// GoogleAuthURI is the production consent endpoint (§4.5).
	GoogleAuthURI = "https://accounts.google.com/o/oauth2/v2/auth"
	// GoogleTokenURI is the production token endpoint (§4.5).
	GoogleTokenURI = "https://oauth2.googleapis.com/token" //nolint:gosec // public endpoint URI, not a credential

	// GmailSendScope is the required scope for the v1 gmail/send tool (§4.5).
	GmailSendScope = "https://www.googleapis.com/auth/gmail.send"

	// accessExpirySkew is the refresh-before-expiry margin (§4.5: expiry minus 60s).
	accessExpirySkew = 60 * time.Second

	// oauthStateBytes is 128 bits of crypto/rand state (§4.5: >=128-bit).
	oauthStateBytes = 16
)

// Sentinel errors for the OAuth login/refresh flow (§4.5, §5.1a mapping).
var (
	// ErrLoginStateMismatch is returned when the callback state does not
	// match the printed state: forged callback refused, audited login_denied.
	ErrLoginStateMismatch = errors.New("login refused: state mismatch")
	// ErrLoginNoState is returned when the callback carries no state param.
	ErrLoginNoState = errors.New("login refused: missing state")
	// ErrLoginDenied is returned when the callback carries an error param.
	ErrLoginDenied = errors.New("login refused: consent denied")
	// ErrLoginNoCode is returned when the callback carries no code param.
	ErrLoginNoCode = errors.New("login refused: no code")
	// ErrLoginTimeout is returned when no callback arrives in time.
	ErrLoginTimeout = errors.New("login timed out")
	// ErrTokenEndpointInsecure refuses non-loopback http:// token endpoints
	// in production wiring (V13 loopback fakes remain allowed).
	ErrTokenEndpointInsecure = errors.New("refuse: non-loopback http token endpoint")
	// ErrCredentialGone is returned when a refresh write-back finds the
	// credential revoked mid-forward (§4.5: write-back revalidates existence).
	ErrCredentialGone = errors.New("credential revoked during refresh")
	// ErrRefreshNotOAuth is returned when the credential is not kind oauth2.
	ErrRefreshNotOAuth = errors.New("credential is not oauth2")
	// ErrTokenExchange is returned when the token endpoint refuses.
	ErrTokenExchange = errors.New("token exchange failed")
	// ErrPasteNoCode is returned when the manual paste-back carries no code.
	ErrPasteNoCode = errors.New("paste-back contains no code")
)

// maxCredentialNames is the CA-1(e) name cap (64 names counting vault ∪
// keys.json). NOTE: this package enforces the vault-side count only; the
// keys.json side of the union is counted where keys.json is read (slice 1/2
// code has no keys.json accessor in this package), so the union cap is
// finalized at the hearthd resolution layer. Grammar refusals use the frozen
// ErrBadName and cap overflow the frozen ErrFull ("key store full"), per
// CA-1(e)/K3.
const maxCredentialNames = 64

// validateCredName enforces the CA-1(e) name grammar (K4, verbatim):
// ^[a-z0-9][a-z0-9_-]{0,63}$ — enforced via the shared keyNameRe so the
// worker lane can never store a name the policy engine would refuse.
func validateCredName(name string) error {
	if !keyNameRe.MatchString(name) {
		return ErrBadName
	}
	return nil
}

// tokenResponse is the token-endpoint JSON reply shape (§4.5 exchange/refresh).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// TokenExchangeResult is the result of a code exchange or refresh forward.
type TokenExchangeResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Scope        string
}

// TokenForwardFunc forwards a form-encoded request to the token endpoint and
// decodes the JSON reply. This is the injectable seam for V13 fake servers
// (mirrors the proxy dial/resolve seam style).
type TokenForwardFunc func(ctx context.Context, tokenURI string, form url.Values) (*TokenExchangeResult, error)

// isDeadlineErr reports deadline/context expiry anywhere in the chain (§5.1a).
func isDeadlineErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// classifyTokenTransport maps a transport failure from the token forward to
// the §5.1a closed classes: deadline → timeout; everything before a response
// (DNS, refused, no status) → transport.
func classifyTokenTransport(err error) error {
	if isDeadlineErr(err) {
		return fmt.Errorf("%w: timeout", ErrTokenExchange)
	}
	return fmt.Errorf("%w: transport", ErrTokenExchange)
}

// tokenStatusError carries a token-endpoint non-2xx status class. §5.1a:
// HTTP upstream failures contribute a status class only — never the URL,
// never the upstream body.
type tokenStatusError struct{ status int }

func (e *tokenStatusError) Error() string {
	return fmt.Sprintf("%s: status %d", ErrTokenExchange.Error(), e.status)
}

// Unwrap keeps ErrTokenExchange membership for callers that match on it.
func (e *tokenStatusError) Unwrap() error { return ErrTokenExchange }

// DefaultTokenForward performs the token-endpoint POST. Non-loopback http://
// endpoints are refused (production wiring); loopback http:// is allowed so
// V13 runs against a local fake OAuth server.
func DefaultTokenForward(ctx context.Context, tokenURI string, form url.Values) (*TokenExchangeResult, error) {
	if err := assertSafeTokenURI(tokenURI); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, classifyTokenTransport(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyTokenTransport(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Status class only; the upstream body never surfaces (§5.1a).
		return nil, &tokenStatusError{status: resp.StatusCode}
	}

	var tr tokenResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&tr); err != nil {
		return nil, fmt.Errorf("%w: bad response", ErrTokenExchange)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("%w: no access token", ErrTokenExchange)
	}
	refresh := tr.RefreshToken
	if refresh == "" {
		// Rotating providers echo the new refresh token; code-exchange
		// responses keep the submitted one when none is returned.
		refresh = form.Get("refresh_token")
	}
	return &TokenExchangeResult{
		AccessToken:  tr.AccessToken,
		RefreshToken: refresh,
		ExpiresIn:    tr.ExpiresIn,
		Scope:        tr.Scope,
	}, nil
}

// assertSafeTokenURI refuses non-loopback http:// token endpoints.
func assertSafeTokenURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return ErrTokenEndpointInsecure
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return ErrTokenEndpointInsecure
		}
		return nil
	default:
		return ErrTokenEndpointInsecure
	}
}

// GenerateOAuthState mints a URL-safe state parameter of >=128 bits (§4.5).
func GenerateOAuthState() (string, error) {
	b := make([]byte, oauthStateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BuildConsentURL renders the consent URL with state and the redirect_uri
// (§4.5: state param present in the printed URL).
func BuildConsentURL(authURI, clientID, redirectURI string, scopes []string, state string) (string, error) {
	u, err := url.Parse(authURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// manualRedirectURI is the unreachable loopback redirect used in --manual
// mode: the browser lands on a dead socket and the operator copies the
// redirect URL out of the address bar (§4.5 headless path).
const manualRedirectURI = "http://127.0.0.1:1/"

// ParseCodeFromPasteback extracts (code, state) from the manual paste-back:
// a full redirect URL (state carried and validated by the caller) or a bare
// code (§4.5: bare code accepted only in manual mode).
func ParseCodeFromPasteback(pasted string) (code, state string, isURL bool, err error) {
	s := strings.TrimSpace(pasted)
	if s == "" {
		return "", "", false, ErrPasteNoCode
	}
	if strings.Contains(s, "://") {
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", true, ErrPasteNoCode
		}
		q := u.Query()
		if e := q.Get("error"); e != "" {
			return "", q.Get("state"), true, ErrLoginDenied
		}
		c := q.Get("code")
		if c == "" {
			return "", q.Get("state"), true, ErrPasteNoCode
		}
		return c, q.Get("state"), true, nil
	}
	// Bare code: no whitespace, no '=' (a malformed URL fragment).
	if strings.ContainsAny(s, " \t\r\n=") {
		return "", "", false, ErrPasteNoCode
	}
	return s, "", false, nil
}

// LoginOptions parameterizes one `custos login` run per §4.5, §11.
type LoginOptions struct {
	Connector    string                 // "gmail" (v1)
	CredName     string                 // vault entry name; defaults to Connector
	ClientID     string                 // required
	ClientSecret string                 // prompted on the TTY, never a flag/argv (§4.5)
	Scopes       []string               // required scopes for the connector's tools
	AuthURI      string                 // defaults to GoogleAuthURI
	TokenURI     string                 // defaults to GoogleTokenURI
	Manual       bool                   // --manual paste-back mode (headless hosts)
	State        string                 // pre-minted state (memory only); empty generates
	Timeout      time.Duration          // consent wait budget (default 5 min)
	Forward      TokenForwardFunc       // injectable token-endpoint seam (V13)
	Out          io.Writer              // where the consent URL is printed
	PasteBack    func() (string, error) // manual-mode paste reader (TTY prompt in CLI)
}

// LoginResult carries the exchanged grant for vault storage plus the state
// that was enforced (§4.5).
type LoginResult struct {
	Name       string // vault entry name the grant should be stored under
	Credential Credential
	State      string
	Redirect   string
}

// oauthConnectorDefaults pins the per-connector scope set (v1: gmail, §4.5).
func oauthConnectorDefaults(connector string) (scopes []string, cred string, err error) {
	switch connector {
	case "gmail":
		return []string{GmailSendScope}, "gmail", nil
	default:
		return nil, "", fmt.Errorf("unknown connector %q", connector)
	}
}

// OAuthLogin runs the consent dance and token exchange per §4.5 and returns
// the credential material. It performs NO vault write: callers route the
// result through the §4.2 Mutate path so snapshots/audit stay conformant.
func OAuthLogin(ctx context.Context, opts *LoginOptions) (*LoginResult, error) {
	if opts == nil {
		return nil, errors.New("nil login options")
	}
	if opts.ClientID == "" {
		return nil, errors.New("client id required")
	}
	scopes, defaultCred, err := oauthConnectorDefaults(opts.Connector)
	if err != nil {
		return nil, err
	}
	credName := defaultCred
	if opts.CredName != "" {
		credName = opts.CredName
	}
	if len(opts.Scopes) > 0 {
		scopes = opts.Scopes
	}
	authURI := opts.AuthURI
	if authURI == "" {
		authURI = GoogleAuthURI
	}
	tokenURI := opts.TokenURI
	if tokenURI == "" {
		tokenURI = GoogleTokenURI
	}
	forward := opts.Forward
	if forward == nil {
		forward = DefaultTokenForward
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	state := opts.State
	if state == "" {
		state, err = GenerateOAuthState()
		if err != nil {
			return nil, err
		}
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	var code, redirectURI string
	if opts.Manual {
		redirectURI = manualRedirectURI
		consentURL, berr := BuildConsentURL(authURI, opts.ClientID, redirectURI, scopes, state)
		if berr != nil {
			return nil, berr
		}
		// Headless path: print the URL; the operator completes consent
		// anywhere and pastes the redirect URL (or bare code) back (§4.5).
		fmt.Fprintf(out, "Open this URL to authorize:\n%s\n", consentURL)
		fmt.Fprintf(out, "state: %s\n", state)
		if opts.PasteBack == nil {
			return nil, errors.New("manual mode requires a paste-back reader")
		}
		pasted, perr := opts.PasteBack()
		if perr != nil {
			return nil, perr
		}
		pasteCode, pasteState, isURL, cerr := ParseCodeFromPasteback(pasted)
		if cerr != nil {
			return nil, cerr
		}
		// §4.5: 'state is still validated against the printed value' —
		// this binds the URL-form paste-back. A bare code carries no
		// state field at all (documented limit of §4.5's bare-code form):
		// nothing exists to validate against there.
		if isURL && pasteState != state {
			return nil, ErrLoginStateMismatch
		}
		code = pasteCode
	} else {
		code, redirectURI, err = interactiveConsent(ctx, opts, out, authURI, scopes, state, timeout)
		if err != nil {
			return nil, err
		}
	}

	res, err := forward(ctx, tokenURI, url.Values{
		"code":          {code},
		"client_id":     {opts.ClientID},
		"client_secret": {opts.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	cred := Credential{
		Kind:         "oauth2",
		ClientID:     opts.ClientID,
		ClientSecret: opts.ClientSecret,
		RefreshToken: res.RefreshToken,
		AccessToken:  res.AccessToken,
		Scopes:       strings.Join(scopes, " "),
		TokenURI:     tokenURI,
		AuthURI:      authURI,
		GrantedAt:    now.Format(time.RFC3339),
	}
	if res.ExpiresIn > 0 {
		cred.AccessExpiry = now.Add(time.Duration(res.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	return &LoginResult{Name: credName, Credential: cred, State: state, Redirect: redirectURI}, nil
}

// oauthCallback is one captured consent redirect.
type oauthCallback struct {
	code     string
	state    string
	errParam string
}

// interactiveConsent binds the one-shot 127.0.0.1:0 listener, prints the
// consent URL, and returns the authorization code (§4.5). The listener is
// asserted loopback with a non-zero :0-assigned port; the first callback
// consumes it, then it closes — later attempts get connection-refused.
func interactiveConsent(ctx context.Context, opts *LoginOptions, out io.Writer, authURI string, scopes []string, state string, timeout time.Duration) (code, redirectURI string, err error) {
	lc := net.ListenConfig{}
	ln, lerr := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if lerr != nil {
		return "", "", lerr
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() || tcpAddr.Port == 0 {
		_ = ln.Close()
		return "", "", errors.New("consent listener must bind loopback with a non-zero port")
	}
	redirectURI = fmt.Sprintf("http://127.0.0.1:%d/", tcpAddr.Port)

	consentURL, berr := BuildConsentURL(authURI, opts.ClientID, redirectURI, scopes, state)
	if berr != nil {
		_ = ln.Close()
		return "", "", berr
	}
	fmt.Fprintf(out, "Open this URL to authorize:\n%s\n", consentURL)

	cbCh := make(chan oauthCallback, 1)
	var once sync.Once
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			// Flush the browser-visible reply BEFORE handing the callback
			// to the consumer: handing it over first lets the deferred
			// srv.Close() race the response write, and the browser (or a
			// test GET) can observe EOF instead of the completion page.
			// One-shot §4.5 semantics hold — only the first callback
			// reaches cbCh (buffered, sync.Once), and the listener is
			// closed right after.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("lararium custos: login complete, you may close this tab\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			once.Do(func() {
				cbCh <- oauthCallback{code: q.Get("code"), state: q.Get("state"), errParam: q.Get("error")}
			})
		}),
	}
	go func() { _ = srv.Serve(ln) }()

	defer func() {
		// One-shot: shut the listener down after the first callback is
		// consumed OR on failure; later attempts see connection-refused.
		_ = srv.Close()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case cb := <-cbCh:
		switch {
		case cb.errParam != "":
			return "", "", ErrLoginDenied
		case cb.state == "":
			return "", "", ErrLoginNoState
		case subtle.ConstantTimeCompare([]byte(cb.state), []byte(state)) != 1:
			return "", "", ErrLoginStateMismatch
		case cb.code == "":
			return "", "", ErrLoginNoCode
		}
		return cb.code, redirectURI, nil
	case <-timer.C:
		return "", "", ErrLoginTimeout
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// LoginDeniedReason maps a login error to the audit reason string for the
// login_denied record (§4.5, §8.1); "" means the error is not a denial.
func LoginDeniedReason(err error) string {
	switch {
	case errors.Is(err, ErrLoginStateMismatch):
		return "state_mismatch"
	case errors.Is(err, ErrLoginNoState):
		return "missing_state"
	case errors.Is(err, ErrLoginDenied):
		return "consent_denied"
	case errors.Is(err, ErrLoginNoCode):
		return "no_code"
	case errors.Is(err, ErrLoginTimeout):
		return "timeout"
	default:
		return ""
	}
}

// StoreOAuthGrant stores kind `oauth2` credential material through the §4.2
// Mutate path (snapshotted, intent-paired) and audits credential_added.
// CA-1(e): the name must match the inherited K4 grammar and the vault-side
// count of the 64-name cap (the keys.json side of the union is counted at
// the resolution layer — see maxCredentialNames). Overwriting an existing
// name does not grow the set, so it never trips the cap.
func (v *Vault) StoreOAuthGrant(passphrase, credName string, cred Credential, actor string) error {
	if err := validateCredName(credName); err != nil {
		return err
	}
	cred.Kind = "oauth2"
	err := v.Mutate(passphrase, func(doc *VaultDoc) ([]string, error) {
		if doc.Credentials == nil {
			doc.Credentials = make(map[string]Credential)
		}
		if _, exists := doc.Credentials[credName]; !exists && len(doc.Credentials) >= maxCredentialNames {
			return nil, ErrFull
		}
		doc.Credentials[credName] = cred
		return []string{credName}, nil
	}, false)
	if err != nil {
		return err
	}
	_ = v.audit.Append(AuditRecord{
		Kind:  AuditKindCredentialAdded,
		Cred:  credName,
		Actor: actor,
	})
	return nil
}

// refreshCall is one in-flight refresh that concurrent callers coalesce onto
// (§4.5: concurrent refreshes of the same credential coalesce).
type refreshCall struct {
	done  chan struct{}
	token string
	err   error
}

// credentialFresh reports whether the access token is usable: expiry minus
// 60s skew has not passed (§4.5).
func credentialFresh(cred Credential, now time.Time) bool {
	if cred.AccessToken == "" {
		return false
	}
	if cred.AccessExpiry == "" {
		return true
	}
	exp, err := time.Parse(time.RFC3339, cred.AccessExpiry)
	if err != nil {
		return false
	}
	return now.Before(exp.Add(-accessExpirySkew))
}

// AcquireToken returns a fresh access token for an oauth2 credential per
// CUSTOS §4.5: per-credential refresh lock with single-flight coalescing;
// the token-endpoint forward runs OUTSIDE the custos.lock mutation flock;
// the brief flock write-back revalidates existence (a revoke committed
// mid-forward drops the result). Access-token-only rotation is an ephemeral
// vault write (no snapshot, §8.4).
func (v *Vault) AcquireToken(credName string) (string, error) {
	return v.AcquireTokenCtx(context.Background(), credName)
}

// AcquireTokenCtx is AcquireToken with caller-cancellation propagation into
// the wait for the refresh; the shared forward itself runs detached
// (context.WithoutCancel) so one coalesced caller cancelling never kills the
// refresh for the others. The refresh write stays detached either way.
func (v *Vault) AcquireTokenCtx(ctx context.Context, credName string) (string, error) {
	cred, ok := v.GetCredential(credName)
	if !ok {
		if !v.IsUnlocked() {
			return "", ErrCustosLocked
		}
		return "", fmt.Errorf("credential %q not found", credName)
	}
	if cred.Kind != "oauth2" || cred.RefreshToken == "" {
		return "", ErrRefreshNotOAuth
	}
	if credentialFresh(cred, time.Now().UTC()) {
		return cred.AccessToken, nil
	}

	v.mu.Lock()
	if v.refreshInflight == nil {
		v.refreshInflight = make(map[string]*refreshCall)
	}
	if call, inflight := v.refreshInflight[credName]; inflight {
		v.mu.Unlock()
		<-call.done
		return call.token, call.err
	}
	call := &refreshCall{done: make(chan struct{})}
	v.refreshInflight[credName] = call
	v.mu.Unlock()

	go func() {
		// The shared forward must survive any individual caller's cancel:
		// coalesced callers wait on this one refresh, so it runs detached
		// from the first caller's context (the 30s cap inside
		// performRefresh bounds it; §4.5 coalescing doctrine).
		token, err := v.performRefresh(context.WithoutCancel(ctx), credName, cred)
		call.token, call.err = token, err
		v.mu.Lock()
		delete(v.refreshInflight, credName)
		v.mu.Unlock()
		close(call.done)
	}()

	<-call.done
	return call.token, call.err
}

// performRefresh forwards the refresh grant outside the flock, then takes a
// brief flock (via MutateWithRegistry) to encrypt + write, revalidating that
// the credential still exists before committing.
func (v *Vault) performRefresh(ctx context.Context, credName string, cred Credential) (string, error) {
	tokenURI := cred.TokenURI
	if tokenURI == "" {
		tokenURI = GoogleTokenURI
	}
	v.mu.RLock()
	forward := v.tokenForward
	v.mu.RUnlock()
	if forward == nil {
		forward = DefaultTokenForward
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := forward(ctx, tokenURI, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
		"client_id":     {cred.ClientID},
		"client_secret": {cred.ClientSecret},
	})
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	refreshRotated := res.RefreshToken != "" && res.RefreshToken != cred.RefreshToken

	var droppedRevoked bool
	mErr := v.MutateWithRegistry("", func(doc *VaultDoc, _ *SurrogateDoc) ([]string, []AuditRecord, error) {
		// §4.5: write-back revalidates existence under the flock. A revoke
		// committed during the forward must never resurrect the credential,
		// and the drop itself is audited credential_rotated (dropped:
		// revoked) through the same Mutate path.
		cur, exists := doc.Credentials[credName]
		if !exists {
			droppedRevoked = true
			// Revoke committed mid-forward: the refresh result is dropped.
			return nil, nil, ErrCredentialGone
		}
		cur.AccessToken = res.AccessToken
		if res.ExpiresIn > 0 {
			cur.AccessExpiry = now.Add(time.Duration(res.ExpiresIn) * time.Second).Format(time.RFC3339)
		}
		if refreshRotated {
			cur.RefreshToken = res.RefreshToken
		}
		doc.Credentials[credName] = cur
		reason := "ephemeral: true"
		if refreshRotated {
			reason = "refresh_token rotated"
		}
		return []string{credName}, []AuditRecord{{
			Kind:   AuditKindCredentialRotated,
			Cred:   credName,
			Actor:  "worker",
			Reason: reason,
		}}, nil
	}, !refreshRotated) // access-token-only rotation = ephemeral (§4.5, §8.4)
	if mErr != nil {
		if droppedRevoked {
			// §4.5: the drop itself is audited credential_rotated with
			// reason "dropped: revoked" (the Mutate error path writes no
			// records, so this append carries it).
			_ = v.audit.Append(AuditRecord{
				Kind:   AuditKindCredentialRotated,
				Cred:   credName,
				Actor:  "worker",
				Reason: "dropped: revoked",
			})
			return "", ErrCredentialGone
		}
		return "", mErr
	}
	return res.AccessToken, nil
}
