package client_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/tree"
)

// whichTree writes a tree manifest into root in which each provider
// ("name/version") of each module, in the order given, provides it as
// file, and creates every file in files (tree-relative).
func whichTree(t *testing.T, root string, modules map[string][]string, file map[string]string, files ...string) {
	t.Helper()

	m := &rocks.Manifest{
		Repository:   map[string]map[string]rocks.RepoEntry{},
		Modules:      modules,
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}

	for mod, providers := range modules {
		for _, p := range providers {
			name, version, _ := strings.Cut(p, "/")
			if m.Repository[name] == nil {
				m.Repository[name] = map[string]rocks.RepoEntry{}
			}

			entry := m.Repository[name][version]
			entry.Arch = "installed"

			if entry.Modules == nil {
				entry.Modules = map[string]string{}
			}

			entry.Modules[mod] = file[mod]
			m.Repository[name][version] = entry
		}
	}

	require.NoError(t, manif.FileStore{}.WriteTree(tree.Paths{Tree: root}.RocksDir(), m), "WriteTree")

	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("return {}\n"), 0o600))
	}
}

// TestWhich_ConfigTreeOnly pins the native Which of go-luarocks: the module's
// plain paths in Config.Tree, and nothing else. A module only another rock
// tree (here the Tarantool prefix) provides is not found, and the tree
// manifest is not consulted, so the plain path answers even when the manifest
// lists a newer provider at a versioned name — where upstream `luarocks
// which` answers from the manifests of every rock tree.
func TestWhich_ConfigTreeOnly(t *testing.T) {
	t.Parallel()

	a, b := t.TempDir(), t.TempDir()

	whichTree(t, a,
		map[string][]string{"mod": {"aaa/1.0-1", "bbb/2.0-1"}},
		map[string]string{"mod": "mod.lua"},
		"share/tarantool/mod.lua", "share/tarantool/bbb_2_0_1-mod.lua")
	whichTree(t, b,
		map[string][]string{"onlyb": {"ccc/2.0-1"}},
		map[string]string{"onlyb": "onlyb.lua"},
		"share/tarantool/onlyb.lua")

	r, err := client.New(rocks.Config{
		Tree:       a,
		WorkingDir: t.TempDir(),
		Tarantool:  rocks.TarantoolConfig{Prefix: b},
	})
	require.NoError(t, err, "New")

	for _, tc := range []struct {
		module, want string
	}{
		{"mod", filepath.Join(a, "share", "tarantool", "mod.lua")},
		{"onlyb", ""},
		{"absent", ""},
	} {
		got, ok, err := r.Which(context.Background(), tc.module)
		require.NoError(t, err, "Which %s", tc.module)
		assert.Equal(t, tc.want != "", ok, "Which %s found", tc.module)
		assert.Equal(t, tc.want, got, "Which %s", tc.module)
	}
}
