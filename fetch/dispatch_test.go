package fetch_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/fetch"
)

type recordingBackend struct {
	calls int
	url   string
	opts  fetch.Options
	// sourceRoot is echoed back as Result.SourceRoot, so a test can pin what
	// the dispatcher passes through from a backend.
	sourceRoot bool
}

func (r *recordingBackend) Fetch(
	_ context.Context, u, dst string, opts fetch.Options,
) (fetch.Result, error) {
	r.calls++
	r.url = u
	r.opts = opts

	return fetch.Result{Path: dst, SourceRoot: r.sourceRoot}, nil
}

//nolint:paralleltest // mutates the package-global backends dispatch table
func TestDispatch_RoutesScheme(t *testing.T) {
	rh := &recordingBackend{}
	rg := &recordingBackend{}
	rf := &recordingBackend{}
	restore := fetch.SwapBackends(map[string]fetch.Backend{
		"http":     rh,
		"https":    rh,
		"git":      rg,
		"git+ssh":  rg,
		"git+http": rg,
		"file":     rf,
	})

	defer restore()

	cases := []struct {
		url  string
		want *recordingBackend
	}{
		{"http://example.com/r.tar.gz", rh},
		{"https://example.com/r.zip", rh},
		{"git://example.com/r.git", rg},
		{"git+ssh://git@example.com/r.git", rg},
		{"git+http://example.com/r.git", rg},
		{"file:///tmp/foo", rf},
	}
	for _, c := range cases {
		before := c.want.calls
		_, err := fetch.Fetch(context.Background(), c.url, t.TempDir())
		require.NoError(t, err, "Fetch %s", c.url)
		assert.Equal(t, before+1, c.want.calls, "Fetch %s did not hit expected backend", c.url)
	}
}

func TestDispatch_UnknownScheme(t *testing.T) {
	t.Parallel()

	_, err := fetch.Fetch(context.Background(), "ftp://example.com/r.tar.gz", t.TempDir())
	require.Error(t, err, "expected error for unknown scheme")
	assert.ErrorIs(t, err, rocks.ErrUnsupportedRockspecFeature)
}

func TestDispatch_EmptyURL(t *testing.T) {
	t.Parallel()

	_, err := fetch.Fetch(context.Background(), "", t.TempDir())
	require.Error(t, err, "expected error for empty URL")
}

func TestSchemeOf(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"http://example.com/x":     "http",
		"HTTPS://example.com/x":    "https",
		"git+ssh://git@host/x.git": "git+ssh",
		"file:///x":                "file",
	}
	for in, want := range cases {
		got, err := fetch.SchemeOf(in)
		require.NoError(t, err, "schemeOf(%q)", in)
		assert.Equal(t, want, got, "schemeOf(%q)", in)
	}
}
