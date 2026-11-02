// Package transport holds the HTTP transports the rock server index and the
// source fetch share, so the requests of a process reuse their connections.
package transport

import (
	"crypto/tls"
	"io"
	"net/http"
	"strings"
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
