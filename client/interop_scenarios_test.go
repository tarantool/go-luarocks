//go:build integration_luaengine

package client_test

// The scenarios of the backend-interoperability harness (interop_test.go).

import (
	"testing"

	"github.com/tarantool/go-luarocks/client"
)

// interopReadFixtures are the parity fixtures the read scenarios deploy: Lua
// and C modules, an init module, wrapped and copied commands, conf and doc
// files, and the make, cmake and command build types.
func interopReadFixtures() []parityFixture {
	keep := map[string]bool{
		"install-sections": true, "copy-directories": true, "c-module": true, "init-module": true,
		"bin-scripts": true, "conf-files": true, "make-rock": true, "cmake-rock": true, "command-rock": true,
	}

	var out []parityFixture

	for _, fx := range parityFixtures() {
		if keep[fx.name] {
			out = append(out, fx)
		}
	}

	return out
}

// interopEveryOp are the read fixtures deployed through every operation; the
// others go through Make alone, since the operations differ in where the
// sources come from, not in how a tree is written.
var interopEveryOp = map[string]bool{"install-sections": true, "c-module": true, "bin-scripts": true}

// TestBackendInterop_Read deploys each read fixture with A and has every
// reader report on the tree; then A deploys it and B deploys it again over
// A's install.
func TestBackendInterop_Read(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, op := range parityOps() {
		for _, fx := range interopReadFixtures() {
			if op.name != "make" && !interopEveryOp[fx.name] {
				continue
			}

			t.Run(op.name+"/"+fx.name, func(t *testing.T) {
				t.Parallel()
				testInterop(t, prefix, incDir, interopScenario{
					name:  op.name + "/" + fx.name,
					steps: []interopStep{deployStep(roleA, op, fx, false)},
				})
			})
			t.Run(op.name+"/"+fx.name+"/redeploy", func(t *testing.T) {
				t.Parallel()
				testInterop(t, prefix, incDir, interopScenario{
					name:  op.name + "/" + fx.name + "/redeploy",
					steps: []interopStep{deployStep(roleA, op, fx, false), deployStep(roleB, op, fx, false)},
				})
			})
		}
	}
}

// alternate returns the steps of a parity version scenario with A running the
// first, B the second, A the third and so on.
func alternate(op parityOp, sc parityVersionScenario) []interopStep {
	steps := make([]interopStep, 0, len(sc.steps))
	for i, st := range sc.steps {
		steps = append(steps, deployStep(interopRole(i%2), op, st.fx, st.keep))
	}

	return steps
}

// interopVersionDivergence says how a version scenario run through Make, its
// steps alternating between A and B, ends by design where the backends differ
// (divOtherVersions, divResolvedDeps): which reference each direction ends at
// (follow), and which direction ends at neither (known). Upstream make
// removes the other versions of the rock it builds, which the native backend
// keeps, so a tree ends with the versions of whichever backend made the last
// version, the B role's in a two-step scenario. Upstream recomputes every
// rock's resolved dependencies on a write, so the tree manifest ends as the
// backend of the last write leaves it, the A role's in a three-step one.
type interopVersionDivergence struct {
	diverges string
	follow   map[string]client.Backend
	known    map[string]string
}

// interopVersionDivergences are the version scenarios of
// interopVersionScenarios that end differently by design, by name.
var interopVersionDivergences = func() map[string]interopVersionDivergence {
	otherB := interopVersionDivergence{diverges: divOtherVersions, follow: followB}
	// A native 2.0, B lua 1.0 (which removes 2.0), A native 3.0 leaves 1.0 and
	// 3.0: neither reference.
	threeVersions := interopVersionDivergence{
		diverges: divOtherVersions,
		follow:   map[string]client.Backend{luaWrites: client.BackendLua},
		known:    map[string]string{nativeWrites: divOtherVersions + ": a lua make between two native ones keeps the last two versions"},
	}

	return map[string]interopVersionDivergence{
		"upgrade":                           otherB,
		"upgrade-keep":                      otherB,
		"downgrade":                         otherB,
		"downgrade-keep":                    otherB,
		"lua-to-c-keep":                     otherB,
		"c-to-lua-keep":                     otherB,
		"downgrade-c-to-lua":                otherB,
		"three-versions":                    threeVersions,
		"three-versions-keep":               threeVersions,
		"dependent-follows-upgrade":         {diverges: divOtherVersions, follow: followA},
		"dependency-chain":                  {diverges: divResolvedDeps, follow: followA},
		"upgrade/remove":                    otherB,
		"upgrade-keep/remove-old":           otherB,
		"upgrade-keep/remove-new":           otherB,
		"downgrade-keep/remove-old":         otherB,
		"downgrade-keep/remove-new":         otherB,
		"upgrade-keep/remove-all":           otherB,
		"three-versions-keep/remove-middle": threeVersions,
		"lua-to-c-keep/remove-new":          otherB,
		"c-to-lua-keep/remove-old":          otherB,
	}
}()

// interopVersionScenarios are trees holding several versions of a rock, or
// rocks providing the same module or command, made by both backends in turn,
// then a version removed: the upgrade and downgrade, with and without Keep,
// and the demoted (versioned) file names both backends must understand to give
// each item's plain path back to the right provider.
func interopVersionScenarios(op parityOp) []interopScenario {
	var out []interopScenario

	for _, sc := range parityVersionScenarios() {
		out = append(out, interopScenario{name: sc.name, seed: sc.seed, steps: alternate(op, sc)})
	}

	dA := func(fx parityFixture, keep bool) interopStep { return deployStep(roleA, op, fx, keep) }
	dB := func(fx parityFixture, keep bool) interopStep { return deployStep(roleB, op, fx, keep) }
	rm := func(role interopRole, name, version string) interopStep {
		return removeStep(role, name, client.RemoveOpts{Version: version})
	}

	return append(out,
		interopScenario{name: "upgrade/remove", steps: []interopStep{
			dA(vrock("1.0"), false), dB(vrock("2.0"), false), rm(roleA, "vrock", "2.0-1"),
		}, show: []string{"vrock"}, which: []string{"vrock", "vrock.only1"}},
		interopScenario{name: "upgrade-keep/remove-old", steps: []interopStep{
			dA(vrock("1.0"), false), dB(vrock("2.0"), true), rm(roleA, "vrock", "1.0-1"),
		}, which: []string{"vrock.only1"}},
		interopScenario{name: "upgrade-keep/remove-new", steps: []interopStep{
			dA(vrock("1.0"), false), dB(vrock("2.0"), true), rm(roleA, "vrock", "2.0-1"),
		}},
		interopScenario{name: "downgrade-keep/remove-old", steps: []interopStep{
			dA(vrock("2.0"), false), dB(vrock("1.0"), true), rm(roleA, "vrock", "1.0-1"),
		}, which: []string{"vrock.only1"}},
		interopScenario{name: "downgrade-keep/remove-new", steps: []interopStep{
			dA(vrock("2.0"), false), dB(vrock("1.0"), true), rm(roleA, "vrock", "2.0-1"),
		}},
		interopScenario{name: "upgrade-keep/remove-all", steps: []interopStep{
			dA(vrock("1.0"), false), dB(vrock("2.0"), true), rm(roleA, "vrock", ""),
		}, show: []string{"vrock"}, which: []string{"vrock", "vrock.only1"}},
		interopScenario{name: "three-versions-keep/remove-middle", steps: []interopStep{
			dA(vrock("2.0"), false), dB(vrock("1.0"), true), dA(vrock("3.0"), true), rm(roleB, "vrock", "2.0-1"),
		}},
		// bbb's files take the versioned names; removing aaa gives them back
		// their plain paths.
		interopScenario{name: "two-rocks-ab/remove-plain", steps: []interopStep{
			dA(sharer("aaa"), false), dB(sharer("bbb"), false), rm(roleA, "aaa", ""),
		}, show: []string{"aaa"}, which: []string{"aaa"}},
		// aaa takes the plain paths from bbb, whose files B renames.
		interopScenario{name: "two-rocks-ba/remove-plain", steps: []interopStep{
			dA(sharer("bbb"), false), dB(sharer("aaa"), false), rm(roleA, "aaa", ""),
		}, show: []string{"aaa"}, which: []string{"aaa"}},
		interopScenario{name: "two-rocks-ba/remove-demoted", steps: []interopStep{
			dA(sharer("bbb"), false), dB(sharer("aaa"), false), rm(roleA, "bbb", ""),
		}, show: []string{"bbb"}, which: []string{"bbb"}},
		interopScenario{name: "two-rocks-lua-then-c/remove", steps: []interopStep{
			dA(formRock("bbb", true), false), dB(formRock("aaa", false), false), rm(roleA, "aaa", ""),
		}, show: []string{"aaa"}, which: []string{"aaa"}},
		interopScenario{name: "lua-to-c-keep/remove-new", steps: []interopStep{
			dA(pairRock("1.0", true), false), dB(pairRock("2.0", false), true), rm(roleA, "pair", "2.0-1"),
		}},
		interopScenario{name: "c-to-lua-keep/remove-old", steps: []interopStep{
			dA(pairRock("1.0", false), false), dB(pairRock("2.0", true), true), rm(roleA, "pair", "1.0-1"),
		}},
		// B's upgrade must keep 1.0, which A's dependent needs.
		interopScenario{name: "dependent-keeps-old/remove-dependent", steps: []interopStep{
			dA(vrock("1.0"), false), dA(dependent("user", "vrock == 1.0"), false), dB(vrock("2.0"), false),
			rm(roleA, "user", ""), rm(roleB, "vrock", "1.0-1"),
		}, show: []string{"user"}},
	)
}

// TestBackendInterop_Versions runs the version scenarios, every step through
// Make.
func TestBackendInterop_Versions(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, op := range parityOps() {
		if op.name != "make" {
			continue
		}

		for _, sc := range interopVersionScenarios(op) {
			if d, ok := interopVersionDivergences[sc.name]; ok {
				sc.diverges, sc.follow, sc.known = d.diverges, d.follow, d.known
			}

			t.Run(op.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				testInterop(t, prefix, incDir, sc)
			})
		}
	}
}
