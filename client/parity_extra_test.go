//go:build integration_luaengine

package client_test

// Broader backend-parity harness (companion to parity_test.go). Where
// parity_test.go pins ONE pure-Lua fixture, this table-drives several
// rockspec shapes through BOTH backends (native + gopher-lua) into separate
// trees and diffs them:
//
//   - install-sections  : build.install.lua + build.install.bin
//   - copy-directories  : build.copy_directories (a doc/ dir)
//   - c-module          : a builtin C module compiled to a .so
//   - init-module       : a build.modules entry whose source is an init.lua
//   - bin-scripts       : build.install.bin with Lua (wrapped) and shell
//     (copied) scripts, and a key with a directory part
//   - conf-files        : build.install.conf
//   - make-rock         : a Makefile installing Lua and C modules and
//     commands into $(LUADIR), $(LIBDIR) and $(BINDIR)
//   - cmake-rock        : the same through CMake install rules, absolute and
//     relative to CMAKE_INSTALL_PREFIX
//   - command-rock      : an install_command copying into the same dirs
//   - make-modules, cmake-modules, command-modules : build.modules listed by
//     a non-builtin rock, which upstream ignores
//   - default-docs      : README/LICENSE/*.md copied with no doc/ dir
//   - implicit-doc-dir  : a doc/ dir copied with no copy_directories
//   - copy-other-dirs   : a copy_directories entry other than doc/
//   - make-confdir      : a Makefile installing into $(CONFDIR), $(DOCDIR)
//   - cmake-prefix-dirs : CMake installing doc/ and conf/ under its prefix
//
// Trees holding more than one rock — versions of one rock, rocks sharing a
// module — are covered by parity_versions_test.go.
//
// Each fixture goes through every way of building a rockspec from a source
// tree (parityOps): Make over the staged source, Build of the rockspec, which
// fetches its source.url first, and Install of the rock from a directory
// server listing the rockspec, which builds it the way Build does. The
// command wrappers an Install writes differ by design (divWrapperTrees): the
// lua backend's, upstream's, list the Tarantool prefix's tree after the
// target one, for the --deps-mode every DepsPolicy maps to, where the native
// one lists the target tree alone. checkWrapperTrees checks each side, and
// the wrappers are compared without the prefix's tree.
//
// The parity contract (same as parity_test.go): DEPLOYED ARTIFACTS must be
// byte-equal. A deployed artifact is any file NOT under
// share/tarantool/rocks/ (that subtree is per-rock bookkeeping + the
// top-level manifest, whose formatting legitimately differs between engines).
// What that bookkeeping records — each rock's rock_manifest and the tree
// manifest — must agree in content (parity_manifest_test.go). Read-path
// projections (List/Show) are also compared.
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
		{
			// Every kind of command script build.install.bin takes: Lua with
			// and without a #! line (wrapped), a shell script (copied, since
			// it does not parse as Lua), and a key with a directory part.
			name:    "bin-scripts",
			pkg:     "withbins",
			version: "1.0-1",
			files: map[string]string{
				"src/withbins.lua":  "return {}\n",
				"bin/lua-shebang":   "#!/usr/bin/env tarantool\nprint('shebang')\n",
				"bin/lua-plain":     "print('plain')\n",
				"bin/sh-script":     "#!/bin/sh\necho shell\n",
				"bin/nested-script": "print('nested')\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withbins"] = "src/withbins.lua",
   },
   install = {
      bin = {
         ["lua-shebang"] = "bin/lua-shebang",
         ["lua-plain"] = "bin/lua-plain",
         ["sh-script"] = "bin/sh-script",
         ["tools/nested"] = "bin/nested-script",
      },
   },
}
`,
		},
		{
			// build.install.conf lands in the per-rock conf dir only, so it is
			// compared through the rock_manifest.
			name:    "conf-files",
			pkg:     "withconf",
			version: "1.0-1",
			files: map[string]string{
				"src/withconf.lua":   "return {}\n",
				"etc/withconf.cfg":   "answer = 42\n",
				"etc/extra/more.cfg": "more = true\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withconf"] = "src/withconf.lua",
   },
   install = {
      conf = {
         ["withconf.cfg"] = "etc/withconf.cfg",
         ["sub/more.cfg"] = "etc/extra/more.cfg",
      },
   },
}
`,
		},
		{
			// A make rock whose Makefile compiles a C module and installs
			// it, two Lua modules and a command into the directories the
			// rockspec passes it.
			name:    "make-rock",
			pkg:     "makerock",
			version: "1.0-1",
			files: map[string]string{
				"Makefile":          parityMakefile,
				"src/makerock.lua":  "return { name = 'makerock' }\n",
				"src/util.lua":      "return { util = true }\n",
				"src/core.c":        parityCModule("makerock_core"),
				"bin/makerock-cli":  "#!/usr/bin/env tarantool\nprint('makerock')\n",
				"bin/makerock-tool": "#!/bin/sh\necho makerock\n",
			},
			rockspec: `
build = {
   type = "make",
   build_variables = {
      CFLAGS = "$(CFLAGS)",
      LIBFLAG = "$(LIBFLAG)",
      LUA_INCDIR = "$(LUA_INCDIR)",
   },
   install_variables = {
      INST_LUADIR = "$(LUADIR)",
      INST_LIBDIR = "$(LIBDIR)",
      INST_BINDIR = "$(BINDIR)",
   },
}
`,
		},
		{
			// A cmake rock installing a C module, Lua modules and commands,
			// half through the absolute directories the rockspec passes and
			// half relative to CMAKE_INSTALL_PREFIX.
			name:    "cmake-rock",
			pkg:     "cmakerock",
			version: "1.0-1",
			files: map[string]string{
				"CMakeLists.txt":     parityCMakeLists,
				"src/cmakerock.lua":  "return { name = 'cmakerock' }\n",
				"src/util.lua":       "return { util = true }\n",
				"src/core.c":         parityCModule("cmakerock_core"),
				"bin/cmakerock-cli":  "#!/usr/bin/env tarantool\nprint('cmakerock')\n",
				"bin/cmakerock-tool": "#!/bin/sh\necho cmakerock\n",
			},
			rockspec: `
build = {
   type = "cmake",
   variables = {
      CMAKE_INSTALL_PREFIX = "$(PREFIX)",
      CMAKE_BUILD_TYPE = "Release",
      LUA_INCDIR = "$(LUA_INCDIR)",
      LUADIR = "$(LUADIR)",
      LIBDIR = "$(LIBDIR)",
      BINDIR = "$(BINDIR)",
   },
}
`,
		},
		{
			// A command rock whose install_command copies a generated Lua
			// module and a command into the rock's directories.
			name:    "command-rock",
			pkg:     "cmdrock",
			version: "1.0-1",
			files: map[string]string{
				"src/cmdrock.lua.in": "return { name = 'cmdrock' }\n",
				"bin/cmdrock-cli":    "print('cmdrock')\n",
			},
			rockspec: `
build = {
   type = "command",
   build_command = "cp src/cmdrock.lua.in cmdrock.lua",
   install_command = [[mkdir -p "$(LUADIR)" "$(BINDIR)" && cp cmdrock.lua "$(LUADIR)/cmdrock.lua" && cp bin/cmdrock-cli "$(BINDIR)/cmdrock-cli"]],
}
`,
		},
		{
			// build.modules belongs to the builtin backend: upstream's make
			// backend never reads it, so a make rock that also lists modules
			// deploys only what its Makefile installs.
			name:    "make-modules",
			pkg:     "makemods",
			version: "1.0-1",
			files: map[string]string{
				"Makefile":         "all:\n\ninstall:\n\tmkdir -p $(INST_LUADIR)\n\tcp src/makemods.lua $(INST_LUADIR)/makemods.lua\n",
				"src/makemods.lua": "return {}\n",
				"src/listed.lua":   "return { listed = true }\n",
			},
			rockspec: `
build = {
   type = "make",
   install_variables = { INST_LUADIR = "$(LUADIR)" },
   modules = {
      ["makemods.listed"] = "src/listed.lua",
   },
}
`,
		},
		{
			// The same for the cmake backend.
			name:    "cmake-modules",
			pkg:     "cmakemods",
			version: "1.0-1",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.5)\nproject(cmakemods NONE)\n" +
					"install(FILES src/cmakemods.lua DESTINATION ${LUADIR})\n",
				"src/cmakemods.lua": "return {}\n",
				"src/listed.lua":    "return { listed = true }\n",
			},
			rockspec: `
build = {
   type = "cmake",
   variables = {
      CMAKE_INSTALL_PREFIX = "$(PREFIX)",
      LUADIR = "$(LUADIR)",
   },
   modules = {
      ["cmakemods.listed"] = "src/listed.lua",
   },
}
`,
		},
		{
			// And for the command backend.
			name:    "command-modules",
			pkg:     "cmdmods",
			version: "1.0-1",
			files: map[string]string{
				"src/cmdmods.lua": "return {}\n",
				"src/listed.lua":  "return { listed = true }\n",
			},
			rockspec: `
build = {
   type = "command",
   install_command = [[mkdir -p "$(LUADIR)" && cp src/cmdmods.lua "$(LUADIR)/cmdmods.lua"]],
   modules = {
      ["cmdmods.listed"] = "src/listed.lua",
   },
}
`,
		},
		{
			// With no copy_directories and no doc/ directory, upstream copies
			// the README, LICENSE and *.md files of the source root into the
			// per-rock doc dir (build.lua install_default_docs).
			name:    "default-docs",
			pkg:     "withreadme",
			version: "1.0-1",
			files: map[string]string{
				"src/withreadme.lua": "return {}\n",
				"README.md":          "# withreadme\n",
				"LICENSE":            "MIT\n",
				"CHANGES.md":         "1.0: first\n",
				"notes.txt":          "not a doc\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withreadme"] = "src/withreadme.lua",
   },
}
`,
		},
		{
			// With no copy_directories, upstream copies a doc/ directory by
			// default (build.lua install_files).
			name:    "implicit-doc-dir",
			pkg:     "withdocdir",
			version: "1.0-1",
			files: map[string]string{
				"src/withdocdir.lua": "return {}\n",
				"doc/index.md":       "# withdocdir\n",
				"README.md":          "# not copied: doc/ exists\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withdocdir"] = "src/withdocdir.lua",
   },
}
`,
		},
		{
			// A copy_directories entry other than doc/: copied into the
			// per-rock dir and listed in the rock_manifest like any file
			// there, and it suppresses the default docs.
			name:    "copy-other-dirs",
			pkg:     "withsamples",
			version: "1.0-1",
			files: map[string]string{
				"src/withsamples.lua":     "return {}\n",
				"samples/demo.lua":        "print('demo')\n",
				"samples/nested/more.txt": "more\n",
				"README.md":               "# not copied: a directory was\n",
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["withsamples"] = "src/withsamples.lua",
   },
   copy_directories = { "samples" },
}
`,
		},
		{
			// A make rock installing into $(CONFDIR) and $(DOCDIR), which
			// name per-rock directories (path.configure_paths).
			name:    "make-confdir",
			pkg:     "makeconf",
			version: "1.0-1",
			files: map[string]string{
				"Makefile": "all:\n\ninstall:\n" +
					"\tmkdir -p $(INST_LUADIR) $(INST_CONFDIR) $(INST_DOCDIR)\n" +
					"\tcp src/makeconf.lua $(INST_LUADIR)/makeconf.lua\n" +
					"\tcp etc/makeconf.cfg $(INST_CONFDIR)/makeconf.cfg\n" +
					"\tcp README.md $(INST_DOCDIR)/README.md\n",
				"src/makeconf.lua":   "return {}\n",
				"etc/makeconf.cfg":   "answer = 42\n",
				"README.md":          "# makeconf\n",
				"doc/not-copied.txt": "make rocks copy doc/ only if asked\n",
			},
			rockspec: `
build = {
   type = "make",
   install_variables = {
      INST_LUADIR = "$(LUADIR)",
      INST_CONFDIR = "$(CONFDIR)",
      INST_DOCDIR = "$(DOCDIR)",
   },
   copy_directories = {},
}
`,
		},
		{
			// A cmake rock installing files relative to CMAKE_INSTALL_PREFIX
			// outside lua/, lib/ and bin/.
			name:    "cmake-prefix-dirs",
			pkg:     "cmakeprefix",
			version: "1.0-1",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.5)\nproject(cmakeprefix NONE)\n" +
					"install(FILES src/cmakeprefix.lua DESTINATION lua)\n" +
					"install(FILES README.md DESTINATION doc)\n" +
					"install(FILES etc/cmakeprefix.cfg DESTINATION conf)\n",
				"src/cmakeprefix.lua": "return {}\n",
				"README.md":           "# cmakeprefix\n",
				"etc/cmakeprefix.cfg": "answer = 42\n",
				"doc/also-copied.txt": "copied by the default copy_directories\n",
			},
			rockspec: `
build = {
   type = "cmake",
   variables = {
      CMAKE_INSTALL_PREFIX = "$(PREFIX)",
   },
}
`,
		},
	}
}

// parityMakefile builds src/core.c into core.so and installs it, two Lua
// modules and two commands into the INST_* directories the make-rock fixture
// passes. Recipe lines start with a tab, as make requires.
const parityMakefile = "all: core.so\n\n" +
	"core.so: src/core.c\n" +
	"\t$(CC) $(CFLAGS) -I$(LUA_INCDIR) $(LIBFLAG) -o core.so src/core.c\n\n" +
	"install:\n" +
	"\tmkdir -p $(INST_LUADIR)/makerock $(INST_LIBDIR)/makerock $(INST_BINDIR)\n" +
	"\tcp src/makerock.lua $(INST_LUADIR)/makerock.lua\n" +
	"\tcp src/util.lua $(INST_LUADIR)/makerock/util.lua\n" +
	"\tcp core.so $(INST_LIBDIR)/makerock/core.so\n" +
	"\tcp bin/makerock-cli bin/makerock-tool $(INST_BINDIR)/\n"

// parityCMakeLists is the CMakeLists.txt of the cmake-rock fixture. The C
// module, cmakerock.lua and cmakerock-cli go to the absolute LIBDIR, LUADIR
// and BINDIR the rockspec passes; util.lua and cmakerock-tool go to lua/ and
// bin/ relative to CMAKE_INSTALL_PREFIX, which the rockspec sets to $(PREFIX).
const parityCMakeLists = `cmake_minimum_required(VERSION 3.5)
project(cmakerock C)
include_directories(${LUA_INCDIR})
add_library(cmakerock_core MODULE src/core.c)
set_target_properties(cmakerock_core PROPERTIES PREFIX "" SUFFIX ".so" OUTPUT_NAME "core")
install(TARGETS cmakerock_core LIBRARY DESTINATION ${LIBDIR}/cmakerock)
install(FILES src/cmakerock.lua DESTINATION ${LUADIR})
install(FILES src/util.lua DESTINATION lua/cmakerock)
install(PROGRAMS bin/cmakerock-cli DESTINATION ${BINDIR})
install(PROGRAMS bin/cmakerock-tool DESTINATION bin)
`

// parityCModule returns the source of a minimal Lua/C module whose
// luaopen_<sym> returns a table with one function.
func parityCModule(sym string) string {
	return `#include <lua.h>
#include <lauxlib.h>

static int l_answer(lua_State *L) {
    lua_pushinteger(L, 42);
    return 1;
}

int luaopen_` + sym + `(lua_State *L) {
    lua_newtable(L);
    lua_pushcfunction(L, l_answer);
    lua_setfield(L, -2, "answer");
    return 1;
}
`
}

// parityOp is one way of getting a fixture deployed: it stages fx as the
// operation needs it and runs the operation on r.
type parityOp struct {
	name string
	// inSource runs the operation with the staged source as the working
	// directory, as Make needs. The others fetch the source themselves and
	// get an empty working directory, so they cannot pick the staged files up.
	inSource bool
	// run runs the operation; keep is BuildOpts.Keep for Build, which the
	// lua backend passes as --keep to keep the other installed versions of
	// the rock (the native one keeps them anyway, and only its staging dir
	// with it). Make and Install have no Keep and ignore it.
	run func(t *testing.T, r *client.Rocks, fx parityFixture, src string, keep bool)
}

// parityOps are the operations TestBackendParity_Fixtures runs every fixture
// through. Each one builds the rockspec from source and deploys the result,
// so they must deploy the same artifacts; they differ in where the source and
// the rockspec come from.
func parityOps() []parityOp {
	return []parityOp{
		{"make", true, func(t *testing.T, r *client.Rocks, fx parityFixture, src string, keep bool) {
			t.Helper()
			specPath := writeParityRockspec(t, fx, src, "file://localhost/"+fx.pkg+".tar.gz")
			require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}),
				"Make(%s)", fx.name)
		}},
		{"build", false, func(t *testing.T, r *client.Rocks, fx parityFixture, src string, keep bool) {
			t.Helper()
			specPath := writeParityRockspec(t, fx, t.TempDir(), parityTarball(t, fx, src))
			require.NoError(t, r.Build(context.Background(), specPath, client.BuildOpts{Keep: keep}),
				"Build(%s)", fx.name)
		}},
		{"install", false, func(t *testing.T, r *client.Rocks, fx parityFixture, src string, keep bool) {
			t.Helper()
			repo := t.TempDir()
			writeParityRockspec(t, fx, repo, parityTarball(t, fx, src))
			manifest := "commands = {}\nmodules = {}\nrepository = {\n   [\"" + fx.pkg + "\"] = { [\"" + fx.version +
				"\"] = { { arch = \"rockspec\" } } },\n}\n"
			require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(manifest), 0o600))
			require.NoError(t, r.Install(context.Background(), fx.pkg, client.InstallOpts{
				Version: fx.version,
				Servers: []string{repo},
			}), "Install(%s)", fx.name)
		}},
	}
}

// TestBackendParity_Fixtures table-drives several rockspec shapes through both
// backends and diffs deployed artifacts + read-path projections.
func TestBackendParity_Fixtures(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, op := range parityOps() {
		for _, fx := range parityFixtures() {
			t.Run(op.name+"/"+fx.name, func(t *testing.T) {
				treeNative, rNative := parityDeploy(t, op, fx, client.BackendNative, prefix, incDir)
				treeLua, rLua := parityDeploy(t, op, fx, client.BackendLua, prefix, incDir)

				// --- Deployed-artifact parity (the hard contract). ---
				compareDeployed(t, fx.name, treeNative, treeLua)
				checkWrapperTrees(t, op, treeNative, treeLua)

				// --- Read-path parity: List. ---
				listN := installedSet(t, rNative)
				listL := installedSet(t, rLua)
				assert.True(t, equalStringSets(listN, listL),
					"[%s] List parity: native=%v lua=%v", fx.name, sortedKeys(listN), sortedKeys(listL))

				// --- Read-path parity: Show. Compare the fields the native reader
				// projects from the installed rockspec and the manifest. ---
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

				assert.Equal(t, showL.Summary, showN.Summary, "[%s] Show.Summary", fx.name)

				// --- Manifest parity: same contents, whatever the format. ---
				compareRockManifests(t, fx.name, treeNative, treeLua, fx.pkg, fx.version)
				compareTreeManifests(t, fx.name, treeNative, treeLua)
			})
		}
	}
}

// parityTarantool locates the real tarantool the lua backend shells out to
// and returns its prefix and the directory holding its lua.h, ending the test
// through testenv.Missingf when either is missing.
func parityTarantool(t *testing.T) (prefix, incDir string) {
	t.Helper()

	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		testenv.Missingf(t, "tarantool not found on PATH; backend-parity needs a real interpreter: %v", err)
	}
	prefix = filepath.Dir(filepath.Dir(ttBin))
	incDir = filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		testenv.Missingf(t, "tarantool lua.h not found at %s; backend-parity needs Lua headers: %v", incDir, err)
	}

	return prefix, incDir
}

// parityDeploy stages fx's source in a fresh dir and runs op through the given
// backend into a fresh tree. Returns (treePath, client).
func parityDeploy(
	t *testing.T, op parityOp, fx parityFixture, backend client.Backend, prefix, incDir string,
) (string, *client.Rocks) {
	t.Helper()

	tree := t.TempDir()
	r := parityStep(t, op, fx, false, backend, tree, prefix, incDir)

	return tree, r
}

// parityStep stages fx's source in a fresh dir and runs op, with keep as its
// Keep option, through the given backend into tree, which may already hold
// rocks. Returns the client it ran op with.
func parityStep(
	t *testing.T, op parityOp, fx parityFixture, keep bool, backend client.Backend, tree, prefix, incDir string,
) *client.Rocks {
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

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     prefix,
			IncludeDir: incDir,
		},
	}, client.WithBackend(backend))
	require.NoError(t, err, "New(%s, backend=%v)", fx.name, backend)
	op.run(t, r, fx, src, keep)

	return r
}

// compareDeployed requires every deployed artifact — every file outside
// share/tarantool/rocks/ — to be present in both trees with the same bytes.
// A compiled .so is compared by presence only, and a generated text artifact
// may differ in the absolute root of the tree it was generated for.
func compareDeployed(tb testing.TB, label, treeNative, treeLua string) {
	tb.Helper()

	depN := deployedArtifacts(tb, treeNative)
	depL := deployedArtifacts(tb, treeLua)

	require.NotEmpty(tb, depN, "native deployed no artifacts for %s", label)
	require.NotEmpty(tb, depL, "lua deployed no artifacts for %s", label)

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
			tb.Errorf("[%s] deployed artifact %q present in LUA tree but MISSING in native", label, rel)
		case !inL:
			tb.Errorf("[%s] deployed artifact %q present in NATIVE tree but MISSING in lua", label, rel)
		case dN != dL:
			// Compiled objects (.so) are not expected to be byte-reproducible
			// across two independent compilations (embedded paths, debug info,
			// ordering). For those the parity contract is presence, not bytes.
			if strings.HasSuffix(rel, ".so") {
				tb.Logf("[%s] note: compiled %q differs byte-wise (expected; non-reproducible build): native md5=%s lua md5=%s",
					label, rel, dN, dL)
				continue
			}
			// Each backend installs into its OWN tree, so generated text
			// artifacts (e.g. bin wrappers) legitimately embed that tree's
			// absolute root. Normalize each tree root to a placeholder before
			// comparing — equal-after-normalization is true parity.
			if textArtifactsEqual(treeNative, treeLua, rel) {
				tb.Logf("[%s] %q matches after tree-root normalization (only the per-tree absolute path differs)", label, rel)
				continue
			}
			tb.Errorf("[%s] ARTIFACT DIVERGENCE %q: native md5=%s lua md5=%s", label, rel, dN, dL)
			dumpDiff(tb, label, rel, treeNative, treeLua)
		}
	}
	tb.Logf("[%s] deployed artifacts compared: %v", label, sortedKeys(allRel))
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
// A command wrapper is compared without the Tarantool prefix's tree, which
// only upstream install lists (divWrapperTrees).
func textArtifactsEqual(treeNative, treeLua, rel string) bool {
	bN, err := os.ReadFile(filepath.Join(treeNative, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	bL, err := os.ReadFile(filepath.Join(treeLua, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	normN := strings.ReplaceAll(dropPrefixTree(string(bN)), treeNative, "<TREE>")
	normL := strings.ReplaceAll(dropPrefixTree(string(bL)), treeLua, "<TREE>")
	return normN == normL
}

// dumpDiff logs a short head of a text artifact from both trees so a content
// divergence is legible in the test output (e.g. native copies a bin script
// verbatim while upstream installs a generated wrapper).
func dumpDiff(tb testing.TB, fx, rel, treeNative, treeLua string) {
	tb.Helper()
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
	tb.Logf("[%s] %q NATIVE content:\n%s", fx, rel, head(treeNative))
	tb.Logf("[%s] %q LUA content:\n%s", fx, rel, head(treeLua))
}

// deployedArtifacts returns map[relpath]md5 for every deployed file — i.e.
// every regular file NOT under share/tarantool/rocks/ (that subtree is
// per-rock bookkeeping + the top-level manifest, which differ by design).
func deployedArtifacts(tb testing.TB, root string) map[string]string {
	tb.Helper()
	full := treeDigest(tb, root)
	out := map[string]string{}
	for rel, sum := range full {
		if strings.HasPrefix(rel, "share/tarantool/rocks/") {
			continue
		}
		out[rel] = sum
	}
	return out
}
