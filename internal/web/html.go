package web

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// extractHTMLMeta parses HTML to extract the page title and up to 20 unique
// same-origin absolute links.
func extractHTMLMeta(r io.Reader, baseURL *url.URL) (string, []string) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", nil
	}

	var title string
	var links []string
	seen := make(map[string]bool)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if strings.EqualFold(n.Data, "title") && title == "" && n.FirstChild != nil {
				title = strings.TrimSpace(n.FirstChild.Data)
			}
			if strings.EqualFold(n.Data, "a") {
				for _, attr := range n.Attr {
					if strings.EqualFold(attr.Key, "href") {
						href := strings.TrimSpace(attr.Val)
						if href == "" ||
							strings.HasPrefix(href, "#") ||
							strings.HasPrefix(href, "javascript:") ||
							strings.HasPrefix(href, "mailto:") ||
							strings.HasPrefix(href, "tel:") {
							continue
						}

						parsedHref, err := url.Parse(href)
						if err != nil {
							continue
						}

						resolved := baseURL.ResolveReference(parsedHref)
						resolvedScheme := strings.ToLower(resolved.Scheme)
						if resolvedScheme != "http" && resolvedScheme != "https" {
							continue
						}

						// Same origin only: scheme and host must match baseURL
						if strings.EqualFold(resolved.Scheme, baseURL.Scheme) && strings.EqualFold(resolved.Host, baseURL.Host) {
							resolved.Fragment = ""
							resolved.RawFragment = ""
							linkStr := resolved.String()
							if !seen[linkStr] {
								seen[linkStr] = true
								if len(links) < 20 {
									links = append(links, linkStr)
								}
							}
						}
					}
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}

	walk(doc)
	return title, links
}

// truncateBytes truncates a string so its byte representation does not exceed maxBytes.
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Avoid cutting in the middle of a multi-byte UTF-8 code point.
	for maxBytes > 0 && maxBytes < len(s) {
		r := s[maxBytes]
		// In UTF-8, continuation bytes have bit pattern 10xxxxxx (0x80 <= b <= 0xBF).
		if (r & 0xC0) != 0x80 {
			break
		}
		maxBytes--
	}
	return s[:maxBytes]
}
