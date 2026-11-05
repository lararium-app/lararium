// Package web provides SSRF-safe web access for the rails agent:
// a static fetcher and a read-only headless browser, per
// docs/WEB-LAYER-SPEC.md.
package web

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	// ErrBrowserUnavailable is returned when Chromium cannot be found or started.
	ErrBrowserUnavailable = "web_browse unavailable: no browser installed — install chromium or set web.chrome_path"
	// ErrBrowserBusy is returned when another browse task holds the mutex.
	ErrBrowserBusy = "browser is busy, try again in a moment"
)

// Browser drives headless Chromium for dynamic web pages.
type Browser struct {
	mu      sync.Mutex
	dataDir string
	cfg     Config
}

// NewBrowser creates a new Browser instance.
func NewBrowser(dataDir string, cfg Config) *Browser {
	return &Browser{
		dataDir: dataDir,
		cfg:     cfg,
	}
}

// Browse executes a read-only navigation and extraction.
func (b *Browser) Browse(ctx context.Context, rawURL, _ string) (string, error) {
	if !b.mu.TryLock() {
		return ErrBrowserBusy, nil
	}
	defer b.mu.Unlock()

	parsed, _, err := ValidateURL(ctx, rawURL)
	if err != nil {
		return "", err
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
		// Defense-in-depth alongside ValidateURL: block every private,
		// loopback, link-local, carrier-NAT (Tailscale!), and reserved
		// destination at the browser resolver, so in-page redirects and
		// subresources cannot reach the local network either.
		chromedp.Flag("host-resolver-rules", "MAP 0.0.0.0/8 ~NOTFOUND, MAP 10.0.0.0/8 ~NOTFOUND, MAP 100.64.0.0/10 ~NOTFOUND, MAP 127.0.0.0/8 ~NOTFOUND, MAP 169.254.0.0/16 ~NOTFOUND, MAP 172.16.0.0/12 ~NOTFOUND, MAP 192.168.0.0/16 ~NOTFOUND, MAP 198.18.0.0/15 ~NOTFOUND, MAP ::1 ~NOTFOUND, MAP fc00::/7 ~NOTFOUND, MAP fe80::/10 ~NOTFOUND, MAP 2002::/16 ~NOTFOUND, MAP 2001::/32 ~NOTFOUND"),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, time.Duration(timeoutSec)*time.Second)
	defer cancelTimeout()

	// 1. Navigate and wait for page to be ready
	if err := chromedp.Do(timeoutCtx, chromedp.Navigate(parsed.String()), chromedp.WaitReady("body")); err != nil {
		if isStartFailure(err) {
			return ErrBrowserUnavailable, nil
		}
		return "", fmt.Errorf("web_browse navigation failed: %w", err)
	}

	// 2. Extract read-only elements
	title, _ := chromedp.Run(timeoutCtx, chromedp.Title())
	visibleText, _ := chromedp.Run(timeoutCtx, chromedp.Evaluate[string]("document.body ? document.body.innerText : ''"))
	rawLinks, _ := chromedp.Run(timeoutCtx, chromedp.Evaluate[[]string]("Array.from(document.querySelectorAll('a[href]')).map(a => a.href)"))
	screenshotBuf, _ := chromedp.Run(timeoutCtx, chromedp.CaptureScreenshot())

	links := filterSameOriginLinks(rawLinks, parsed)

	var screenshotPath string
	if len(screenshotBuf) > 0 && b.dataDir != "" {
		shotsDir := filepath.Join(b.dataDir, "web", "shots")
		if err := os.MkdirAll(shotsDir, 0o700); err == nil {
			h := sha256.Sum256(screenshotBuf)
			ts := time.Now().UTC().Format("20060102-150405")
			filename := fmt.Sprintf("%s-%x.png", ts, h[:6])
			fullPath := filepath.Join(shotsDir, filename)
			if err := os.WriteFile(fullPath, screenshotBuf, 0o600); err == nil {
				screenshotPath = fullPath
			}
		}
	}

	maxText := b.cfg.MaxTextBytes
	if maxText <= 0 {
		maxText = DefaultMaxTextBytes
	}
	visibleText = strings.TrimSpace(visibleText)
	if len(visibleText) > maxText {
		visibleText = truncateBytes(visibleText, maxText)
	}

	var sb strings.Builder
	if title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", title)
	}
	fmt.Fprintf(&sb, "URL: %s\n", parsed.String())
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

	return sb.String(), nil
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
