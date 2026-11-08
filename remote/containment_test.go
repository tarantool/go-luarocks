package remote_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/remote"
)

// TestFileRemoteIndex_RowsStayInServerDir: a local server's artifact path is
// joined from the name, version and arch its manifest lists, so a version or
// an arch holding ".." named a file outside the server directory. Such a row
// is dropped; the rows beside it are kept.
func TestFileRemoteIndex_RowsStayInServerDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "repo")
	writeFile(t, filepath.Join(dir, "manifest"), `repository = { foo = {
  ["1.0-1"] = { { arch = "all" }, { arch = "../../../outside/x" } },
  ["../../outside/2.0-1"] = { { arch = "installed" } },
} }`)

	idx := &remote.FileRemoteIndex{Server: dir}

	rows, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, filepath.Join(dir, "foo-1.0-1.all.rock"), rows[0].URL)

	matches, err := remote.Search(context.Background(), []string{dir}, remote.IndexOptions{},
		remote.Query{Name: "foo", Arch: []string{"all", "installed", "../../../outside/x"}})
	require.NoError(t, err)

	for _, m := range matches {
		assert.True(t, strings.HasPrefix(m.URL, dir+string(filepath.Separator)), m.URL)
		assert.Equal(t, dir, filepath.Dir(m.URL), m.URL)
	}

	_, err = idx.Query(context.Background(), "foo", "../..")
	require.Error(t, err)
}
