// Package httpclient centralizes construction of HTTP clients for upstream
// WorkBuddy/CodeBuddy calls. Every client bypasses the system proxy, mirroring
// the Python upstream's `trust_env=False` on all httpx clients: honoring
// HTTP_PROXY/HTTPS_PROXY routes proxy-only-reachable requests (e.g. domestic
// billing when an intl proxy is running) through the proxy and breaks them,
// while direct calls succeed. All upstream endpoints must be reached directly.
package httpclient

import (
	"errors"
	"net/http"
	"time"
	"work2api/internal/workbuddy/siterouting"
)

// NoProxyTransport clones http.DefaultTransport (keeping its dial/keepalive/idle
// defaults) but disables proxy resolution.
func NoProxyTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

// New returns an *http.Client with the given timeout and no proxy.
func New(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NoProxyTransport(), CheckRedirect: GuardRedirect(nil)}
}

func GuardRedirect(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && siterouting.CloudHostedDomain(via[0].URL.Hostname()) && (req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host) {
			return http.ErrUseLastResponse
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}
