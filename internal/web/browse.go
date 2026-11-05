// Package web provides SSRF-safe web access for the rails agent:
// a static fetcher and a read-only headless browser, per
// docs/WEB-LAYER-SPEC.md.
package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/lararium-app/lararium/internal/loop"
)

const (
	// ErrBrowserUnavailable is returned when Chromium cannot be found or started.
	ErrBrowserUnavailable = "web_browse unavailable: no browser installed — install chromium or set web.chrome_path"
	// ErrBrowserBusy is returned when another browse task holds the mutex.
	ErrBrowserBusy = "browser is busy, try again in a moment"
)

var loginPathRe = regexp.MustCompile(`(?i)(?:^|/)(?:login|signin|sign-in|log-in)(?:/|$)`)

// Act is one browser action (click, fill, submit) requested alongside a browse goal.
type Act struct {
	Kind     string `json:"kind"`           // "click", "fill", or "submit"
	Selector string `json:"selector"`       // CSS selector
	Text     string `json:"text,omitempty"` // text to enter for fill
	Intent   string `json:"intent"`         // one-line human-readable intent (required)
}

// Browser drives headless Chromium for dynamic web pages.
type Browser struct {
	mu               sync.Mutex
	dataDir          string
	cfg              Config
	wallet           *Wallet
	index            *WalletIndex
	activity         *ActivityLogger
	approver         loop.Approver
	allowedTestHosts []string
}

// BrowseOption configures a Browser instance.
type BrowseOption func(*Browser)

// WithApprover attaches an approval hook to the browser.
func WithApprover(ap loop.Approver) BrowseOption {
	return func(b *Browser) {
		b.approver = ap
	}
}

// WithAllowedTestHosts exempts test servers from SSRF destination IP checks.
func WithAllowedTestHosts(hosts ...string) BrowseOption {
	return func(b *Browser) {
		b.allowedTestHosts = append(b.allowedTestHosts, hosts...)
	}
}

// WithWallet overrides the wallet instance (useful in testing).
func WithWallet(w *Wallet) BrowseOption {
	return func(b *Browser) {
		b.wallet = w
	}
}

// NewBrowser creates a new Browser instance.
func NewBrowser(dataDir string, cfg Config, opts ...BrowseOption) *Browser {
	b := &Browser{
		dataDir:  dataDir,
		cfg:      cfg,
		wallet:   NewWallet(dataDir, nil),
		index:    NewWalletIndex(dataDir),
		activity: NewActivityLogger(dataDir),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// SetApprover sets the approver callback for act approval gates.
func (b *Browser) SetApprover(ap loop.Approver) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.approver = ap
}

// Wallet returns the underlying session wallet.
func (b *Browser) Wallet() *Wallet {
	return b.wallet
}

// Index returns the wallet metadata index.
func (b *Browser) Index() *WalletIndex {
	return b.index
}

// Activity returns the activity logger.
func (b *Browser) Activity() *ActivityLogger {
	return b.activity
}

// SetAllowedTestHosts sets allowed test host names/addresses.
func (b *Browser) SetAllowedTestHosts(hosts ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.allowedTestHosts = hosts
}

// normalizeTestHost maps a configured test host to a bare lowercase
// hostname. Tests configure these as httptest server URLs (scheme and
// port included); everything else matches hostnames. Never configured
// outside tests.
func normalizeTestHost(h string) string {
	s := strings.TrimSpace(h)
	if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	return strings.ToLower(s)
}

// Browse executes navigation, optional acts, and extraction.
func (b *Browser) Browse(ctx context.Context, rawURL, goal string, acts ...Act) (string, error) {
	if !b.mu.TryLock() {
		return ErrBrowserBusy, nil
	}
	defer b.mu.Unlock()

	var allowedHost string
	reqHost := ""
	if u, err := url.Parse(rawURL); err == nil {
		reqHost = strings.ToLower(u.Hostname())
	}
	if reqHost != "" {
		for _, h := range b.allowedTestHosts {
			if normalizeTestHost(h) == reqHost {
				allowedHost = reqHost
				break
			}
		}
	}

	parsed, _, err := ValidateURLWithResolver(ctx, rawURL, net.DefaultResolver, allowedHost)
	if err != nil {
		return "", err
	}

	origin := parsed.Hostname()
	today := time.Now().UTC().Format("2006-01-02")

	// 1. Policy model + act gate (W-3 enforcement)
	if len(acts) > 0 {
		denial, blocked := b.enforceActsGate(ctx, origin, parsed.String(), today, acts)
		if blocked {
			return denial, nil
		}
	}

	// 2. Session wallet check (W-2)
	sessionActive, releaseFunc, userDataDir, err := b.prepareSession(origin)
	if err != nil {
		return "", err
	}
	if releaseFunc != nil {
		defer releaseFunc()
	}

	chromePath := b.cfg.ChromePath
	if chromePath == "" {
		chromePath = DetectChrome()
	}
	if chromePath == "" {
		return ErrBrowserUnavailable, nil
	}
	if _, err := exec.LookPath(chromePath); err != nil {
		return ErrBrowserUnavailable, nil
	}

	timeoutSec := b.cfg.BrowseTimeoutS
	if timeoutSec <= 0 {
		timeoutSec = DefaultBrowseTimeoutS
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)

	if userDataDir != "" {
		opts = append(opts, chromedp.UserDataDir(userDataDir))
	}

	if allowedHost == "" {
		opts = append(opts, chromedp.Flag("host-resolver-rules", "MAP 0.0.0.0/8 ~NOTFOUND, MAP 10.0.0.0/8 ~NOTFOUND, MAP 100.64.0.0/10 ~NOTFOUND, MAP 127.0.0.0/8 ~NOTFOUND, MAP 169.254.0.0/16 ~NOTFOUND, MAP 172.16.0.0/12 ~NOTFOUND, MAP 192.168.0.0/16 ~NOTFOUND, MAP 198.18.0.0/15 ~NOTFOUND, MAP ::1 ~NOTFOUND, MAP fc00::/7 ~NOTFOUND, MAP fe80::/10 ~NOTFOUND, MAP 2002::/16 ~NOTFOUND, MAP 2001::/32 ~NOTFOUND"))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, time.Duration(timeoutSec)*time.Second)
	defer cancelTimeout()

	// Hard on-origin rule check before initial navigation
	if sessionActive && !strings.EqualFold(parsed.Hostname(), origin) {
		_ = b.activity.Append(ActivityRecord{
			Origin:  origin,
			URL:     rawURL,
			Kind:    KindRead,
			Verdict: VerdictBlocked,
		})
		return fmt.Sprintf("refused: off-origin navigation to %q blocked (session is locked to %s)", rawURL, origin), nil
	}

	// 3. Navigate and wait for page to be ready
	if err := chromedp.Do(timeoutCtx, chromedp.Navigate(parsed.String()), chromedp.WaitReady("body")); err != nil {
		if isStartFailure(err) {
			return ErrBrowserUnavailable, nil
		}
		return "", fmt.Errorf("web_browse navigation failed: %w", err)
	}

	// Post-navigation check: ensure server redirect did not take us off-origin
	initialLoc, locErr := chromedp.Run(timeoutCtx, chromedp.Location())
	if sessionActive && (locErr != nil || initialLoc == "") {
		// Fail closed: with an active session we must be able to prove the
		// page's final location before handing its content to the model.
		_ = b.activity.Append(ActivityRecord{
			Origin:  origin,
			URL:     parsed.String(),
			Kind:    KindRead,
			Verdict: VerdictBlocked,
		})
		return "refused: could not confirm page location with an active session", nil
	}
	if sessionActive && initialLoc != "" {
		if u, err := url.Parse(initialLoc); err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), origin) {
			_ = b.activity.Append(ActivityRecord{
				Origin:  origin,
				URL:     initialLoc,
				Kind:    KindRead,
				Verdict: VerdictBlocked,
			})
			return fmt.Sprintf("refused: off-origin navigation to %q blocked (session is locked to %s)", initialLoc, origin), nil
		}
	}

	// 4. Execute acts sequentially
	if len(acts) > 0 {
		actObs, actBlocked, actErr := b.executeActs(timeoutCtx, sessionActive, origin, parsed, acts)
		if actBlocked || actErr != nil {
			return actObs, actErr
		}
	}

	// 5. Extract read-only elements and login-wall marker
	obs, finalURLStr, screenshotRel := b.extractPage(timeoutCtx, parsed, origin)

	// 6. Record activity and index
	now := time.Now().UTC().Format(time.RFC3339)
	if len(acts) > 0 {
		var intents []string
		for _, a := range acts {
			intents = append(intents, a.Intent)
		}
		_ = b.activity.Append(ActivityRecord{
			TS:            now,
			Origin:        origin,
			URL:           finalURLStr,
			Kind:          KindAct,
			Verdict:       VerdictOK,
			Intent:        strings.Join(intents, "; "),
			ScreenshotRef: screenshotRel,
		})
	}
	_ = b.activity.Append(ActivityRecord{
		TS:            now,
		Origin:        origin,
		URL:           finalURLStr,
		Kind:          KindRead,
		Verdict:       VerdictOK,
		ScreenshotRef: screenshotRel,
	})
	_ = b.index.Touch(origin, goal)

	return obs, nil
}

func (b *Browser) extractPage(timeoutCtx context.Context, parsed *url.URL, origin string) (string, string, string) {
	title, _ := chromedp.Run(timeoutCtx, chromedp.Title())
	visibleText, _ := chromedp.Run(timeoutCtx, chromedp.Evaluate[string]("document.body ? document.body.innerText : ''"))
	rawLinks, _ := chromedp.Run(timeoutCtx, chromedp.Evaluate[[]string]("Array.from(document.querySelectorAll('a[href]')).map(a => a.href)"))
	screenshotBuf, _ := chromedp.Run(timeoutCtx, chromedp.CaptureScreenshot())

	links := filterSameOriginLinks(rawLinks, parsed)

	var screenshotPath string
	var screenshotRel string
	if len(screenshotBuf) > 0 && b.dataDir != "" {
		shotsDir := filepath.Join(b.dataDir, "web", "shots")
		if err := os.MkdirAll(shotsDir, 0o700); err == nil {
			h := sha256.Sum256(screenshotBuf)
			ts := time.Now().UTC().Format("20060102-150405")
			filename := fmt.Sprintf("%s-%x.png", ts, h[:6])
			fullPath := filepath.Join(shotsDir, filename)
			if err := os.WriteFile(fullPath, screenshotBuf, 0o600); err == nil {
				screenshotPath = fullPath
				screenshotRel = filepath.Join("web", "shots", filename)
			}
		}
	}

	hasPasswordInput, _ := chromedp.Run(timeoutCtx, chromedp.Evaluate[bool]("document.querySelector('input[type=\"password\"]') !== null"))
	finalURLStr := parsed.String()
	finalLoc, _ := chromedp.Run(timeoutCtx, chromedp.Location())
	if finalLoc != "" {
		finalURLStr = finalLoc
	}
	finalParsed, _ := url.Parse(finalURLStr)
	if finalParsed == nil {
		finalParsed = parsed
	}

	isLoginWall := hasPasswordInput || loginPathRe.MatchString(finalParsed.Path)

	maxText := b.cfg.MaxTextBytes
	if maxText <= 0 {
		maxText = DefaultMaxTextBytes
	}
	visibleText = strings.TrimSpace(visibleText)
	if len(visibleText) > maxText {
		visibleText = truncateBytes(visibleText, maxText)
	}

	var sb strings.Builder
	if isLoginWall {
		fmt.Fprintf(&sb, "[login-wall origin=%s] Propose sign-in to the user rather than retry.\n\n", origin)
	}
	if title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", title)
	}
	fmt.Fprintf(&sb, "URL: %s\n", finalURLStr)
	if screenshotPath != "" {
		fmt.Fprintf(&sb, "Screenshot: %s\n", screenshotPath)
	}
	fmt.Fprintln(&sb)
	sb.WriteString(visibleText)

	if len(links) > 0 {
		sb.WriteString("\n\nLinks:\n")
		for _, l := range links {
			fmt.Fprintf(&sb, "- %s\n", l)
		}
	}
	return sb.String(), finalURLStr, screenshotRel
}

func (b *Browser) prepareSession(origin string) (bool, func(), string, error) {
	if b.wallet != nil && b.wallet.Has(origin) {
		activeDir, rel, err := b.wallet.Load(origin)
		if err != nil {
			return false, nil, "", fmt.Errorf("load session wallet: %w", err)
		}
		release := func() {
			_ = rel()
		}
		return true, release, activeDir, nil
	}
	return false, nil, "", nil
}

func (b *Browser) enforceActsGate(ctx context.Context, origin, targetURL, today string, acts []Act) (string, bool) {
	var intents []string
	for _, a := range acts {
		intents = append(intents, a.Intent)
	}
	summary := strings.Join(intents, "; ")

	entry, _ := b.index.Get(origin)
	policy := entry.Policy
	if policy == "" {
		policy = PolicyAlwaysAsk
	}

	// A stamped daily_allow day is the only free pass: always_ask, unknown
	// policies, and unstamped days all require a card (spec 10.1: one card
	// per site per day, re-prompted the next day).
	needsCard := policy != PolicyDailyAllow || entry.DailyAllowDate != today

	if !needsCard {
		return "", false
	}

	approver := b.approver
	if approver == nil {
		approver = ApproverFromContext(ctx)
	}
	if approver == nil {
		_ = b.activity.Append(ActivityRecord{
			Origin:  origin,
			URL:     targetURL,
			Kind:    KindAct,
			Verdict: VerdictDenied,
			Intent:  summary,
		})
		return denialGuidance("no-approver"), true
	}

	ok, approvalID := approver("web_browse", summary)
	if !ok {
		gate := "denied"
		if strings.Contains(strings.ToLower(approvalID), "timeout") {
			gate = "timeout"
		} else if strings.Contains(strings.ToLower(approvalID), "disconnected") {
			gate = "disconnected"
		}
		_ = b.activity.Append(ActivityRecord{
			Origin:  origin,
			URL:     targetURL,
			Kind:    KindAct,
			Verdict: VerdictDenied,
			Intent:  summary,
		})
		return denialGuidance(gate), true
	}

	if policy == PolicyDailyAllow {
		_ = b.index.StampDailyAllow(origin, today)
	}
	return "", false
}

func (b *Browser) executeActs(ctx context.Context, sessionActive bool, origin string, parsed *url.URL, acts []Act) (string, bool, error) {
	for _, act := range acts {
		// Password field refusal law
		if act.Kind == "fill" {
			checkPwdJS := fmt.Sprintf(`(() => {
				const el = document.querySelector(%s);
				return el && (el.getAttribute('type') || '').toLowerCase() === 'password';
			})()`, mustJSONString(act.Selector))
			isPwd, _ := chromedp.Run(ctx, chromedp.Evaluate[bool](checkPwdJS))
			if isPwd {
				_ = b.activity.Append(ActivityRecord{
					Origin:  origin,
					URL:     parsed.String(),
					Kind:    KindAct,
					Verdict: VerdictBlocked,
					Intent:  act.Intent,
				})
				return fmt.Sprintf("refused: fill act on password field %s is blocked (law: credentials are never handled by the agent)", act.Selector), true, nil
			}
		}

		// Off-origin refusal for navigational acts
		if sessionActive && act.Kind == "click" {
			checkHrefJS := fmt.Sprintf(`(() => {
				const el = document.querySelector(%s);
				if (!el) return '';
				const a = el.closest('a');
				return a ? a.href : '';
			})()`, mustJSONString(act.Selector))
			targetHref, _ := chromedp.Run(ctx, chromedp.Evaluate[string](checkHrefJS))
			if targetHref != "" {
				if u, err := url.Parse(targetHref); err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), origin) {
					_ = b.activity.Append(ActivityRecord{
						Origin:  origin,
						URL:     targetHref,
						Kind:    KindAct,
						Verdict: VerdictBlocked,
						Intent:  act.Intent,
					})
					return fmt.Sprintf("refused: off-origin navigation to %q blocked (session is locked to %s)", targetHref, origin), true, nil
				}
			}
		}

		if sessionActive && act.Kind == "submit" {
			checkActionJS := fmt.Sprintf(`(() => {
				const el = document.querySelector(%s);
				if (!el) return '';
				const form = el.tagName.toLowerCase() === 'form' ? el : el.form;
				return form ? form.action : '';
			})()`, mustJSONString(act.Selector))
			targetAction, _ := chromedp.Run(ctx, chromedp.Evaluate[string](checkActionJS))
			if targetAction != "" {
				if u, err := url.Parse(targetAction); err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), origin) {
					_ = b.activity.Append(ActivityRecord{
						Origin:  origin,
						URL:     targetAction,
						Kind:    KindAct,
						Verdict: VerdictBlocked,
						Intent:  act.Intent,
					})
					return fmt.Sprintf("refused: off-origin navigation to %q blocked (session is locked to %s)", targetAction, origin), true, nil
				}
			}
		}

		switch act.Kind {
		case "click":
			if _, err := chromedp.Run(ctx, chromedp.Click(act.Selector)); err != nil {
				return "", true, fmt.Errorf("click %q failed: %w", act.Selector, err)
			}
		case "fill":
			if _, err := chromedp.Run(ctx, chromedp.SetValue(act.Selector, act.Text)); err != nil {
				return "", true, fmt.Errorf("fill %q failed: %w", act.Selector, err)
			}
		case "submit":
			if _, err := chromedp.Run(ctx, chromedp.Submit(act.Selector)); err != nil {
				return "", true, fmt.Errorf("submit %q failed: %w", act.Selector, err)
			}
		}

		_, _ = chromedp.Run(ctx, chromedp.Sleep(200*time.Millisecond))

		postLoc, postErr := chromedp.Run(ctx, chromedp.Location())
		if sessionActive && (postErr != nil || postLoc == "") {
			// Fail closed (see initial-location law above).
			_ = b.activity.Append(ActivityRecord{
				Origin:  origin,
				URL:     parsed.String(),
				Kind:    KindAct,
				Verdict: VerdictBlocked,
				Intent:  act.Intent,
			})
			// Refusal is observation text for the model, not a tool error.
			//nolint:nilerr // fail-closed by contract; postErr is intentionally swallowed
			return "refused: could not confirm page location after act with an active session", true, nil
		}
		if sessionActive && postLoc != "" {
			if u, err := url.Parse(postLoc); err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), origin) {
				_ = b.activity.Append(ActivityRecord{
					Origin:  origin,
					URL:     postLoc,
					Kind:    KindAct,
					Verdict: VerdictBlocked,
					Intent:  act.Intent,
				})
				return fmt.Sprintf("refused: off-origin navigation to %q blocked (session is locked to %s)", postLoc, origin), true, nil
			}
		}
	}
	return "", false, nil
}

func mustJSONString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

func isStartFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "executable file not found") ||
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "cannot find") ||
		strings.Contains(s, "failed to start") ||
		strings.Contains(s, "exec:")
}

func filterSameOriginLinks(rawLinks []string, baseURL *url.URL) []string {
	var result []string
	seen := make(map[string]bool)

	for _, l := range rawLinks {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		u, err := url.Parse(l)
		if err != nil {
			continue
		}
		resolved := baseURL.ResolveReference(u)
		scheme := strings.ToLower(resolved.Scheme)
		if scheme != "http" && scheme != "https" {
			continue
		}
		if strings.EqualFold(resolved.Scheme, baseURL.Scheme) && strings.EqualFold(resolved.Host, baseURL.Host) {
			resolved.Fragment = ""
			resolved.RawFragment = ""
			linkStr := resolved.String()
			if !seen[linkStr] {
				seen[linkStr] = true
				result = append(result, linkStr)
				if len(result) >= 20 {
					break
				}
			}
		}
	}
	return result
}
