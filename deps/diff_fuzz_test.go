package deps_test

// Differential tests: every input goes through the Go implementation and
// through upstream luarocks.core.vers / luarocks.queries, run by
// internal/luaref, and the two must agree. `go test` runs the seeds below;
// `go test -run '^$' -fuzz FuzzXxx ./deps/` explores further.

import (
	"strings"
	"testing"

	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/luaref"
)

// versionSeeds are version strings from the deps tests, from upstream's
// version tests (luarocks spec/unit/deps_spec.lua) and from the edges of
// parse_version: keywords, the byte fallback for unknown words, separators,
// revisions, and inputs upstream cannot parse.
var versionSeeds = []string{
	"1", "1.0", "1.0.0", "1.0.1", "1.1", "1.2", "1.2.0", "1.2.3", "1.2.3-4",
	"1.2.4", "1.2.99", "1.3", "1.5", "1.99", "2.0", "2.1", "2.0.0", "2.1.4",
	"5.1", "5.1.0", "0.9", "0.0.0", "99.99.99", "1.0-1", "1.0-2", "1.0.0-1",
	"1.0.0-2", "1.2-0", "1.2-5", "1.2.3-1", "1.2.3-2", "1.0rc1", "1.0rc2",
	"1.0-rc1", "1.0rc", "1.0pre", "1.0beta", "1.0alpha", "1.0-beta",
	"1.2beta", "1.0work", "1.0patch", "1_0", "1-0", "1..0", "1.0a1",
	"1.0.0a", "a1", "scm", "scm-1", "dev", "dev-1", "dev-2", "cvs", "cvs-1",
	"SCM", "Dev", "work", "patch", "3.0", "3.1", "1.10", "1.010", "007",
	"20230101.0", "2.1.0-beta3", "5.3.6-1", "0.1.0-0",
}

// constraintSeeds are constraint lists from the deps tests and the upstream
// specs: every operator and alias, separators, whitespace, the no-upgrade
// marker, and malformed operators.
var constraintSeeds = []string{
	"1.0", "== 1.0", "= 1.0", "~= 1.0", "!= 1.0", "> 1.0", "< 2.0", ">= 1.0",
	"<= 2.0", "~> 1.2", "~> 1.2.3", "~> 1.2.3-1", "~> 1.2-0", ">=1.0",
	">1.0", ">= 1.0, < 2.0", ">=1.0,<2.0", ">=1.0 <2.0", ">= 1.0 < 2.0",
	">= 1.2.3, < 2.0", "== 1.0-1", "== 5.1", "== 5.1.0", "~= 5.1", "<= 5.1",
	">= 5.1", ">= 5.1, < 5.5", "@>= 1.0", "@1.0", ">= scm", "== dev-1",
	"!! 2.0", ">", ">=", "==", "~>", "<", "<=", "~=", "=> 1.0", "=< 1.0",
	"<> 1.0", ">= 1.0,", ">= 1.0,,", ", >= 1.0",
}

// maxInput bounds the fuzzed strings. Real versions and constraints are
// short, and upstream's unanchored revision pattern "(.*)%-(%d+)$" takes
// quadratic time, which on long inputs only slows the fuzzer down.
const maxInput = 256

// tooLong reports whether any input exceeds maxInput.
func tooLong(in ...string) bool {
	for _, s := range in {
		if len(s) > maxInput {
			return true
		}
	}

	return false
}

// newVM boots the reference for one fuzz target.
func newVM(f *testing.F) *luaref.VM {
	f.Helper()

	vm, err := luaref.New()
	if err != nil {
		f.Fatalf("boot reference: %v", err)
	}

	f.Cleanup(vm.Close)

	return vm
}

// FuzzParseVersion checks ParseVersion against vers.parse_version: the same
// inputs are rejected (upstream warns that it cannot parse them), and the
// rest parse to the same components and revision.
func FuzzParseVersion(f *testing.F) {
	for _, s := range versionSeeds {
		f.Add(s)
	}

	vm := newVM(f)

	f.Fuzz(func(t *testing.T, s string) {
		if tooLong(s) {
			return
		}

		gv, gerr := deps.ParseVersion(s)

		lv, err := vm.ParseVersion(s)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if (gerr == nil) == lv.Warned {
			t.Fatalf("ParseVersion(%q): error %v; upstream warned: %v (%q)", s, gerr, lv.Warned, vm.Warnings())
		}

		if gerr != nil {
			return
		}

		if d := luaref.DiffVersion(gv, lv); d != "" {
			t.Fatalf("ParseVersion(%q): %s", s, d)
		}
	})
}

// FuzzCompareVersions checks Compare and Equal against the operators of the
// upstream version metatable (__lt, __eq, __le) and vers.compare_versions,
// on every pair of versions both sides parse.
func FuzzCompareVersions(f *testing.F) {
	for i, a := range versionSeeds {
		f.Add(a, a)
		f.Add(a, versionSeeds[(i+1)%len(versionSeeds)])
		f.Add(a, versionSeeds[(i*7+3)%len(versionSeeds)])
	}

	vm := newVM(f)

	f.Fuzz(func(t *testing.T, a, b string) {
		if tooLong(a, b) {
			return
		}

		ga, errA := deps.ParseVersion(a)
		gb, errB := deps.ParseVersion(b)

		if errA != nil || errB != nil {
			return // FuzzParseVersion covers inputs Go rejects.
		}

		la, err := vm.ParseVersion(a)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		lb, err := vm.ParseVersion(b)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if la.Warned || lb.Warned {
			return // FuzzParseVersion covers inputs upstream rejects.
		}

		rel, err := vm.CompareVersions(a, b)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		cmp := deps.Compare(ga, gb)

		got := luaref.VersionRelations{
			Less:    cmp < 0,
			Equal:   deps.Equal(ga, gb),
			LessEq:  cmp <= 0,
			Greater: cmp > 0,
		}
		if got != rel {
			t.Fatalf("%q vs %q: Go %+v, upstream %+v", a, b, got, rel)
		}
	})
}

// luaSpace is Lua's %s class.
const luaSpace = " \t\n\v\f\r"

// FuzzParseConstraints checks ParseConstraints against upstream's
// parse_constraints (the local of luarocks.queries behind from_dep_string):
// the same inputs are rejected, and the rest give the same operators over the
// same versions.
//
// Upstream only ever hands parse_constraints what follows a dependency name
// and the whitespace after it, so the reference gets the input without its
// leading whitespace; ParseConstraints trims its input itself.
func FuzzParseConstraints(f *testing.F) {
	for _, s := range constraintSeeds {
		f.Add(s)
	}

	vm := newVM(f)

	f.Fuzz(func(t *testing.T, s string) {
		if tooLong(s) {
			return
		}

		gcs, gerr := deps.ParseConstraints(s)

		lcs, ok, msg, err := vm.ParseConstraints(strings.TrimLeft(s, luaSpace))
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if (gerr == nil) != ok {
			t.Fatalf("ParseConstraints(%q): error %v; upstream accepts: %v (%s)", s, gerr, ok, msg)
		}

		if gerr != nil {
			return
		}

		if d := luaref.DiffConstraints(gcs, lcs); d != "" {
			t.Fatalf("ParseConstraints(%q): %s", s, d)
		}
	})
}

// FuzzMatch checks Match against vers.match_constraints, on every version and
// constraint list both sides parse.
func FuzzMatch(f *testing.F) {
	for i, c := range constraintSeeds {
		for j := range 4 {
			f.Add(versionSeeds[(i*5+j*11)%len(versionSeeds)], c)
		}
	}

	vm := newVM(f)

	f.Fuzz(func(t *testing.T, version, constraints string) {
		if tooLong(version, constraints) {
			return
		}

		gv, err := deps.ParseVersion(version)
		if err != nil {
			return
		}

		gcs, err := deps.ParseConstraints(constraints)
		if err != nil {
			return
		}

		lv, err := vm.ParseVersion(version)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if lv.Warned {
			return
		}

		match, ok, err := vm.MatchConstraints(version, strings.TrimLeft(constraints, luaSpace))
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if !ok {
			return // FuzzParseConstraints covers inputs only Go accepts.
		}

		if got := deps.Match(gv, gcs); got != match {
			t.Fatalf("Match(%q, %q) = %v, upstream %v", version, constraints, got, match)
		}
	})
}
