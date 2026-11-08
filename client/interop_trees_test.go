//go:build integration_luaengine

package client_test

// The scenarios of the backend-interoperability harness (interop_test.go)
// that remove rocks and rewrite the tree manifest.

import (
	"slices"
	"testing"

	"github.com/tarantool/go-luarocks/client"
)

// TestBackendInterop_Remove has B remove what A installed: hello and its
// dependency dep from installTestRepo, dep while hello depends on it, with
// and without Force and ForceFast. The native Remove removes dep either way,
// and recomputes no rock's resolved dependencies, where upstream refuses
// without a force and recomputes them with one (divRemoveDependents), so
// those scenarios end where B's backend alone ends, and a reinstall by A
// after them where A's does.
func TestBackendInterop_Remove(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)

	hello := func(role interopRole) interopStep {
		return installStep(role, 0, repo, "hello")
	}
	rm := removeStep
	both := []string{"hello", "dep"}

	const div = divRemoveDependents

	scenarios := []interopScenario{
		{name: "dependency", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{}),
		}, show: both, which: both, diverges: div, follow: followB},
		{name: "dependency/force", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{Force: true}),
		}, show: both, which: both, diverges: div, follow: followB},
		{name: "dependency/force-fast", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{ForceFast: true}),
		}, show: both, which: both, diverges: div, follow: followB},
		// The native backend's removal rewrites the manifest the lua one
		// wrote, and the lua reinstall then reads it.
		{name: "dependency/force/reinstall", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{Force: true}), hello(roleA),
		}, diverges: div, follow: followA},
		{name: "dependent-then-dependency", steps: []interopStep{
			hello(roleA), rm(roleB, "hello", client.RemoveOpts{}), rm(roleB, "dep", client.RemoveOpts{}),
		}, show: both, which: both},
		{name: "absent", steps: []interopStep{hello(roleA), rm(roleB, "nosuchrock", client.RemoveOpts{})}},
		{name: "absent-version", steps: []interopStep{hello(roleA), rm(roleB, "dep", client.RemoveOpts{Version: "9.9-1"})}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			testInterop(t, prefix, incDir, sc)
		})
	}
}

// TestBackendInterop_ManifestRoundTrip has B rewrite a tree manifest A wrote
// for a tree of several rocks — two versions of one, a dependency chain, rocks
// sharing a module and a command, a rock with a dependency from
// installTestRepo — by installing an unrelated rock, then by removing it
// again: every entry of the rocks B did not touch must come out as A left it.
// The second version is built with Keep, the one way upstream keeps the
// first. The lua backend recomputes every rock's resolved dependencies,
// transitively, when it writes the manifest and the native one leaves them as
// they are (divResolvedDeps), so each direction ends where the lua backend
// alone ends: a lua B recomputes what a native A wrote, and a native B keeps
// what a lua A wrote.
func TestBackendInterop_ManifestRoundTrip(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)

	var mk, bd parityOp

	for _, op := range parityOps() {
		switch op.name {
		case "make":
			mk = op
		case "build":
			bd = op
		}
	}

	written := []interopStep{
		deployStep(roleA, mk, vrock("1.0"), false),
		deployStep(roleA, bd, vrock("2.0"), true),
		deployStep(roleA, mk, dependent("mid", "vrock"), false),
		deployStep(roleA, mk, dependent("top", "mid"), false),
		deployStep(roleA, mk, sharer("aaa"), false),
		deployStep(roleA, mk, sharer("bbb"), false),
		deployStep(roleA, mk, formRock("ddd", true), false),
		deployStep(roleA, mk, formRock("ccc", false), false),
		installStep(roleA, 0, repo, "hello"),
	}
	unrelated := deployStep(roleB, mk, namedItems("zzz", "zzz.extra", "zzz-cli"), false)

	toLua := map[string]client.Backend{nativeWrites: client.BackendLua, luaWrites: client.BackendLua}

	for _, sc := range []interopScenario{
		{name: "install", steps: append(slices.Clone(written), unrelated), diverges: divResolvedDeps, follow: toLua},
		{name: "install-remove", steps: append(slices.Clone(written), unrelated,
			removeStep(roleB, "zzz", client.RemoveOpts{})), diverges: divResolvedDeps, follow: toLua},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			testInterop(t, prefix, incDir, sc)
		})
	}
}
