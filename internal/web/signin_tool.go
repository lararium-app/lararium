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
	Manager       *SigninManager
	Wallet        *Wallet
	Approver      loop.Approver
	CardRegistrar func(cardTitle, cardDest, summary string) (string, error)
	CardResult    func(approvalID, viewURL string)
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

// WithLoginCardRegistrar injects a custody card registrar callback.
func WithLoginCardRegistrar(fn func(cardTitle, cardDest, summary string) (string, error)) LoginOption {
	return func(c *LoginToolConfig) {
		c.CardRegistrar = fn
	}
}

// WithLoginCardResult injects a callback to update card result with the view URL.
func WithLoginCardResult(fn func(approvalID, viewURL string)) LoginOption {
	return func(c *LoginToolConfig) {
		c.CardResult = fn
	}
}

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

	mgr := tc.Manager
	if mgr == nil && wallet != nil {
		timeoutS := cfg.Signin.TimeoutS
		if timeoutS <= 0 {
			timeoutS = DefaultSigninTimeoutS
		}
		novncDir := cfg.Signin.NoVNCDir
		if novncDir == "" {
			novncDir = DefaultNoVNCDir
		}
		runner := NewRealRunner(wallet.Home(), WithRunnerNoVNCDir(novncDir))
		mgr = NewSigninManager(wallet, runner,
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

			// 2. If wallet.Has(origin) and !force -> return a plain tool result stating an existing saved session is present and force:true re-does sign-in. NO window opens.
			if wallet != nil && wallet.Has(originKey) && !a.Force {
				return fmt.Sprintf("an existing saved session is present for %s; pass force: true to re-do sign-in", host), nil
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

			if mgr == nil {
				return "this host cannot open a sign-in window: no sign-in manager configured", nil
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
			ok, approvalID := approver(cardTitle, "Open sign-in window")
			if !ok {
				gate := "denied"
				if strings.Contains(strings.ToLower(approvalID), "timeout") {
					gate = "timeout"
				} else if strings.Contains(strings.ToLower(approvalID), "disconnected") {
					gate = "disconnected"
				}
				return loginDenialGuidance(gate), nil
			}

			// 5. On approve: call the shared SigninManager.Start with the normalized origin.
			viewInfo, err := mgr.Start(ctx, originRaw)
			if err != nil {
				return fmt.Sprintf("failed to open sign-in window: %v", err), nil
			}

			// The card payload/result must then carry the VIEW URL (PART C token URL) so every paired surface gets a clickable link.
			if tc.CardResult != nil && approvalID != "" {
				tc.CardResult(approvalID, viewInfo.ViewURL)
			}
			if tc.CardRegistrar != nil {
				regID, _ := tc.CardRegistrar(cardTitle, host, viewInfo.ViewURL)
				if regID != "" {
					mgr.SetCardID(regID)
				}
			} else if mgr.CardID() == "" && approvalID != "" {
				mgr.SetCardID(approvalID)
			}

			// 6. The tool result (what the model sees) says: window is open, owner has been asked to sign in, session will be saved when they press Done; the model must NOT wait/poll. Return immediately after the card resolves approved.
			return "The sign-in window is open. The owner has been asked to sign in, and the session will be saved when they press Done. Do not wait or poll for completion.", nil
		},
	}
}
