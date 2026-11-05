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

// WebFetchTool returns the rails agent loop tool for fetching static web pages.
func WebFetchTool(cfg Config) loop.Tool {
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

// WebBrowseTool returns the rails agent loop tool for driving headless Chromium.
func WebBrowseTool(home string, cfg Config) loop.Tool {
	browser := NewBrowser(home, cfg)
	return loop.Tool{
		Spec: router.ToolSpec{
			Name:        "web_browse",
			Description: "Browse a public web page using a headless browser. Best for JavaScript-heavy single-page applications or dynamic pages where web_fetch is insufficient.",
			ParamsJSONSchema: `{"type":"object","properties":{
				"url":{"type":"string","description":"public http or https URL to browse"},
				"goal":{"type":"string","description":"what information to find or inspect on the page"}
			},"required":["url"]}`,
		},
		Trusted: true,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				URL  string `json:"url"`
				Goal string `json:"goal"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", fmt.Errorf("bad args: %w", err)
			}
			u := strings.TrimSpace(a.URL)
			if u == "" {
				return "", errors.New("url is required")
			}
			return browser.Browse(ctx, u, a.Goal)
		},
	}
}
