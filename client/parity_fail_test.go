//go:build integration_luaengine

package client_test

// Failure parity harness. Where the other parity suites compare what two
// successful runs leave, this one injects a failure into a build or a deploy
// and compares what the failed runs leave: the same tree, set up by the
// native backend, is given to each backend, the same failing operation runs
// through both, and both must fail and leave the tree exactly as it was
// before — every file, mode and symlink, the rock_manifests and the tree
// manifest — as upstream LuaRocks rolls a failed build, install or deploy
// back (build.lua, cmd/install.lua, repos.deploy_files). Tree.Check must find
// nothing wrong with any tree afterwards.
//
// The one thing upstream keeps is the removal of an installed copy of the
// very version it builds, which it makes for good before it builds: a failed
// reinstall leaves both trees without the version, and the two are compared
// with each other instead (failScenario.removes).
//
// Two injections run through the native backend alone, since the lua backend
// cannot be stopped or starved the same way: a context cancelled while the
// build runs, and a read-only directory of the tree the deploy must write
// into.
//
// Build-tagged off by default; run with `-tags integration_luaengine`.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// failBasis is a pure-Lua rock every failure scenario starts from: it
// provides the module shared, which the failing rocks provide too and, of a
// name sorting first, take the plain path of, and a command.
var failBasis = parityFixture{
	name:    "basis",
	pkg:     "basis",
	version: "1.0-1",
	files: map[string]string{
		"shared.lua":     "return 'basis shared'\n",
		"basis.lua":      "return 'basis'\n",
		"bin/basis-tool": "print('basis tool')\n",
	},
	rockspec: `
build = {
   type = "builtin",
   modules = { shared = "shared.lua", basis = "basis.lua" },
   install = { bin = { ["basis-tool"] = "bin/basis-tool" } },
}
`,
}

// failBlocker is a make rock that installs the module blocker as a file of
// that very name, with no extension, where a module blocker.sub needs a
// directory.
var failBlocker = parityFixture{
	name:    "blocker",
	pkg:     "blocker",
	version: "1.0-1",
	files: map[string]string{
		"Makefile": "all:\n\ninstall:\n\tmkdir -p $(INST_LUADIR)\n\tcp blocker $(INST_LUADIR)/blocker\n",
		"blocker":  "return 'a module file with no extension'\n",
	},
	rockspec: `
build = {
   type = "make",
   install_variables = { INST_LUADIR = "$(LUADIR)" },
}
`,
}

// failLate is a rock, named to sort first, whose build succeeds and whose
// deploy then fails: its make installs a module into $(LUADIR), its
// build.install.lua takes the plain path of shared from basis, and its
// build.install.bin names a script the sources lack.
func failLate(pkg, version string) parityFixture {
	return parityFixture{
		name:    pkg + "-late-" + version,
		pkg:     pkg,
		version: version,
		files: map[string]string{
			"Makefile":   "all:\n\ninstall:\n\tmkdir -p $(INST_LUADIR)\n\tcp own.lua $(INST_LUADIR)/" + pkg + ".lua\n",
			"own.lua":    "return '" + pkg + " " + version + "'\n",
			"shared.lua": "return '" + pkg + " shared " + version + "'\n",
		},
		rockspec: `
build = {
   type = "make",
   install_variables = { INST_LUADIR = "$(LUADIR)" },
   install = {
      lua = { shared = "shared.lua" },
      bin = { ["` + pkg + `-tool"] = "bin/missing" },
   },
}
`,
	}
}

// failUpgradeBase is the version of the rock aupg every upgrade and
// reinstall scenario installs first: it holds the plain path of shared, a
// module of its own and a command.
var failUpgradeBase = parityFixture{
	name:    "aupg-1.0",
	pkg:     "aupg",
	version: "1.0-1",
	files: map[string]string{
		"shared.lua":    "return 'aupg shared 1.0'\n",
		"aupg.lua":      "return 'aupg 1.0'\n",
		"bin/aupg-tool": "print('aupg tool 1.0')\n",
	},
	rockspec: `
build = {
   type = "builtin",
   modules = { shared = "shared.lua", aupg = "aupg.lua" },
   install = { bin = { ["aupg-tool"] = "bin/aupg-tool" } },
}
`,
}

// failScenario is one failing operation and the tree it fails in.
type failScenario struct {
	name string
	// setup lists the rocks the native backend makes into the tree first, in
	// order.
	setup []parityFixture
	// fail is the rock whose build or deploy fails.
	fail parityFixture
	// removes is set for a reinstall: the version installed that both
	// backends remove for good before they build, as upstream does.
	removes bool
	// nativeErr and luaErr are substrings each backend's error must carry,
	// telling the kind of failure.
	nativeErr, luaErr string
}

// failScenarios are the failures both backends run.
func failScenarios() []failScenario {
	return []failScenario{
		{
			// (a) A builtin C module that does not compile.
			name:  "c-syntax-error",
			setup: []parityFixture{failBasis},
			fail: parityFixture{
				name: "cbroken", pkg: "abroken", version: "1.0-1",
				files: map[string]string{
					"abroken.c":  "int luaopen_abroken(void *L) { return 0 this is not C; }\n",
					"shared.lua": "return 'abroken shared'\n",
				},
				rockspec: `
build = {
   type = "builtin",
   modules = { shared = "shared.lua", abroken = "abroken.c" },
}
`,
			},
			nativeErr: "abroken.c", luaErr: "Failed compiling",
		},
		{
			// (b) A make rock whose install writes into $(LUADIR), then fails.
			name:  "make-install-fails",
			setup: []parityFixture{failBasis},
			fail: parityFixture{
				name: "mkfail", pkg: "amkfail", version: "1.0-1",
				files: map[string]string{
					"Makefile": "all:\n\ninstall:\n\tmkdir -p $(INST_LUADIR)\n" +
						"\tcp shared.lua $(INST_LUADIR)/shared.lua\n\texit 1\n",
					"shared.lua": "return 'amkfail shared'\n",
				},
				rockspec: `
build = {
   type = "make",
   install_variables = { INST_LUADIR = "$(LUADIR)" },
}
`,
			},
			nativeErr: "make", luaErr: "Failed installing",
		},
		{
			// (c) A command script the rockspec names is missing: the native
			// deploy fails after it deployed the modules.
			name:      "bin-missing",
			setup:     []parityFixture{failBasis},
			fail:      failLate("alate", "1.0-1"),
			nativeErr: "missing", luaErr: "missing",
		},
		{
			// (d) A regular file where a module directory must go.
			name:  "file-for-dir",
			setup: []parityFixture{failBasis, failBlocker},
			fail: parityFixture{
				name: "blocked", pkg: "ablocked", version: "1.0-1",
				files: map[string]string{
					"shared.lua": "return 'ablocked shared'\n",
					"sub.lua":    "return 'ablocked sub'\n",
				},
				rockspec: `
build = {
   type = "builtin",
   modules = { shared = "shared.lua", ["blocker.sub"] = "sub.lua" },
}
`,
			},
			nativeErr: "share/tarantool/blocker", luaErr: "failed making directory",
		},
		{
			// (e) An upgrade, 1.0 installed, whose 2.0 fails to deploy.
			name:      "upgrade",
			setup:     []parityFixture{failBasis, failUpgradeBase},
			fail:      failLate("aupg", "2.0-1"),
			nativeErr: "missing", luaErr: "missing",
		},
		{
			// (e) A reinstall of the version installed that fails to deploy.
			name:      "reinstall",
			setup:     []parityFixture{failBasis, failUpgradeBase},
			fail:      failLate("aupg", "1.0-1"),
			removes:   true,
			nativeErr: "missing", luaErr: "missing",
		},
	}
}

// The operations a failing rock runs through: Make over its sources and
// Install from a directory server listing its rockspec, which builds it as
// Build does.
const (
	failMake    = "make"
	failInstall = "install"
)

// failOps are the operations every failure scenario runs through.
func failOps() []string {
	return []string{failMake, failInstall}
}

// TestBackendParity_Failures runs each failure scenario through both
// backends, on two trees set up alike, and requires both to fail and to
// leave their trees as they were.
func TestBackendParity_Failures(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, op := range failOps() {
		for _, sc := range failScenarios() {
			t.Run(op+"/"+sc.name, func(t *testing.T) {
				label := op + "/" + sc.name

				treeNative := failSetup(t, sc.setup, prefix, incDir)
				treeLua := failSetup(t, sc.setup, prefix, incDir)
				beforeNative := snapshotTree(t, treeNative)
				beforeLua := snapshotTree(t, treeLua)

				errNative := runFailing(t, context.Background(), op, client.BackendNative, treeNative, sc.fail, prefix, incDir)
				errLua := runFailing(t, context.Background(), op, client.BackendLua, treeLua, sc.fail, prefix, incDir)

				require.Error(t, errNative, "[%s] native", label)
				require.Error(t, errLua, "[%s] lua", label)
				t.Logf("[%s] native: %v", label, errNative)
				t.Logf("[%s] lua: %v", label, errLua)
				assert.Contains(t, errNative.Error(), sc.nativeErr, "[%s] the kind of the native failure", label)
				assert.Contains(t, errLua.Error(), sc.luaErr, "[%s] the kind of the lua failure", label)

				requireTreeClean(t, label+" native", failCfg(treeNative, prefix, incDir))
				requireTreeClean(t, label+" lua", failCfg(treeLua, prefix, incDir))

				if !sc.removes {
					assert.Equal(t, beforeNative, snapshotTree(t, treeNative), "[%s] the native tree after the failure", label)
					assert.Equal(t, beforeLua, snapshotTree(t, treeLua), "[%s] the lua tree after the failure", label)

					return
				}

				// Both removed the version for good before they built, and
				// undid the rest.
				for _, root := range []string{treeNative, treeLua} {
					assert.NotContains(t, installedVersions(t, root), [2]string{sc.fail.pkg, sc.fail.version},
						"[%s] the reinstalled version in %s", label, root)
					assert.NoDirExists(t, filepath.Join(root, "share", "tarantool", "rocks", sc.fail.pkg))
				}

				assert.Equal(t, installedVersions(t, treeLua), installedVersions(t, treeNative), "[%s] the rocks left", label)
				compareDeployed(t, label, treeNative, treeLua)
				compareTreeManifests(t, label, treeNative, treeLua)
			})
		}
	}
}

// TestBackendParity_Failures_Cancelled cancels the context of a native
// upgrade while its build runs, and requires the tree as it was before.
func TestBackendParity_Failures_Cancelled(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	root := failSetup(t, []parityFixture{failBasis, failUpgradeBase}, prefix, incDir)
	before := snapshotTree(t, root)

	fx := parityFixture{
		name: "sleeper", pkg: "aupg", version: "2.0-1",
		files: map[string]string{"shared.lua": "return 'aupg shared 2.0'\n"},
		rockspec: `
build = {
   type = "command",
   build_command = [[mkdir -p "$(LUADIR)" && cp shared.lua "$(LUADIR)/shared.lua" && touch started && sleep 60]],
}
`,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := t.TempDir()

	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(filepath.Join(src, "started")); err == nil {
				cancel()

				return
			}

			time.Sleep(20 * time.Millisecond)
		}
	}()

	start := time.Now()
	err := runFailingIn(t, ctx, failMake, client.BackendNative, root, fx, prefix, incDir, src)
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled, "the build failed as the context was cancelled: %v", err)
	assert.Less(t, time.Since(start), 30*time.Second, "the build stops with the context")

	assert.Equal(t, before, snapshotTree(t, root), "the tree after the cancelled build")
	requireTreeClean(t, "cancelled", failCfg(root, prefix, incDir))
}

// TestBackendParity_Failures_ReadOnlyDir makes a module directory of the
// tree read-only, so the native deploy of a rock with a module in it fails
// with EACCES after it took the plain path of shared from basis, and
// requires the tree as it was before.
func TestBackendParity_Failures_ReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}

	prefix, incDir := parityTarantool(t)

	locked := parityFixture{
		name: "rodir", pkg: "rodir", version: "1.0-1",
		files: map[string]string{"inner.lua": "return 'inner'\n"},
		rockspec: `
build = {
   type = "builtin",
   modules = { ["rodir.inner"] = "inner.lua" },
}
`,
	}

	root := failSetup(t, []parityFixture{failBasis, locked}, prefix, incDir)

	dir := filepath.Join(root, "share", "tarantool", "rodir")
	require.NoError(t, os.Chmod(dir, 0o555))       //nolint:gosec // a read-only directory is the point
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // let the test's cleanup remove it

	before := snapshotTree(t, root)

	fx := parityFixture{
		name: "aro", pkg: "aro", version: "1.0-1",
		files: map[string]string{
			"shared.lua": "return 'aro shared'\n",
			"aro.lua":    "return 'aro'\n",
			"x.lua":      "return 'x'\n",
		},
		rockspec: `
build = {
   type = "builtin",
   modules = { shared = "shared.lua", aro = "aro.lua", ["rodir.x"] = "x.lua" },
}
`,
	}

	err := runFailing(t, context.Background(), failMake, client.BackendNative, root, fx, prefix, incDir)
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrPermission)

	assert.Equal(t, before, snapshotTree(t, root), "the tree after the failed deploy")
	requireTreeClean(t, "read-only dir", failCfg(root, prefix, incDir))
}

// failCfg is the configuration the harness runs against root with.
func failCfg(root, prefix, incDir string) rocks.Config {
	return rocks.Config{Tree: root, Tarantool: rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir}}
}

// failSetup returns a fresh tree with the rocks of setup made into it, in
// order, by the native backend.
func failSetup(t *testing.T, setup []parityFixture, prefix, incDir string) string {
	t.Helper()

	root := t.TempDir()
	for _, fx := range setup {
		parityStep(t, parityOps()[0], fx, false, client.BackendNative, root, prefix, incDir)
	}

	return root
}

// runFailing stages fx's sources in a fresh directory and runs op on fx
// through backend into root, returning the operation's error.
func runFailing(
	t *testing.T, ctx context.Context, op string, backend client.Backend, root string, fx parityFixture, prefix, incDir string,
) error {
	t.Helper()

	return runFailingIn(t, ctx, op, backend, root, fx, prefix, incDir, t.TempDir())
}

// runFailingIn is runFailing with fx's sources staged in src.
func runFailingIn(
	t *testing.T, ctx context.Context, op string, backend client.Backend, root string, fx parityFixture, prefix, incDir, src string,
) error {
	t.Helper()

	for rel, body := range fx.files {
		full := filepath.Join(src, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))

		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "bin/") {
			mode = 0o755
		}

		require.NoError(t, os.WriteFile(full, []byte(body), mode))
	}

	cfg := failCfg(root, prefix, incDir)
	cfg.WorkingDir = src

	if op != failMake {
		cfg.WorkingDir = t.TempDir()
	}

	r, err := client.New(cfg, client.WithBackend(backend))
	require.NoError(t, err, "New(%s, backend=%v)", fx.name, backend)

	switch op {
	case failMake:
		specPath := writeParityRockspec(t, fx, src, "file://localhost/"+fx.pkg+".tar.gz")

		return r.Make(ctx, client.MakeOpts{RockspecPath: specPath})
	case failInstall:
		repo := t.TempDir()
		writeParityRockspec(t, fx, repo, parityTarball(t, fx, src))
		manifest := "commands = {}\nmodules = {}\nrepository = {\n   [\"" + fx.pkg + "\"] = { [\"" + fx.version +
			"\"] = { { arch = \"rockspec\" } } },\n}\n"
		require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(manifest), 0o600))

		return r.Install(ctx, fx.pkg, client.InstallOpts{Version: fx.version, Servers: []string{repo}})
	}

	return errors.New("unknown operation " + op)
}
