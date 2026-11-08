//go:build integration_luaengine

package client_test

// Backend parity for trees that hold more than one rock providing the same
// files: two versions of one rock, made one after the other with and without
// Keep, and two different rocks providing the same module and command. These
// exercise what a single-rock fixture cannot: upstream's choice of which
// provider gets the plain (active) path and which a versioned one
// (repos.lua check_spot_if_available), the demotion of the previous
// provider's files (deploy_files), their promotion back when it goes away
// (delete_version), and the backups of files no rock owns (backup_existing).
//
// Each scenario runs every step through the same operation (Make, Build or
// Install) into one tree per backend, then compares the two trees as the
// single-rock fixtures do: deployed artifacts byte for byte, every installed
// rock's rock_manifest, and the tree manifest.
//
// Upstream's make, build and install also remove the other versions of a
// rock unless --keep (cmd/make.lua, cmd/build.lua, cmd/install.lua), and
// recompute every rock's resolved dependencies on each write; the native
// backend keeps go-luarocks' own semantics and does neither. The scenarios
// that shows in are listed in knownVersionDivergences: for them each backend
// must leave what it is meant to — every version deployed on the native one,
// the listed ones on the lua one — or the tree manifests must agree but for
// the resolved dependencies.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
)

// parityVersionStep is one operation of a scenario: deploy fx, with keep as
// the Keep option.
type parityVersionStep struct {
	fx   parityFixture
	keep bool
}

// parityVersionScenario deploys its steps in order into one tree, after
// writing seed (tree-relative path → contents) into it.
type parityVersionScenario struct {
	name  string
	seed  map[string]string
	steps []parityVersionStep
}

// vrock returns version ver of the "vrock" rock: a module and a command whose
// contents name the version, plus, in 1.0 only, a module 2.0 dropped.
func vrock(ver string) parityFixture {
	fx := parityFixture{
		name:    "vrock-" + ver,
		pkg:     "vrock",
		version: ver + "-1",
		files: map[string]string{
			"src/vrock.lua": "return '" + ver + "'\n",
			"bin/vrock-cli": "print('vrock " + ver + "')\n",
		},
	}

	modules := `      ["vrock"] = "src/vrock.lua",` + "\n"

	if ver == "1.0" {
		fx.files["src/only1.lua"] = "return 'only in 1.0'\n"
		modules += `      ["vrock.only1"] = "src/only1.lua",` + "\n"
	}

	fx.rockspec = `
build = {
   type = "builtin",
   modules = {
` + modules + `   },
   install = {
      bin = { ["vrock-cli"] = "bin/vrock-cli" },
   },
}
`

	return fx
}

// vrockSlim returns vrock 1.0 without the module 2.0 dropped: a re-make of the
// installed version whose build no longer ships one of its files.
func vrockSlim() parityFixture {
	fx := vrock("1.0")
	delete(fx.files, "src/only1.lua")
	fx.rockspec = strings.Replace(fx.rockspec, `      ["vrock.only1"] = "src/only1.lua",`+"\n", "", 1)

	return fx
}

// sharer returns the rock pkg, which provides its own module plus the module
// "shared" and the command "shared-cli" that every sharer provides.
func sharer(pkg string) parityFixture {
	return parityFixture{
		name:    pkg,
		pkg:     pkg,
		version: "1.0-1",
		files: map[string]string{
			"src/own.lua":    "return '" + pkg + "'\n",
			"src/shared.lua": "return 'shared from " + pkg + "'\n",
			"bin/shared-cli": "print('shared-cli from " + pkg + "')\n",
		},
		rockspec: `
build = {
   type = "builtin",
   modules = {
      ["` + pkg + `"] = "src/own.lua",
      ["shared"] = "src/shared.lua",
   },
   install = {
      bin = { ["shared-cli"] = "bin/shared-cli" },
   },
}
`,
	}
}

// namedItems returns the rock pkg, which provides its own module plus the
// module modName and the command cmdName, each when not empty.
func namedItems(pkg, modName, cmdName string) parityFixture {
	fx := parityFixture{
		name:    pkg,
		pkg:     pkg,
		version: "1.0-1",
		files:   map[string]string{"src/own.lua": "return '" + pkg + "'\n"},
	}

	modules := `      ["` + pkg + `"] = "src/own.lua",` + "\n"

	if modName != "" {
		fx.files["src/item.lua"] = "return 'module from " + pkg + "'\n"
		modules += `      ["` + modName + `"] = "src/item.lua",` + "\n"
	}

	install := ""

	if cmdName != "" {
		fx.files["bin/item"] = "print('command from " + pkg + "')\n"
		install = `   install = { bin = { ["` + cmdName + `"] = "bin/item" } },` + "\n"
	}

	fx.rockspec = `
build = {
   type = "builtin",
   modules = {
` + modules + `   },
` + install + `}
`

	return fx
}

// formRock returns the rock pkg, which provides its own module plus the module
// "form", a Lua file when lua is true and a C module otherwise.
func formRock(pkg string, lua bool) parityFixture {
	fx := parityFixture{
		name:    pkg,
		pkg:     pkg,
		version: "1.0-1",
		files:   map[string]string{"src/own.lua": "return '" + pkg + "'\n"},
	}

	form := `      ["form"] = "src/form.lua",`

	if lua {
		fx.files["src/form.lua"] = "return 'form from " + pkg + "'\n"
	} else {
		fx.files["src/form.c"] = parityCModule("form")
		form = `      ["form"] = "src/form.c",`
	}

	fx.rockspec = `
build = {
   type = "builtin",
   modules = {
      ["` + pkg + `"] = "src/own.lua",
` + form + `
   },
}
`

	return fx
}

// pairRock returns version ver of the "pair" rock, whose module pair.impl is
// a Lua file when lua is true and a C module otherwise. Upstream deploys the
// two forms of one module as a pair: the provider that loses the module's
// plain path loses it for both files.
func pairRock(ver string, lua bool) parityFixture {
	fx := parityFixture{
		name:    "pair-" + ver,
		pkg:     "pair",
		version: ver + "-1",
		files: map[string]string{
			"src/pair.lua": "return '" + ver + "'\n",
		},
	}

	impl := `      ["pair.impl"] = "src/impl.lua",`

	if lua {
		fx.files["src/impl.lua"] = "return 'impl " + ver + "'\n"
	} else {
		fx.files["src/impl.c"] = parityCModule("pair_impl")
		impl = `      ["pair.impl"] = "src/impl.c",`
	}

	fx.rockspec = `
build = {
   type = "builtin",
   modules = {
      ["pair"] = "src/pair.lua",
` + impl + `
   },
}
`

	return fx
}

// dependent returns the rock pkg, which provides its own module and depends on
// each of deps, written as rockspec dependency strings ("vrock >= 1.0").
func dependent(pkg string, deps ...string) parityFixture {
	quoted := make([]string, 0, len(deps))
	for _, d := range deps {
		quoted = append(quoted, `"`+d+`"`)
	}

	return parityFixture{
		name:    pkg,
		pkg:     pkg,
		version: "1.0-1",
		files: map[string]string{
			"src/" + pkg + ".lua": "return '" + pkg + "'\n",
		},
		rockspec: `
dependencies = { ` + strings.Join(quoted, ", ") + ` }
build = {
   type = "builtin",
   modules = {
      ["` + pkg + `"] = "src/` + pkg + `.lua",
   },
}
`,
	}
}

func parityVersionScenarios() []parityVersionScenario {
	return []parityVersionScenario{
		{name: "upgrade", steps: []parityVersionStep{{fx: vrock("1.0")}, {fx: vrock("2.0")}}},
		{name: "upgrade-keep", steps: []parityVersionStep{{fx: vrock("1.0")}, {fx: vrock("2.0"), keep: true}}},
		{name: "downgrade", steps: []parityVersionStep{{fx: vrock("2.0")}, {fx: vrock("1.0")}}},
		{name: "downgrade-keep", steps: []parityVersionStep{{fx: vrock("2.0")}, {fx: vrock("1.0"), keep: true}}},
		{name: "reinstall", steps: []parityVersionStep{{fx: vrock("1.0")}, {fx: vrock("1.0")}}},
		// A re-make of an installed version starts from a tree without it
		// (build.lua:393-395): a file the new build no longer ships goes.
		{name: "remake-drops-module", steps: []parityVersionStep{{fx: vrock("1.0")}, {fx: vrockSlim()}}},
		{name: "three-versions-keep", steps: []parityVersionStep{
			{fx: vrock("2.0")}, {fx: vrock("1.0"), keep: true}, {fx: vrock("3.0"), keep: true},
		}},
		{name: "three-versions", steps: []parityVersionStep{
			{fx: vrock("2.0")}, {fx: vrock("1.0"), keep: true}, {fx: vrock("3.0")},
		}},
		// Different rocks are not other versions of each other: both stay,
		// and the lexicographically smaller name owns the plain paths.
		{name: "two-rocks-ab", steps: []parityVersionStep{{fx: sharer("aaa")}, {fx: sharer("bbb")}}},
		{name: "two-rocks-ba", steps: []parityVersionStep{{fx: sharer("bbb")}, {fx: sharer("aaa")}}},
		// A module and a command are different items, even under one name
		// (manif.get_providers): zzz's command "tool" has no provider while
		// aaa's module "tool" is installed, so it takes the plain path.
		{name: "module-and-command-share-a-name", steps: []parityVersionStep{
			{fx: namedItems("aaa", "tool", "")}, {fx: namedItems("zzz", "", "tool")},
		}},
		// The rock taking a module's plain path demotes the other rock's file
		// of the module's other form too, so the module's .lua and .so never
		// come from two rocks (repos.lua:495-500, 514-519).
		{name: "two-rocks-lua-then-c", steps: []parityVersionStep{
			{fx: formRock("bbb", true)}, {fx: formRock("aaa", false)},
		}},
		{name: "two-rocks-c-then-lua", steps: []parityVersionStep{
			{fx: formRock("bbb", false)}, {fx: formRock("aaa", true)},
		}},
		{name: "lua-to-c-keep", steps: []parityVersionStep{
			{fx: pairRock("1.0", true)}, {fx: pairRock("2.0", false), keep: true},
		}},
		{name: "c-to-lua-keep", steps: []parityVersionStep{
			{fx: pairRock("1.0", false)}, {fx: pairRock("2.0", true), keep: true},
		}},
		// Removing 2.0's .so promotes 1.0's .lua, the module's other form.
		{name: "downgrade-c-to-lua", steps: []parityVersionStep{
			{fx: pairRock("2.0", false)}, {fx: pairRock("1.0", true)},
		}},
		// The other versions stay when an installed rock depends on one
		// that no remaining version satisfies (remove.lua check_dependents).
		{name: "dependent-keeps-old", steps: []parityVersionStep{
			{fx: vrock("1.0")}, {fx: dependent("user", "vrock == 1.0")}, {fx: vrock("2.0")},
		}},
		// ... and go when a remaining version satisfies it; the dependent's
		// resolved dependencies in the tree manifest follow the upgrade.
		{name: "dependent-follows-upgrade", steps: []parityVersionStep{
			{fx: vrock("1.0")}, {fx: dependent("user", "vrock >= 1.0")}, {fx: vrock("2.0")},
		}},
		// The tree manifest records each rock's dependencies transitively
		// (deps.scan_deps).
		{name: "dependency-chain", steps: []parityVersionStep{
			{fx: vrock("1.0")}, {fx: dependent("mid", "vrock")}, {fx: dependent("top", "mid")},
		}},
		// Files no installed rock provides are moved aside, never
		// overwritten, and an existing backup is not clobbered either.
		{
			name: "untracked-files",
			seed: map[string]string{
				"share/tarantool/vrock.lua":  "return 'untracked'\n",
				"share/tarantool/vrock.lua~": "return 'an older backup'\n",
				"bin/vrock-cli":              "#!/bin/sh\necho untracked\n",
			},
			steps: []parityVersionStep{{fx: vrock("1.0")}},
		},
	}
}

// TestBackendParity_Versions runs each multi-rock scenario through both
// backends and both operations and diffs the resulting trees.
func TestBackendParity_Versions(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, op := range parityOps() {
		for _, sc := range parityVersionScenarios() {
			t.Run(op.name+"/"+sc.name, func(t *testing.T) {
				run := func(backend client.Backend) string {
					tree := t.TempDir()

					for rel, body := range sc.seed {
						p := filepath.Join(tree, filepath.FromSlash(rel))
						require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
						require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
					}

					for _, step := range sc.steps {
						parityStep(t, op, step.fx, step.keep, backend, tree, prefix, incDir)
					}

					return tree
				}

				treeNative := run(client.BackendNative)
				treeLua := run(client.BackendLua)

				div, diverges := versionDivergenceOf(op, sc)
				if diverges && div.reason == divOtherVersions {
					t.Logf("[%s] known divergence: %s", sc.name, div.reason)
					require.Equal(t, stepVersions(sc), installedVersions(t, treeNative),
						"[%s] the native backend keeps every version", sc.name)
					require.Equal(t, div.lua, installedVersions(t, treeLua),
						"[%s] the lua backend removes the other versions", sc.name)

					return
				}

				compareDeployed(t, sc.name, treeNative, treeLua)

				installedN := installedVersions(t, treeNative)
				installedL := installedVersions(t, treeLua)
				require.Equal(t, installedL, installedN, "[%s] installed rocks", sc.name)

				for _, nv := range installedL {
					compareRockManifests(t, sc.name, treeNative, treeLua, nv[0], nv[1])
				}

				if diverges && div.reason == divResolvedDeps {
					t.Logf("[%s] known divergence: %s", sc.name, div.reason)
					compareResolvedDeps(t, sc.name, treeNative, treeLua)

					return
				}

				compareTreeManifests(t, sc.name, treeNative, treeLua)
			})
		}
	}
}
