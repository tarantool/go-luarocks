//go:build integration_luaengine

package client_test

// Packing across backends (see interop_test.go for the harness): a rock one
// backend built is packed by either backend, and the binary rock installed
// by the first one into a fresh tree.

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// interopPackFixtures are the parity fixtures packed: Lua and C modules, an
// init module, wrapped and copied commands, conf and doc files, and a make
// rock.
func interopPackFixtures() []parityFixture {
	keep := map[string]bool{
		"install-sections": true, "copy-directories": true, "c-module": true, "init-module": true,
		"bin-scripts": true, "conf-files": true, "make-rock": true,
	}

	var out []parityFixture

	for _, fx := range parityFixtures() {
		if keep[fx.name] {
			out = append(out, fx)
		}
	}

	return out
}

// TestBackendInterop_Pack builds each pack fixture with A (Make and Build),
// packs the installed rock with B, and installs the packed .rock with A into
// a fresh tree, for every pair of backends. The two packers must produce the
// same archive under the same name, and each install of it must leave the
// tree a direct build by A leaves, and the tree an install of the rock A
// packed itself leaves.
func TestBackendInterop_Pack(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	backends := []client.Backend{client.BackendNative, client.BackendLua}

	// Upstream packs with the zip program: the engine has no lua-zlib, which
	// luarocks.tools.zip needs, so fs.zip is the one of fs/unix/tools.lua.
	if _, err := exec.LookPath("zip"); err != nil {
		testenv.Missingf(t, "zip not found on PATH; the lua backend packs a rock with it: %v", err)
	}

	for _, op := range parityOps() {
		if op.name == "install" {
			continue
		}

		for _, fx := range interopPackFixtures() {
			t.Run(op.name+"/"+fx.name, func(t *testing.T) {
				t.Parallel()

				release := interopSlot()
				defer release()

				x := interopCtx{prefix: prefix, incDir: incDir}

				type pair struct{ a, b client.Backend }

				direct := map[client.Backend]string{}
				packed := map[pair]string{}
				installed := map[pair]string{}

				for _, a := range backends {
					direct[a] = t.TempDir()
					parityStep(t, op, fx, false, a, direct[a], prefix, incDir)
				}

				for _, a := range backends {
					for _, b := range backends {
						p := pair{a, b}
						packed[p] = packInstalled(t, x, b, direct[a], fx.pkg)
						installed[p] = installPacked(t, x, a, packed[p], fx)

						if b == client.BackendNative {
							markNativePacked(installed[p]) // divPackModes
						}
					}
				}

				run := func(root string) interopRun {
					rx := x
					rx.trees = []string{root}

					return interopRun{trees: rx.trees, views: []map[string]string{interopViews(t, rx, 0, nil, nil)}}
				}

				for _, a := range backends {
					label := fmt.Sprintf("%s/%s, built by %v", op.name, fx.name, a)
					assert.Equal(t, filepath.Base(packed[pair{a, client.BackendLua}]),
						filepath.Base(packed[pair{a, client.BackendNative}]), "[%s] the name of the packed rock", label)
					compareArchivesBuiltBy(t, label, a, packed[pair{a, client.BackendNative}], packed[pair{a, client.BackendLua}])

					want := run(direct[a])
					compareReaders(t, label+", direct", want)

					for _, b := range backends {
						got := run(installed[pair{a, b}])
						compareReaders(t, fmt.Sprintf("%s, packed by %v", label, b), got)
						compareInteropRuns(t, fmt.Sprintf("%s, packed by %v vs direct", label, b), got, want)
						compareInteropRuns(t, fmt.Sprintf("%s, packed by %v vs packed by %v", label, b, a), got,
							run(installed[pair{a, a}]))
					}
				}
			})
		}
	}
}

// packInstalled packs the rock name installed in root with backend and
// returns the path of the archive.
func packInstalled(t *testing.T, x interopCtx, backend client.Backend, root, name string) string {
	t.Helper()

	px := x
	px.trees = []string{root}

	archive, err := newClient(t, px.cfg(t, 0), client.WithBackend(backend)).
		Pack(context.Background(), name, client.PackOpts{})
	require.NoError(t, err, "Pack %s (%v)", name, backend)

	return archive
}

// installPacked installs fx from the binary rock archive, served by a
// directory server of its own, into a fresh tree with backend, and returns
// the tree. The server's manifest lists the rock under the arch its file name
// carries (<name>-<version>.<arch>.rock), as upstream's pack names it.
func installPacked(t *testing.T, x interopCtx, backend client.Backend, archive string, fx parityFixture) string {
	t.Helper()

	base := filepath.Base(archive)
	arch, ok := strings.CutPrefix(base, fx.pkg+"-"+fx.version+".")
	arch, hasSuffix := strings.CutSuffix(arch, ".rock")
	require.True(t, ok && hasSuffix && arch != "", "packed rock %s is not named <name>-<version>.<arch>.rock", base)

	repo := t.TempDir()
	body, err := os.ReadFile(archive) //nolint:gosec // test temp path
	require.NoError(t, err, "read %s", archive)
	require.NoError(t, os.WriteFile(filepath.Join(repo, base), body, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(fmt.Sprintf(
		"commands = {}\nmodules = {}\nrepository = {\n   [%q] = { [%q] = { { arch = %q } } },\n}\n", fx.pkg, fx.version, arch)), 0o600))

	ix := x
	ix.trees = []string{t.TempDir()}

	require.NoError(t, newClient(t, ix.cfg(t, 0, repo), client.WithBackend(backend)).
		Install(context.Background(), fx.pkg, client.InstallOpts{Version: fx.version}),
		"Install %s from %s (%v)", fx.pkg, base, backend)

	return ix.trees[0]
}

// compareArchives requires the two rock archives to hold the same files with
// the same contents: both pack one installed rock.
func compareArchives(t *testing.T, label, native, lua string) {
	t.Helper()

	n, l := archiveDigest(t, native), archiveDigest(t, lua)
	assert.Equal(t, sortedKeys(setOf(l)), sortedKeys(setOf(n)), "[%s] the files of the packed rock (native vs lua)", label)

	for name, sum := range l {
		if got, ok := n[name]; ok {
			assert.Equal(t, sum, got, "[%s] %s in the packed rock (native vs lua)", label, name)
		}
	}
}

// archiveDigest returns the md5 of every file of the zip archive at path.
func archiveDigest(t *testing.T, path string) map[string]string {
	t.Helper()

	zr, err := zip.OpenReader(path)
	require.NoError(t, err, "open %s", path)

	defer func() { _ = zr.Close() }()

	out := map[string]string{}

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}

		rc, err := f.Open()
		require.NoError(t, err, "open %s in %s", f.Name, path)

		h := md5.New()
		_, err = io.Copy(h, rc) //nolint:gosec // test archive
		require.NoError(t, err)
		require.NoError(t, rc.Close())

		out[f.Name] = hex.EncodeToString(h.Sum(nil))
	}

	return out
}
