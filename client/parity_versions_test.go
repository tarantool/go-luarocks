//go:build integration_luaengine

package client_test

// Backend parity for trees that hold more than one rock providing the same
// files: versions of one rock made one after the other, and two different
// rocks providing the same module and command. These exercise what a
// single-rock fixture cannot: upstream's choice of which provider gets the
// plain (active) path and which a versioned one (repos.lua
// check_spot_if_available), the demotion of the previous provider's files
// (deploy_files), and the backups of files no rock owns (backup_existing).
//
// Each scenario runs every step through the same operation (Make or Build)
// into one tree per backend, then compares the two trees as the single-rock
// fixtures do: deployed artifacts byte for byte, every installed rock's
// rock_manifest, and the tree manifest.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
)

// parityVersionStep is one operation of a scenario: deploy fx.
type parityVersionStep struct {
	fx parityFixture
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

func parityVersionScenarios() []parityVersionScenario {
	return []parityVersionScenario{
		{name: "reinstall", steps: []parityVersionStep{{fx: vrock("1.0")}, {fx: vrock("1.0")}}},
		{name: "two-rocks-ba", steps: []parityVersionStep{{fx: sharer("bbb")}, {fx: sharer("aaa")}}},
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
						parityStep(t, op, step.fx, backend, tree, prefix, incDir)
					}

					return tree
				}

				treeNative := run(client.BackendNative)
				treeLua := run(client.BackendLua)

				compareDeployed(t, sc.name, treeNative, treeLua)

				installedN := installedVersions(t, treeNative)
				installedL := installedVersions(t, treeLua)
				require.Equal(t, installedL, installedN, "[%s] installed rocks", sc.name)

				for _, nv := range installedL {
					compareRockManifests(t, sc.name, treeNative, treeLua, nv[0], nv[1])
				}

				compareTreeManifests(t, sc.name, treeNative, treeLua)
			})
		}
	}
}
