package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
)

type approverCtxKey struct{}

// ContextWithApprover stores an Approver in ctx.
func ContextWithApprover(ctx context.Context, ap loop.Approver) context.Context {
	return context.WithValue(ctx, approverCtxKey{}, ap)
}

// ApproverFromContext extracts an Approver from ctx, if present.
func ApproverFromContext(ctx context.Context) loop.Approver {
	if ap, ok := ctx.Value(approverCtxKey{}).(loop.Approver); ok {
		return ap
	}
	return nil
}

// FetchTool returns the rails agent loop tool for fetching static web pages.
func FetchTool(cfg Config) loop.Tool {
	return loop.Tool{
		Spec: router.ToolSpec{
			Name:        "web_fetch",
			Description: "Fetch a public web page or API endpoint over HTTP/HTTPS and extract readable text or markdown. Best for static pages, documentation, and APIs (does not execute JavaScript).",
			ParamsJSONSchema: `{"type":"object","properties":{
				"url":{"type":"string","description":"public http or https URL to fetch"}
			},"required":["url"]}`,
		},
		Trusted: true,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", fmt.Errorf("bad args: %w", err)
			}
			u := strings.TrimSpace(a.URL)
			if u == "" {
				return "", errors.New("url is required")
			}
			return Fetch(ctx, u, cfg)
		},
	}
}

// BrowseTool returns the rails agent loop tool for driving headless Chromium.
func BrowseTool(home string, cfg Config, opts ...BrowseOption) loop.Tool {
	browser := NewBrowser(home, cfg, opts...)
	return loop.Tool{
		Spec: router.ToolSpec{
			Name:        "web_browse",
			Description: "Browse a public web page using a headless browser. Best for JavaScript-heavy single-page applications or dynamic pages where web_fetch is insufficient. Actions (acts) inside a signed-in session surface an approval card to the user automatically: call this tool directly and never discuss or negotiate approval in chat.",
			ParamsJSONSchema: `{"type":"object","properties":{
				"url":{"type":"string","description":"public http or https URL to browse"},
				"goal":{"type":"string","description":"what information to find or inspect on the page"},
				"acts":{"type":"array","description":"optional bounded sequence of actions to execute on the page","items":{
					"type":"object","properties":{
						"kind":{"type":"string","enum":["click","fill","submit"],"description":"action kind"},
						"selector":{"type":"string","description":"CSS selector for target element"},
						"text":{"type":"string","description":"text to fill (required for fill)"},
						"intent":{"type":"string","description":"one-line intent description (required)"}
					},
					"required":["kind","selector","intent"]
				}}
			},"required":["url"]}`,
		},
		Trusted: true,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				URL  string `json:"url"`
				Goal string `json:"goal"`
				Acts []Act  `json:"acts"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", fmt.Errorf("bad args: %w", err)
			}
			u := strings.TrimSpace(a.URL)
			if u == "" {
				return "", errors.New("url is required")
			}

			for i, act := range a.Acts {
				intent := strings.TrimSpace(act.Intent)
				if intent == "" {
					return "", errors.New("bad args: act intent is required")
				}
				if strings.Contains(intent, "\n") || strings.Contains(intent, "\r") {
					return "", errors.New("bad args: act intent must be a single line")
				}
				if strings.TrimSpace(act.Selector) == "" {
					return "", errors.New("bad args: act selector is required")
				}
				kind := strings.ToLower(strings.TrimSpace(act.Kind))
				if kind != "click" && kind != "fill" && kind != "submit" {
					return "", fmt.Errorf("bad args: unsupported act kind %q", act.Kind)
				}
				a.Acts[i].Kind = kind
				a.Acts[i].Intent = intent
			}

			return browser.Browse(ctx, u, a.Goal, a.Acts...)
		},
	}
}

// LoginTool returns the rails agent loop tool for guided browser sign-in sessions.
func LoginTool(home string, cfg Config, opts ...LoginOption) loop.Tool {
	return SessionLoginTool(home, cfg, opts...)
}
