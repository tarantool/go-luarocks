package client_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	luarocksembed "github.com/tarantool/go-luarocks/internal/luarocks"
)

// TestLuaEngine_EmbedsOnlyWhatItLoads is the gate on the go:embed patterns of
// internal/luarocks: every embedded file is a module the engine preloads, a
// vendored module it deliberately answers with an absent-module stub, or the
// fork's license. Whatever else gets embedded ships in every binary that
// imports this module. A failure names the file; narrow the pattern, or
// preload the module if the engine needs it.
func TestLuaEngine_EmbedsOnlyWhatItLoads(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"src/COPYING": true}
	for _, path := range client.PreloadPaths() {
		allowed[path] = true
	}

	for _, mod := range client.AbsentModules() {
		allowed["src/src/"+strings.ReplaceAll(mod, ".", "/")+".lua"] = true
	}

	var extra []string

	err := fs.WalkDir(luarocksembed.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && !allowed[path] {
			extra = append(extra, path)
		}

		return nil
	})
	require.NoError(t, err, "walk the embedded FS")
	require.Empty(t, extra, "embedded files the engine never loads")
}
