package router

import "net/http"

// providerClient is the one HTTP client for calls that carry provider
// credentials (Authorization: Bearer <api key>). Redirects are refused
// (H8/F1 family): a 3xx would replay the credential-bearing request to
// the redirect target — host chosen by whoever controls the redirect.
// A 3xx is therefore terminal and flows into the existing non-2xx paths.
var providerClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}
