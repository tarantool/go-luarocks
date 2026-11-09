//go:build integration_luaengine

package client_test

// Backend parity under hostile paths: the fixtures of every build type, made,
// built and installed by both backends into a Config.Tree, and from a
// Config.WorkingDir, whose path holds a space, a single quote, a dollar sign,
// non-ASCII letters or a component about 200 bytes long, or that is reached
// through a symlink. Every path a build runs with names the tree — the
// $(PREFIX), $(LUADIR), … of a make, cmake or command build, the include and
// output paths of a builtin compile, the package.path a command wrapper sets —
// so a path the shell or make reads specially shows a quoting bug in either
// backend. Both backends must end alike: both deploy the same tree, whose
// command wrappers run and print the same, or both fail.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
)

// hostilePath is one shape of path the trees and working dirs of a case live
// under.
type hostilePath struct {
	name string
	// component is the directory name the case's dirs are made under.
	component string
	// symlink reaches that directory through a symlink.
	symlink bool
}

func hostilePaths() []hostilePath {
	return []hostilePath{
		{name: "space", component: "with space"},
		{name: "quote", component: "it's"},
		{name: "dollar", component: "cost$HOME$(PREFIX)"},
		{name: "unicode", component: "дерево-木"}, //nolint:gosmopolitan // non-ASCII on purpose
		{name: "long", component: strings.Repeat("l", 200)},
		{name: "symlink", component: "real", symlink: true},
	}
}

// hostileBase returns a fresh directory whose path ends in p's component,
// reached through a symlink when p says so.
func hostileBase(t *testing.T, p hostilePath) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), p.component)
	require.NoError(t, os.Mkdir(dir, 0o755))

	if !p.symlink {
		return dir
	}

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))

	return link
}

// hostileFixtures are the fixtures of the hostile-path cases: one of each
// build type, each deploying a Lua module, a C module where the build type
// compiles one, and a Lua command requiring them, whose wrapper the case
// runs. Their build scripts quote every path they are given, as a rock that
// means to work under any tree does.
func hostileFixtures() []parityFixture {
	// cli is a command printing the name of each Lua module of mods and what
	// the answer function of each C module returns.
	cli := func(mods ...string) string {
		var b strings.Builder

		b.WriteString("#!/usr/bin/env tarantool\nlocal out = {}\n")

		for _, m := range mods {
			fmt.Fprintf(&b, "local m = require(%q)\ntable.insert(out, tostring(m.answer and m.answer() or m.name))\n", m)
		}

		b.WriteString("print(table.concat(out, ' '))\n")

		return b.String()
	}

	return []parityFixture{
		{
			name:    "builtin",
			pkg:     "hbuiltin",
			version: "1.0-1",
			files: map[string]string{
				"src/hbuiltin.lua": "return { name = 'hbuiltin' }\n",
				"src/core.c":       parityCModule("hbuiltin_core"),
				"bin/hbuiltin-cli": cli("hbuiltin", "hbuiltin.core"),
			},
			rockspec: `
build = {
   type = "builtin",
   modules = {
      ["hbuiltin"] = "src/hbuiltin.lua",
      ["hbuiltin.core"] = "src/core.c",
   },
   install = { bin = { ["hbuiltin-cli"] = "bin/hbuiltin-cli" } },
}
`,
		},
		{
			name:    "make",
			pkg:     "hmake",
			version: "1.0-1",
			files: map[string]string{
				"Makefile": "all: core.so\n\n" +
					"core.so: src/core.c\n" +
					"\t$(CC) $(CFLAGS) -I\"$(LUA_INCDIR)\" $(LIBFLAG) -o core.so src/core.c\n\n" +
					"install:\n" +
					"\tmkdir -p \"$(INST_LUADIR)\" \"$(INST_LIBDIR)/hmake\" \"$(INST_BINDIR)\"\n" +
					"\tcp src/hmake.lua \"$(INST_LUADIR)/hmake.lua\"\n" +
					"\tcp core.so \"$(INST_LIBDIR)/hmake/core.so\"\n" +
					"\tcp bin/hmake-cli \"$(INST_BINDIR)/hmake-cli\"\n",
				"src/hmake.lua": "return { name = 'hmake' }\n",
				"src/core.c":    parityCModule("hmake_core"),
				"bin/hmake-cli": cli("hmake", "hmake.core"),
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
			name:    "cmake",
			pkg:     "hcmake",
			version: "1.0-1",
			files: map[string]string{
				"CMakeLists.txt": `cmake_minimum_required(VERSION 3.5)
project(hcmake C)
include_directories("${LUA_INCDIR}")
add_library(hcmake_core MODULE src/core.c)
set_target_properties(hcmake_core PROPERTIES PREFIX "" SUFFIX ".so" OUTPUT_NAME "core")
install(TARGETS hcmake_core LIBRARY DESTINATION "${LIBDIR}/hcmake")
install(FILES src/hcmake.lua DESTINATION "${LUADIR}")
install(PROGRAMS bin/hcmake-cli DESTINATION bin)
`,
				"src/hcmake.lua": "return { name = 'hcmake' }\n",
				"src/core.c":     parityCModule("hcmake_core"),
				"bin/hcmake-cli": cli("hcmake", "hcmake.core"),
			},
			rockspec: `
build = {
   type = "cmake",
   variables = {
      CMAKE_INSTALL_PREFIX = "$(PREFIX)",
      LUA_INCDIR = "$(LUA_INCDIR)",
      LUADIR = "$(LUADIR)",
      LIBDIR = "$(LIBDIR)",
   },
}
`,
		},
		{
			name:    "command",
			pkg:     "hcmd",
			version: "1.0-1",
			files: map[string]string{
				"src/hcmd.lua.in": "return { name = 'hcmd' }\n",
				"bin/hcmd-cli":    cli("hcmd"),
			},
			rockspec: `
build = {
   type = "command",
   build_command = "cp src/hcmd.lua.in hcmd.lua",
   install_command = [[mkdir -p "$(LUADIR)" "$(BINDIR)" && cp hcmd.lua "$(LUADIR)/hcmd.lua" && cp bin/hcmd-cli "$(BINDIR)/hcmd-cli"]],
}
`,
		},
	}
}

// hostileOutputs is what the command of each hostile fixture prints.
var hostileOutputs = map[string]string{
	"builtin": "hbuiltin 42\n",
	"make":    "hmake 42\n",
	"cmake":   "hcmake 42\n",
	"command": "hcmd\n",
}

// hostileOutcome is how one backend ends a hostile-path case.
type hostileOutcome int

const (
	// hostileOK: the operation deploys the fixture, and its command prints
	// what hostileOutputs says.
	hostileOK hostileOutcome = iota
	// hostileFails: the operation fails.
	hostileFails
	// hostileNothing: the operation succeeds but deploys nothing into the
	// tree; its build installed the files somewhere else.
	hostileNothing
)

func (o hostileOutcome) String() string {
	return [...]string{"deploys and runs", "fails", "deploys nothing"}[o]
}

// The reasons the backends end a hostile-path case otherwise than both
// deploying it alike.
const (
	// hostileDollarExpanded: make expands a $ in the value of a variable
	// given on its command line, and sh one in a double-quoted word of a
	// build command, before a build script sees it: $(LUADIR) and the
	// others name a directory beside the tree, where both backends' builds
	// install, so neither deploys anything. Neither backend can quote that:
	// both pass the make variables as arguments of their own, quoted for sh
	// (build/make.lua through fs.execute), and make reads them as make does;
	// the command a command build runs and the recipes of a Makefile are the
	// rockspec's own shell code, into which the paths are substituted as text,
	// and they quote them for sh in double quotes, where a $ is expanded.
	hostileDollarExpanded = "make and sh expand the $ of the tree's path in $(LUADIR) and the rest, " +
		"so the build installs beside the tree, through either backend"
	// hostileCMakeDollar: the Makefiles CMake generates do not escape a $
	// of the source or build dir, so a CMake build in a dir whose path holds
	// one fails ("cd: can't cd to …"). Make builds in the working dir through
	// both backends, so both fail. The native Build and Install stage the
	// source in the working dir too and fail; upstream's stage it under
	// TMPDIR and deploy it, the tree's path reaching CMake as it is
	// (extra/build_cmake.lua, extra/util.lua).
	hostileCMakeDollar = "CMake's Makefiles do not escape a $ of the build dir, which the native backend " +
		"stages in Config.WorkingDir, and upstream's Make too; upstream's Build and Install stage under TMPDIR"
)

// hostileExpectation is how each backend ends a hostile-path case that does
// not end with both deploying it alike, and why.
type hostileExpectation struct {
	native, lua hostileOutcome
	why         string
}

// hostileExpected returns how each backend ends the case of fixture fx made
// by op under the path hp, when that is not both deploying it alike.
func hostileExpected(hp hostilePath, op parityOp, fx parityFixture) (hostileExpectation, bool) {
	switch {
	case hp.name == "dollar" && (fx.name == "make" || fx.name == "command"):
		return hostileExpectation{hostileNothing, hostileNothing, hostileDollarExpanded}, true
	case hp.name == "dollar" && fx.name == "cmake" && op.name == "make":
		return hostileExpectation{hostileFails, hostileFails, hostileCMakeDollar}, true
	case hp.name == "dollar" && fx.name == "cmake":
		return hostileExpectation{hostileFails, hostileOK, hostileCMakeDollar}, true
	default:
		return hostileExpectation{}, false
	}
}

// TestBackendParity_HostilePaths makes, builds and installs every hostile
// fixture through both backends under every hostile path and requires the
// two to end alike, or as hostileExpected says.
func TestBackendParity_HostilePaths(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, hp := range hostilePaths() {
		for _, op := range parityOps() {
			for _, fx := range hostileFixtures() {
				t.Run(hp.name+"/"+op.name+"/"+fx.name, func(t *testing.T) {
					t.Parallel()

					release := interopSlot()
					defer release()

					testHostile(t, hp, op, fx, prefix, incDir)
				})
			}
		}
	}
}

// hostileRun is how one backend ended a hostile-path case.
type hostileRun struct {
	tree string
	err  error
	// out is what each command printed, by name.
	out map[string]string
}

func testHostile(t *testing.T, hp hostilePath, op parityOp, fx parityFixture, prefix, incDir string) {
	t.Helper()

	runs := map[client.Backend]hostileRun{}

	for _, backend := range []client.Backend{client.BackendNative, client.BackendLua} {
		base := hostileBase(t, hp)
		run := hostileRun{tree: filepath.Join(base, "tree")}

		_, run.err = parityStepIn(t, op, fx, false, backend, run.tree, prefix, incDir, base)
		if run.err == nil {
			run.out = runWrappers(t, run.tree)
		} else {
			t.Logf("[%s] %v under %q: %v", fx.name, backend, hp.component, run.err)
		}

		runs[backend] = run
	}

	native, lua := runs[client.BackendNative], runs[client.BackendLua]

	if want, ok := hostileExpected(hp, op, fx); ok {
		t.Logf("[%s] known under %q: the native backend %v, the lua backend %v: %s", fx.name, hp.component,
			want.native, want.lua, want.why)
		checkHostileOutcome(t, fx, client.BackendNative, native, want.native)
		checkHostileOutcome(t, fx, client.BackendLua, lua, want.lua)

		return
	}

	checkHostileOutcome(t, fx, client.BackendNative, native, hostileOK)
	checkHostileOutcome(t, fx, client.BackendLua, lua, hostileOK)

	compareDeployed(t, fx.name, native.tree, lua.tree)
	checkWrapperTrees(t, op, native.tree, lua.tree)
	compareRockManifests(t, fx.name, native.tree, lua.tree, fx.pkg, fx.version)
	compareTreeManifests(t, fx.name, native.tree, lua.tree)
}

// checkHostileOutcome requires run, how backend ended the case of fx, to be
// want.
func checkHostileOutcome(t *testing.T, fx parityFixture, backend client.Backend, run hostileRun, want hostileOutcome) {
	t.Helper()

	label := fmt.Sprintf("[%s] the %v backend %v", fx.name, backend, want)
	if want == hostileFails {
		require.Error(t, run.err, label)

		return
	}

	require.NoError(t, run.err, label)

	cmd := fx.pkg + "-cli"

	if want == hostileNothing {
		var deployed map[string]string
		if _, err := os.Stat(run.tree); err == nil {
			deployed = deployedArtifacts(t, run.tree)
		}

		require.Empty(t, deployed, label)

		return
	}

	require.Contains(t, run.out, cmd, "%s; the tree holds %v", label, slices.Sorted(maps.Keys(treeEntries(t, run.tree))))
	require.Equal(t, hostileOutputs[fx.name], run.out[cmd], label)
}

// runWrappers runs every command of tree with no arguments and returns what
// each printed, stdout and stderr, and its exit status when it failed, with
// tree's root normalized, by name.
func runWrappers(t *testing.T, tree string) map[string]string {
	t.Helper()

	bins, err := os.ReadDir(filepath.Join(tree, "bin"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	require.NoError(t, err)

	out := map[string]string{}

	for _, b := range bins {
		var buf bytes.Buffer

		cmd := exec.CommandContext(context.Background(), filepath.Join(tree, "bin", b.Name()))
		cmd.Stdout, cmd.Stderr = &buf, &buf
		cmd.Dir = t.TempDir()

		if err := cmd.Run(); err != nil {
			buf.WriteString("exit: " + err.Error() + "\n")
		}

		out[b.Name()] = strings.ReplaceAll(buf.String(), tree, "<TREE>")
	}

	return out
}
