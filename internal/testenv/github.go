package testenv

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// GitHub stands in for the two GitHub services a rock index kept in git is
// served by: raw.githubusercontent.com, which serves the files of a branch,
// and the release downloads of github.com, which answer 302 with an object
// on another host, under a query string of its own — the only host that
// serves the bytes, at a path that does not end in the asset's name. The
// front host answers both, the objects host is a second server. Both are TLS
// servers whose certificate no system root signs, so a client lists
// InsecureServers among its own. A path nothing was put at is a 404, as
// GitHub answers, and every request is recorded.
type GitHub struct {
	front, objects *httptest.Server

	mu       sync.Mutex
	files    map[string][]byte
	assets   map[string]string
	blobs    map[string][]byte
	requests []string
}

// NewGitHub starts a GitHub, closed when tb ends.
func NewGitHub(tb testing.TB) *GitHub {
	tb.Helper()

	g := &GitHub{
		files:  map[string][]byte{},
		assets: map[string]string{},
		blobs:  map[string][]byte{},
	}

	g.objects = httptest.NewTLSServer(http.HandlerFunc(g.serveObject))
	tb.Cleanup(g.objects.Close)

	g.front = httptest.NewTLSServer(http.HandlerFunc(g.serveFront))
	tb.Cleanup(g.front.Close)

	return g
}

// URL is the base URL of the front host, without a trailing slash.
func (g *GitHub) URL() string { return g.front.URL }

// InsecureServers lists the hosts of both servers, as InsecureServers
// options take them.
func (g *GitHub) InsecureServers() []string {
	return []string{hostOf(g.front.URL), hostOf(g.objects.URL)}
}

// Put serves body at path on the front host, as raw serves a file of a
// branch ("org/repo/index/tt-rocks-index.json"), and returns its URL.
func (g *GitHub) Put(path string, body []byte) string {
	path = "/" + strings.TrimPrefix(path, "/")

	g.mu.Lock()
	defer g.mu.Unlock()

	g.files[path] = body

	return g.front.URL + path
}

// Release serves body as the asset name of the release tag of repo
// ("org/repo"), and returns its download URL, which redirects to the
// objects host.
func (g *GitHub) Release(repo, tag, name string, body []byte) string {
	path := "/" + repo + "/releases/download/" + tag + "/" + name

	g.mu.Lock()
	defer g.mu.Unlock()

	blob := "/release-asset/" + strconv.Itoa(len(g.blobs))
	g.assets[path] = blob
	g.blobs[blob] = body

	return g.front.URL + path
}

// Requests returns the paths requested so far, in order, each prefixed with
// the host that got it: "front /org/repo/..." or "objects /release-asset/0".
func (g *GitHub) Requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]string(nil), g.requests...)
}

// Requested reports whether the front host has been asked for a path whose
// last segment is name.
func (g *GitHub) Requested(name string) bool {
	for _, r := range g.Requests() {
		if strings.HasPrefix(r, "front ") && strings.HasSuffix(r, "/"+name) {
			return true
		}
	}

	return false
}

func (g *GitHub) serveFront(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, "front "+r.URL.Path)
	body, isFile := g.files[r.URL.Path]
	blob, isAsset := g.assets[r.URL.Path]
	g.mu.Unlock()

	switch {
	case isFile:
		_, _ = w.Write(body)
	case isAsset:
		http.Redirect(w, r, g.objects.URL+blob+"?X-Amz-Signature=0", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

func (g *GitHub) serveObject(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, "objects "+r.URL.Path)
	body, ok := g.blobs[r.URL.Path]
	g.mu.Unlock()

	if !ok {
		http.NotFound(w, r)

		return
	}

	_, _ = w.Write(body)
}

// hostOf is the host:port of a server URL httptest made.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic("testenv: httptest gave the unparsable URL " + raw)
	}

	return u.Host
}
