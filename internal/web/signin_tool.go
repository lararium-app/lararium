package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
)

// LoginOption configures a session_login tool instance.
type LoginOption func(*LoginToolConfig)

// LoginToolConfig holds dependencies and options for session_login.
type LoginToolConfig struct {
	Manager         *SigninManager
	ManagerProvider func() *SigninManager
	Wallet          *Wallet
	Index           *WalletIndex
	Approver        loop.Approver
}

// WithLoginIndex injects a WalletIndex into the tool.
func WithLoginIndex(idx *WalletIndex) LoginOption {
	return func(c *LoginToolConfig) {
		c.Index = idx
	}
}

// WithLoginManager injects a SigninManager into the tool.
func WithLoginManager(mgr *SigninManager) LoginOption {
	return func(c *LoginToolConfig) {
		c.Manager = mgr
	}
}

// WithLoginWallet injects a Wallet into the tool.
func WithLoginWallet(w *Wallet) LoginOption {
	return func(c *LoginToolConfig) {
		c.Wallet = w
	}
}

// WithLoginApprover injects a custom approver callback into the tool.
func WithLoginApprover(ap loop.Approver) LoginOption {
	return func(c *LoginToolConfig) {
		c.Approver = ap
	}
}

// WithLoginManagerProvider injects a late-bound provider for the shared
// SigninManager. The provider is consulted on every call so the tool and the
// /signin/ routes operate on the single surface-owned instance even when the
// tool registry is built before the Server exists.
func WithLoginManagerProvider(fn func() *SigninManager) LoginOption {
	return func(c *LoginToolConfig) {
		c.ManagerProvider = fn
	}
}

// signinValidateURL resolves and screens the sign-in target against the web
// denylist (SSRF parity with web_fetch/web_browse). A var so tests can
// inject deterministic answers.
var signinValidateURL = ValidateURL

// extractHost extracts the host/domain portion of an origin string without credentials or scheme.
func extractHost(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty origin")
	}

	var host string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("parse origin URL: %w", err)
		}
		host = u.Hostname()
		if host == "" {
			host = u.Host
		}
	} else {
		if idx := strings.IndexByte(raw, '/'); idx >= 0 {
			raw = raw[:idx]
		}
		if h, _, err := net.SplitHostPort(raw); err == nil {
			host = h
		} else {
			host = raw
		}
	}
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "" || host == "." || host == ".." {
		return "", errors.New("invalid origin host")
	}
	return host, nil
}

func loginDenialGuidance(gate string) string {
	const tool = "session_login"
	var b strings.Builder
	switch gate {
	case "timeout":
		b.WriteString("Approval for tool " + tool + " timed out: nobody answered the approval card within serve.approval_timeout.")
	case "no-approver":
		b.WriteString("Blocked by the approval policy: tool " + tool + " requires approval and no approver is wired.")
	case "disconnected":
		b.WriteString("Blocked by the approval policy: tool " + tool + " needs approval but no approver channel was connected to answer it.")
	default:
		b.WriteString("Blocked by the approval policy: the user denied tool " + tool + " on its approval card.")
	}
	return b.String()
}

// sessionPresenceGuidance decides what happens when a saved session already
// exists for the origin: a fresh session short-circuits the call, a stale
// (login-wall-marked) one proceeds to the card flow unless the day's re-raise
// has already been posted. A non-empty first return is the tool result to
// hand back as-is; the bool reports whether the origin is marked stale.
func sessionPresenceGuidance(idx *WalletIndex, wallet *Wallet, originKey, host string, force bool) (string, bool) {
	if wallet == nil || !wallet.Has(originKey) {
		return "", false
	}
	var entry WalletEntry
	if idx != nil {
		entry, _ = idx.Get(originKey)
	}
	isStale := entry.Stale
	if force {
		return "", isStale
	}
	if !isStale {
		return fmt.Sprintf("an existing saved session is present for %s; pass force: true to re-do sign-in", host), false
	}
	if entry.ReRaiseDate == time.Now().UTC().Format("2006-01-02") {
		return fmt.Sprintf("a refresh sign-in for %s was raised earlier today; approve it or pass force:true", host), isStale
	}
	return "", isStale
}

// SessionLoginTool returns the rails agent loop tool for guided browser sign-in sessions.
func SessionLoginTool(home string, cfg Config, opts ...LoginOption) loop.Tool {
	tc := &LoginToolConfig{}
	for _, opt := range opts {
		opt(tc)
	}

	wallet := tc.Wallet
	if wallet == nil && home != "" {
		wallet = NewWallet(home, nil)
	}

	idx := tc.Index
	if idx == nil {
		if home != "" {
			idx = NewWalletIndex(home)
		} else if wallet != nil && wallet.Home() != "" {
			idx = NewWalletIndex(wallet.Home())
		}
	}

	// buildMgr constructs a private manager for standalone use (tests, the
	// repl). The daemon path always resolves the shared surface instance via
	// tc.Manager first or the provider second.
	buildMgr := func() *SigninManager {
		if wallet == nil {
			return nil
		}
		timeoutS := cfg.Signin.TimeoutS
		if timeoutS <= 0 {
			timeoutS = DefaultSigninTimeoutS
		}
		novncDir := cfg.Signin.NoVNCDir
		if novncDir == "" {
			novncDir = DefaultNoVNCDir
		}
		runner := NewRealRunner(wallet.Home(), WithRunnerNoVNCDir(novncDir))
		return NewSigninManager(wallet, runner,
			WithTimeout(time.Duration(timeoutS)*time.Second),
			WithViewBaseURL(cfg.Signin.ViewBaseURL),
			WithNoVNCDir(novncDir),
		)
	}

	return loop.Tool{
		Spec: router.ToolSpec{
			Name: "session_login",
			Description: "Open a guided sign-in window for the user to authenticate to an origin. " +
				"Actions surface an approval card to the user: call this tool directly and never discuss or negotiate approval in chat.",
			ParamsJSONSchema: `{"type":"object","properties":{
				"origin":{"type":"string","description":"origin URL or hostname to sign in to (e.g. https://example.com)"},
				"force":{"type":"boolean","description":"re-authenticate even if an existing session is saved"}
			},"required":["origin"]}`,
		},
		Trusted: true,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				Origin string `json:"origin"`
				Force  bool   `json:"force"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", fmt.Errorf("bad args: %w", err)
			}
			originRaw := strings.TrimSpace(a.Origin)
			if originRaw == "" {
				return "invalid origin: origin is required", nil
			}

			// 1. Normalize via the existing OriginKey; bad origin -> tool error string, no panic.
			originKey, err := OriginKey(originRaw)
			if err != nil {
				return fmt.Sprintf("invalid origin %q: %v", originRaw, err), nil
			}

			host, err := extractHost(originRaw)
			if err != nil {
				return fmt.Sprintf("invalid origin %q: %v", originRaw, err), nil
			}

			// 2. Saved session, stale expiry check, and rate limiting
			guidance, isStale := sessionPresenceGuidance(idx, wallet, originKey, host, a.Force)
			if guidance != "" {
				return guidance, nil
			}

			// 3. If the window core prereqs are missing (CheckPrereqs -> ErrPrereqMissing) -> return an honest tool result naming the missing binary.
			novncDir := cfg.Signin.NoVNCDir
			if novncDir == "" {
				novncDir = DefaultNoVNCDir
			}
			if err := CheckPrereqs(novncDir); err != nil {
				var prereqErr *ErrPrereqMissing
				if errors.As(err, &prereqErr) {
					return fmt.Sprintf("this host cannot open a sign-in window: %s not installed", prereqErr.Binary), nil
				}
				return fmt.Sprintf("this host cannot open a sign-in window: %v", err), nil
			}

			// Resolve the manager per call: the provider (the surface-owned
			// shared instance) wins over a build-time manager, because a
			// private instance would open a window the /signin/ routes can
			// never serve.
			mgr := tc.Manager
			if tc.ManagerProvider != nil {
				mgr = tc.ManagerProvider()
			}
			if mgr == nil {
				mgr = buildMgr()
			}
			if mgr == nil {
				return "this host cannot open a sign-in window: no sign-in manager configured", nil
			}

			// Denylist parity with web_fetch/web_browse: never raise a
			// sign-in window at a private-network, link-local, or blocked
			// host (router/SSRF confusion traps that noVNC can click).
			target := originRaw
			if !strings.Contains(target, "://") {
				target = "https://" + target
			}
			if _, _, err := signinValidateURL(ctx, target); err != nil {
				return fmt.Sprintf("sign-in window refused for %s: %v", host, err), nil
			}

			// 4. Otherwise raise a custody approval card titled "Sign me in to <host>" (host only — NEVER any credential text).
			// Actions follow the existing custody-card action pattern: approve ("Open sign-in window") and deny ("Cancel").
			// Then wait on the approval exactly like other gated tools do.
			approver := tc.Approver
			if approver == nil {
				approver = ApproverFromContext(ctx)
			}
			if approver == nil {
				return loginDenialGuidance("no-approver"), nil
			}

			cardTitle := "Sign me in to " + host
			if isStale {
				cardTitle = "Re-sign me in to " + host
			}
			// Pre-mint the view token so this same approval card can carry
			// the link: it 404s until approval lets Start bind the assets,
			// then becomes live (Start reuses a still-fresh pre-mint).
			mgr.NewViewToken()
			summary := "Open sign-in window"
			if u := mgr.ViewURL(); u != "" {
				summary = "Open sign-in window. After you approve, open this link to enter the credentials: " + u
			}
			// The day's re-raise quota is consumed only when a card truly
			// goes out — a prereq or window refusal must not burn it.
			if isStale && idx != nil {
				_ = idx.StampReRaise(originKey, time.Now().UTC().Format("2006-01-02"))
			}
			ok, approvalID := approver(cardTitle, summary)
			if !ok {
				mgr.ClearViewToken()
				gate := "denied"
				if strings.Contains(strings.ToLower(approvalID), "timeout") {
					gate = "timeout"
				} else if strings.Contains(strings.ToLower(approvalID), "disconnected") {
					gate = "disconnected"
				}
				return loginDenialGuidance(gate), nil
			}

			// When the user taps Done/Cancel the HTTP route resolves this
			// card — the single resolution path (the approval hub
			// double-resolve is a no-op, but one writer keeps it honest).
			if approvalID != "" {
				mgr.SetCardID(approvalID)
			}

			// 5. On approve: open the window on the shared manager.
			if _, err := mgr.Start(ctx, originRaw); err != nil {
				return fmt.Sprintf("failed to open sign-in window: %v", err), nil
			}
			// 6. The tool result (what the model sees) says: window is open, owner has been asked to sign in, session will be saved when they press Done; the model must NOT wait/poll. Return immediately after the card resolves approved.
			return "The sign-in window is open. The owner has been asked to sign in, and the session will be saved when they press Done. Do not wait or poll for completion.", nil
		},
	}
}
