//go:build unix

package client_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// TestNativeModes_UnderUmask: the directories the native backend creates —
// a tree's, a rock's build/ staging dir in it, and an unpacked rock's — have
// the mode upstream LuaRocks gives them, 0777 less the process umask, and the
// files it deploys 0755 (commands) or 0644 (the rest) less the umask.
func TestNativeModes_UnderUmask(t *testing.T) {
	t.Parallel()

	for _, mask := range []int{0o022, 0o077} {
		root := t.TempDir()
		testenv.RunWithUmask(t, "TestNativeModes_Child", mask, root)

		for _, dir := range []string{"tree", "unpacked"} {
			requireNativeModes(t, filepath.Join(root, dir), os.FileMode(mask))
		}
	}
}

// TestNativeModes_Child is the body of the child processes of
// TestNativeModes_UnderUmask: it makes a pure-Lua rock with a command into
// root/tree, a tree that does not exist yet, packs it and unpacks the rock
// into root/unpacked.
func TestNativeModes_Child(t *testing.T) {
	t.Parallel()

	root := testenv.UmaskChild(t)
	src := filepath.Join(root, "src")

	for rel, body := range map[string]string{
		"modes.lua":      "return 1\n",
		"bin/modes-tool": "print(1)\n",
		"modes-1.0-1.rockspec": `package = "modes"
version = "1.0-1"
source = { url = "file://localhost/modes.tar.gz" }
build = {
   type = "builtin",
   modules = { modes = "modes.lua" },
   install = { bin = { ["modes-tool"] = "bin/modes-tool" } },
}
`,
	} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o777))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}

	r, err := client.New(rocks.Config{
		Tree:       filepath.Join(root, "tree"),
		WorkingDir: src,
		Tarantool:  rocks.TarantoolConfig{Prefix: "/opt/test", IncludeDir: "/opt/test/include"},
	})
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, r.Make(ctx, client.MakeOpts{RockspecPath: filepath.Join(src, "modes-1.0-1.rockspec")}))

	archive, err := r.Pack(ctx, "modes", client.PackOpts{})
	require.NoError(t, err)
	require.NoError(t, r.Unpack(ctx, archive, filepath.Join(root, "unpacked", "modes")))
}

// requireNativeModes checks the modes under root: every directory 0777, and
// outside the build/ staging dir of a rock and an unpacked rock, whose files
// keep the modes their sources have, every command 0755 and every other file
// 0644 or 0666, each less mask; for the masks tested, 022 and 077, 0666 and
// 0644 come out the same.
func requireNativeModes(t *testing.T, root string, mask os.FileMode) {
	t.Helper()

	var dirs int

	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		st, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		want := 0o666 &^ mask

		switch {
		case d.IsDir():
			want = 0o777 &^ mask
			dirs++
		case strings.Contains(rel, "/build/") || strings.HasPrefix(rel, "modes/"):
			return nil
		case strings.HasPrefix(rel, "bin/") || strings.Contains(rel, "/bin/"):
			want = 0o755 &^ mask
		}

		assert.Equal(t, want.String(), st.Mode().Perm().String(), "%s under umask %#o", p, mask)

		return nil
	}))

	assert.Positive(t, dirs, "the directories of %s", root)
}
