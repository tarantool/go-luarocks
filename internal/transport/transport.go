// Package transport holds the HTTP transports the rock server index and the
// source fetch share, so the requests of a process reuse their connections.
package transport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxDrain bounds how much of an unwanted response body Drain reads to hand
// its connection back to the pool: past it, dropping the connection costs
// less than reading on.
const maxDrain = 64 << 10

var (
	verifying = newTransport(nil)
	//nolint:gosec // opt-in per host, see For
	skipping = newTransport(&tls.Config{InsecureSkipVerify: true})
)

// For returns the transport for a request to host. A host listed in insecure
// gets the one that does not verify TLS certificates — the escape hatch for
// rocks.tarantool.org, whose certificate upstream LuaRocks distrusts too —
// any other host the verifying one.
//
// Both are clones of http.DefaultTransport and live for the process, so they
// keep connections alive between requests, honour HTTP_PROXY, HTTPS_PROXY and
// NO_PROXY, and bound dialing and the TLS handshake. A transport built per
// request did none of that: every manifest, rockspec and source download paid
// for a new TCP connection and TLS handshake.
func For(host string, insecure []string) *http.Transport {
	for _, h := range insecure {
		if strings.EqualFold(h, host) {
			return skipping
		}
	}

	return verifying
}

// defaultMaxRedirects is how many redirects net/http follows by default.
const defaultMaxRedirects = 10

// ErrDowngrade is the error a redirect from https to plain http fails with.
var ErrDowngrade = errors.New("refusing to follow a redirect from https to http")

// NewClient returns the HTTP client of a request to a rock server or a
// source URL. Every request it sends, each hop of a redirect included, goes
// through the transport For picks for that request's own host, so a host
// listed in insecure skips certificate verification for its own requests
// only: a redirect from it to another host is verified. It refuses a
// redirect from https to plain http, which would send in the clear what was
// asked for over TLS, follows at most maxRedirects redirects (net/http's 10
// for 0), and gives up on a request after timeout.
func NewClient(insecure []string, timeout time.Duration, maxRedirects int) *http.Client {
	if maxRedirects <= 0 {
		maxRedirects = defaultMaxRedirects
	}

	return &http.Client{
		Transport: hops(insecure),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}

			from := via[len(via)-1].URL
			if strings.EqualFold(from.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return fmt.Errorf("%w: %s to %s", ErrDowngrade, from.Redacted(), req.URL.Redacted())
			}

			return nil
		},
	}
}

// hops is the round tripper of NewClient: it sends each request through the
// transport For picks for the request's own host.
type hops []string

// RoundTrip implements http.RoundTripper.
func (insecure hops) RoundTrip(req *http.Request) (*http.Response, error) {
	return For(req.URL.Host, insecure).RoundTrip(req)
}

// Drain reads what is left of a response body, up to maxDrain, so that
// closing it hands the connection back to the pool. A body closed unread takes
// its connection with it, so each 404 of a manifest probe would cost the next
// request a new connection and handshake.
func Drain(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrain))
}

func newTransport(tlsConfig *tls.Config) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// A program replaced the default transport: start from its defaults.
		base = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	}

	tr := base.Clone()
	tr.TLSClientConfig = tlsConfig

	return tr
}
