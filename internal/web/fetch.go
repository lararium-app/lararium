package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

const (
	// BrowserUserAgent is a standard browser user agent string.
	BrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	// FetchTimeout is the maximum request duration for web_fetch.
	FetchTimeout = 15 * time.Second
)

// FetchOption configures optional behavior for Fetch (e.g. test hooks).
type FetchOption func(*fetchOptions)

type fetchOptions struct {
	resolver        Resolver
	allowedTestHost string
	header          http.Header
}

// WithResolver sets a custom DNS resolver.
func WithResolver(r Resolver) FetchOption {
	return func(o *fetchOptions) {
		o.resolver = r
	}
}

// WithAllowedTestHost permits a specific host:port (used for httptest servers).
func WithAllowedTestHost(h string) FetchOption {
	return func(o *fetchOptions) {
		o.allowedTestHost = h
	}
}

// WithHeader sets or overrides an HTTP header for Fetch.
func WithHeader(key, value string) FetchOption {
	return func(o *fetchOptions) {
		if o.header == nil {
			o.header = make(http.Header)
		}
		o.header.Set(key, value)
	}
}

// WithHeaders sets or overrides multiple HTTP headers for Fetch.
func WithHeaders(h http.Header) FetchOption {
	return func(o *fetchOptions) {
		if o.header == nil {
			o.header = make(http.Header)
		}
		for k, vv := range h {
			o.header[k] = vv
		}
	}
}

// Fetch retrieves a public web page or API over HTTP/HTTPS, enforcing SSRF protections,
// redirect bounds, and content size caps.
func Fetch(ctx context.Context, rawURL string, cfg Config, opts ...FetchOption) (string, error) {
	var opt fetchOptions
	for _, fn := range opts {
		fn(&opt)
	}

	if opt.resolver == nil {
		opt.resolver = netDefaultResolverAdapter{}
	}

	// 1. Initial URL validation
	parsed, _, err := ValidateURLWithResolver(ctx, rawURL, opt.resolver, opt.allowedTestHost)
	if err != nil {
		return "", err
	}

	// 2. Safe HTTP client setup
	transport := &http.Transport{
		DialContext:       NewSafeDialContext(opt.resolver, opt.allowedTestHost),
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   FetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("stopped after 3 redirects")
			}
			// Re-validate each redirect hop URL against the SSRF guard
			if _, _, err := ValidateURLWithResolver(req.Context(), req.URL.String(), opt.resolver, opt.allowedTestHost); err != nil {
				return fmt.Errorf("ssrf: redirect destination blocked: %w", err)
			}
			return nil
		},
	}

	reqCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	defaults := [][2]string{
		{"User-Agent", BrowserUserAgent},
		{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		{"Accept-Language", "en-US,en;q=0.9"},
		{"Sec-Fetch-Mode", "navigate"},
		{"Sec-Fetch-Dest", "document"},
		{"Sec-Fetch-Site", "none"},
		{"Sec-Fetch-User", "?1"},
		{"Upgrade-Insecure-Requests", "1"},
	}
	for _, kv := range defaults {
		req.Header.Set(kv[0], kv[1])
	}
	if opt.header != nil {
		for k, vv := range opt.header {
			req.Header[k] = vv
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	// Response cap: read at most max_text_bytes * 4 bytes
	maxRead := int64(cfg.MaxTextBytes * 4)
	if maxRead <= 0 {
		maxRead = int64(DefaultMaxTextBytes * 4)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRead))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if DetectBotWall(resp.StatusCode, bodyBytes) {
		return FormatBlocked(bodyBytes), nil
	}

	finalURL := resp.Request.URL.String()

	// Determine Content-Type
	ct := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType == "" {
		mediaType = "text/plain"
	}

	isHTML := mediaType == "text/html" || mediaType == "application/xhtml+xml"
	isTextOrJSON := strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")

	if !isHTML && !isTextOrJSON {
		return fmt.Sprintf("binary content type %s, not fetched", mediaType), nil
	}

	maxText := cfg.MaxTextBytes
	if maxText <= 0 {
		maxText = DefaultMaxTextBytes
	}

	if isHTML {
		title, links := extractHTMLMeta(bytes.NewReader(bodyBytes), resp.Request.URL)

		markdown, err := htmltomarkdown.ConvertString(string(bodyBytes))
		if err != nil {
			markdown = string(bodyBytes)
		}
		markdown = strings.TrimSpace(markdown)

		if len(markdown) > maxText {
			markdown = truncateBytes(markdown, maxText)
		}

		var sb strings.Builder
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(&sb, "HTTP status: %s\n", resp.Status)
		}
		if title != "" {
			fmt.Fprintf(&sb, "Title: %s\n", title)
		}
		fmt.Fprintf(&sb, "URL: %s\n\n", finalURL)
		sb.WriteString(markdown)

		if len(links) > 0 {
			sb.WriteString("\n\nLinks:\n")
			for _, l := range links {
				fmt.Fprintf(&sb, "- %s\n", l)
			}
		}

		return sb.String(), nil
	}

	// Plain text or JSON
	text := string(bodyBytes)
	if len(text) > maxText {
		text = truncateBytes(text, maxText)
	}

	var sb strings.Builder
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(&sb, "HTTP status: %s\n", resp.Status)
	}
	fmt.Fprintf(&sb, "URL: %s\n", finalURL)
	fmt.Fprintf(&sb, "Content-Type: %s\n\n", mediaType)
	sb.WriteString(text)
	return sb.String(), nil
}

type netDefaultResolverAdapter struct{}

// LookupIP resolves host via the standard library's default resolver.
func (netDefaultResolverAdapter) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, network, host)
}
