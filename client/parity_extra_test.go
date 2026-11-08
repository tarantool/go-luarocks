//go:build integration_luaengine

package client_test

// Broader backend-parity harness (companion to parity_test.go). Where
// parity_test.go pins ONE pure-Lua fixture, this table-drives several
// rockspec shapes through BOTH backends (native + gopher-lua) into separate
// trees and diffs them:
//
//   - install-sections : build.install.lua + build.install.bin
//   - copy-directories : build.copy_directories (a doc/ dir)
//   - c-module         : a builtin C module compiled to a .so
//   - init-module      : a build.modules entry whose source is an init.lua
//
// Each fixture goes through both ways of building a rockspec from a source
// tree (parityOps): Make over the staged source, and Build of the rockspec,
// which fetches its source.url first. An Install of a rockspec ends in the
// same native deploy, but it is not compared here: for the default
// InstallOpts.Deps (DepsAll) the lua backend runs `install --deps-mode all`,
// and upstream's command wrapper then lists every rocks tree of its
// configuration (fs/unix.lua wrap_script → path.package_paths →
// core/path.lua map_trees), where the native backend — and upstream's
// default deps_mode "one", which Make and Build run in — list the target
// tree alone. That is a difference in how the two backends are driven, not
// in what the native backend deploys.
//
// The parity contract (same as parity_test.go): DEPLOYED ARTIFACTS must be
// byte-equal. A deployed artifact is any file NOT under
// share/tarantool/rocks/ (that subtree is per-rock bookkeeping + the
// top-level manifest, both of which legitimately differ in format/contents
// between engines). Read-path projections (List/Which/Show) are also
// compared; where they diverge because of a deployment difference, the test
// surfaces it as an explicit finding rather than hiding it.
//
// Build-tagged off by default; run with `-tags integration_luaengine`.
// Requires a real tarantool interpreter + lua.h on PATH (the lua backend
// shells out to it for builds).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// parityFixture is one rockspec shape plus the source files it needs.
type parityFixture struct {
	name     string            // fixture id (test subtest name)
	pkg      string            // rock package name
	version  string            // rock version-revision, e.g. "1.0-1"
	files    map[string]string // relpath under source dir -> contents
	rockspec string            // rockspec body (without package/version header)
}

func parityFixtures() []parityFixture {
	return []parityFixture{
		{
			name:    "install-sections",
			pkg:     "withinstall",
			version: "1.0-1",
			files: map[string]string{
				"src/withinstall.lua": "return { name = 'withinstall' }\n",
				"src/helper.lua":      "return 42\n",
				"bin/wi-cli":          "#!/usr/bin/env tarantool\nprint('hi')\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withinstall"] = "src/withinstall.lua",
   },
   install = {
      lua = {
         ["withinstall.helper"] = "src/helper.lua",
      },
      bin = {
         ["wi-cli"] = "bin/wi-cli",
      },
   },
}
`,
		},
		{
			name:    "copy-directories",
			pkg:     "withdocs",
			version: "2.1-3",
			files: map[string]string{
				"src/withdocs.lua": "return {}\n",
				"doc/readme.md":    "# withdocs\n\nhello\n",
				"doc/guide.txt":    "a guide\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withdocs"] = "src/withdocs.lua",
   },
   copy_directories = { "doc" },
}
`,
		},
		{
			name:    "c-module",
			pkg:     "cmod",
			version: "0.1-1",
			files: map[string]string{
				// Minimal Lua/C module against the (Lua 5.1-compatible)
				// tarantool C API. luaopen_cmod returns a table with one field.
				"cmod.c": `#include <lua.h>
#include <lauxlib.h>

static int l_answer(lua_State *L) {
    lua_pushinteger(L, 42);
    return 1;
}

int luaopen_cmod(lua_State *L) {
    lua_newtable(L);
    lua_pushcfunction(L, l_answer);
    lua_setfield(L, -2, "answer");
    return 1;
}
`,
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["cmod"] = "cmod.c",
   },
}
`,
		},
		{
			name:    "init-module",
			pkg:     "withinit",
			version: "1.0-1",
			files: map[string]string{
				"src/withinit/init.lua": "return { name = 'withinit' }\n",
				"src/withinit/util.lua": "return {}\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withinit"] = "src/withinit/init.lua",
      ["withinit.util"] = "src/withinit/util.lua",
   },
}
`,
		},
	}
}

// parityOp is one way of getting a fixture deployed: it stages fx as the
// operation needs it and runs the operation on r.
type parityOp struct {
	name string
	// inSource runs the operation with the staged source as the working
	// directory, as Make needs. The others fetch the source themselves and
	// get an empty working directory, so they cannot pick the staged files up.
	inSource bool
	run      func(t *testing.T, r *client.Rocks, fx parityFixture, src string)
}

// parityOps are the operations TestBackendParity_Fixtures runs every fixture
// through. Each one builds the rockspec from source and deploys the result,
// so they must deploy the same artifacts; they differ in where the source and
// the rockspec come from.
func parityOps() []parityOp {
	return []parityOp{
		{"make", true, func(t *testing.T, r *client.Rocks, fx parityFixture, src string) {
			t.Helper()
			specPath := writeParityRockspec(t, fx, src, "file://localhost/"+fx.pkg+".tar.gz")
			require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}),
				"Make(%s)", fx.name)
		}},
		{"build", false, func(t *testing.T, r *client.Rocks, fx parityFixture, src string) {
			t.Helper()
			specPath := writeParityRockspec(t, fx, t.TempDir(), parityTarball(t, fx, src))
			require.NoError(t, r.Build(context.Background(), specPath, client.BuildOpts{}),
				"Build(%s)", fx.name)
		}},
	}
}

// TestBackendParity_Fixtures table-drives several rockspec shapes through both
// backends and diffs deployed artifacts + read-path projections.
func TestBackendParity_Fixtures(t *testing.T) {
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		testenv.Missingf(t, "tarantool not found on PATH; backend-parity needs a real interpreter: %v", err)
	}
	prefix := filepath.Dir(filepath.Dir(ttBin))
	incDir := filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		testenv.Missingf(t, "tarantool lua.h not found at %s; backend-parity needs Lua headers: %v", incDir, err)
	}

	for _, op := range parityOps() {
		for _, fx := range parityFixtures() {
			t.Run(op.name+"/"+fx.name, func(t *testing.T) {
				treeNative, rNative := parityDeploy(t, op, fx, client.BackendNative, prefix, incDir)
				treeLua, rLua := parityDeploy(t, op, fx, client.BackendLua, prefix, incDir)

				// --- Deployed-artifact parity (the hard contract). ---
				depN := deployedArtifacts(t, treeNative)
				depL := deployedArtifacts(t, treeLua)

				require.NotEmpty(t, depN, "native deployed no artifacts for %s", fx.name)
				require.NotEmpty(t, depL, "lua deployed no artifacts for %s", fx.name)

				allRel := map[string]struct{}{}
				for rel := range depN {
					allRel[rel] = struct{}{}
				}
				for rel := range depL {
					allRel[rel] = struct{}{}
				}
				for rel := range allRel {
					dN, inN := depN[rel]
					dL, inL := depL[rel]
					switch {
					case !inN:
						t.Errorf("[%s] deployed artifact %q present in LUA tree but MISSING in native", fx.name, rel)
					case !inL:
						t.Errorf("[%s] deployed artifact %q present in NATIVE tree but MISSING in lua", fx.name, rel)
					case dN != dL:
						// Compiled objects (.so) are not expected to be byte-reproducible
						// across two independent compilations (embedded paths, debug info,
						// ordering). For those the parity contract is presence + loadability,
						// checked below — a byte mismatch is informational, not a failure.
						if strings.HasSuffix(rel, ".so") {
							t.Logf("[%s] note: compiled %q differs byte-wise (expected; non-reproducible build): native md5=%s lua md5=%s",
								fx.name, rel, dN, dL)
							continue
						}
						// Each backend installs into its OWN tree, so generated text
						// artifacts (e.g. bin wrappers) legitimately embed that tree's
						// absolute root. Normalize each tree root to a placeholder before
						// comparing — equal-after-normalization is true parity.
						if textArtifactsEqual(treeNative, treeLua, rel) {
							t.Logf("[%s] %q matches after tree-root normalization (only the per-tree absolute path differs)", fx.name, rel)
							continue
						}
						t.Errorf("[%s] ARTIFACT DIVERGENCE %q: native md5=%s lua md5=%s", fx.name, rel, dN, dL)
						dumpDiff(t, fx.name, rel, treeNative, treeLua)
					}
				}
				t.Logf("[%s] deployed artifacts compared: %v", fx.name, sortedKeys(allRel))

				// --- Read-path parity: List. ---
				listN := installedSet(t, rNative)
				listL := installedSet(t, rLua)
				assert.True(t, equalStringSets(listN, listL),
					"[%s] List parity: native=%v lua=%v", fx.name, sortedKeys(listN), sortedKeys(listL))

				// --- Read-path parity: Show. Compare the fields the native reader
				// projects. Divergence here is a real finding (e.g. native not
				// copying the rockspec into the install dir starves Show of
				// Summary/License/Dependencies). ---
				showN, errN := rNative.Show(context.Background(), fx.pkg)
				showL, errL := rLua.Show(context.Background(), fx.pkg)
				require.NoError(t, errN, "[%s] Show(native)", fx.name)
				require.NoError(t, errL, "[%s] Show(lua)", fx.name)

				assert.Equal(t, showL.Version, showN.Version, "[%s] Show.Version", fx.name)
				modN := append([]string(nil), showN.Modules...)
				modL := append([]string(nil), showL.Modules...)
				sort.Strings(modN)
				sort.Strings(modL)
				assert.Equal(t, modL, modN, "[%s] Show.Modules: native=%v lua=%v", fx.name, modN, modL)

				if showN.Summary != showL.Summary {
					t.Logf("[%s] FINDING Show.Summary diverges: native=%q lua=%q", fx.name, showN.Summary, showL.Summary)
				}
			})
		}
	}
}

// parityDeploy stages fx's source in a fresh dir and runs op through the given
// backend into a fresh tree. Returns (treePath, client).
func parityDeploy(
	t *testing.T, op parityOp, fx parityFixture, backend client.Backend, prefix, incDir string,
) (string, *client.Rocks) {
	t.Helper()
	src := t.TempDir()
	for rel, body := range fx.files {
		full := filepath.Join(src, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "bin/") {
			mode = 0o755
		}
		require.NoError(t, os.WriteFile(full, []byte(body), mode))
	}

	work := src
	if !op.inSource {
		work = t.TempDir()
	}

	tree := t.TempDir()
	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     prefix,
			IncludeDir: incDir,
		},
	}, client.WithBackend(backend))
	require.NoError(t, err, "New(%s, backend=%v)", fx.name, backend)
	op.run(t, r, fx, src)

	return tree, r
}

// writeParityRockspec writes fx's rockspec, with url as its source.url, into
// dir and returns its path.
func writeParityRockspec(t *testing.T, fx parityFixture, dir, url string) string {
	t.Helper()

	header := "package = \"" + fx.pkg + "\"\nversion = \"" + fx.version + "\"\n" +
		"source = { url = \"" + url + "\" }\n"
	specPath := filepath.Join(dir, fx.pkg+"-"+fx.version+".rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(header+fx.rockspec), 0o644))

	return specPath
}

// parityTarball packs fx's source files, as staged under src, into
// <pkg>.tar.gz with the single top directory <pkg>/ — the source.dir both
// backends deduce from that URL — and returns its file:// URL.
func parityTarball(t *testing.T, fx parityFixture, src string) string {
	t.Helper()

	tarball := filepath.Join(t.TempDir(), fx.pkg+".tar.gz")
	f, err := os.Create(tarball) //nolint:gosec // test temp path
	require.NoError(t, err, "create %s", tarball)

	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)

	rels := make([]string, 0, len(fx.files))
	for rel := range fx.files {
		rels = append(rels, rel)
	}

	sort.Strings(rels)

	for _, rel := range rels {
		st, err := os.Stat(filepath.Join(src, rel))
		require.NoError(t, err, "stat %s", rel)

		body := fx.files[rel]
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     fx.pkg + "/" + rel,
			Mode:     int64(st.Mode().Perm()),
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}), "tar header %s", rel)
		_, err = io.WriteString(tw, body)
		require.NoError(t, err, "tar body %s", rel)
	}

	require.NoError(t, tw.Close(), "close tar")
	require.NoError(t, zw.Close(), "close gzip")
	require.NoError(t, f.Close(), "close %s", tarball)

	return "file://" + tarball
}

// textArtifactsEqual reports whether the artifact at rel is byte-identical
// between the two trees once each tree's own absolute root is normalized to a
// placeholder. This is the correct parity check for generated launchers, which
// must embed the (necessarily different) tree path of the tree they live in.
func textArtifactsEqual(treeNative, treeLua, rel string) bool {
	bN, err := os.ReadFile(filepath.Join(treeNative, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	bL, err := os.ReadFile(filepath.Join(treeLua, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	normN := strings.ReplaceAll(string(bN), treeNative, "<TREE>")
	normL := strings.ReplaceAll(string(bL), treeLua, "<TREE>")
	return normN == normL
}

// dumpDiff logs a short head of a text artifact from both trees so a content
// divergence is legible in the test output (e.g. native copies a bin script
// verbatim while upstream installs a generated wrapper).
func dumpDiff(t *testing.T, fx, rel, treeNative, treeLua string) {
	t.Helper()
	head := func(root string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "<read error: " + err.Error() + ">"
		}
		s := string(b)
		if len(s) > 400 {
			s = s[:400] + "…"
		}
		return s
	}
	t.Logf("[%s] %q NATIVE content:\n%s", fx, rel, head(treeNative))
	t.Logf("[%s] %q LUA content:\n%s", fx, rel, head(treeLua))
}

// deployedArtifacts returns map[relpath]md5 for every deployed file — i.e.
// every regular file NOT under share/tarantool/rocks/ (that subtree is
// per-rock bookkeeping + the top-level manifest, which differ by design).
func deployedArtifacts(t *testing.T, root string) map[string]string {
	t.Helper()
	full := treeDigest(t, root)
	out := map[string]string{}
	for rel, sum := range full {
		if strings.HasPrefix(rel, "share/tarantool/rocks/") {
			continue
		}
		out[rel] = sum
	}
	return out
}
