package deps_test

import (
	"context"
	"fmt"
	"log"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// ExampleParseVersion parses a "components-revision" version string into its
// numeric parts and trailing revision.
func ExampleParseVersion() {
	v, err := deps.ParseVersion("1.2.3-4")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v.Components, "rev", v.Revision)
	// Output: [1 2 3] rev 4
}

// ExampleParseVersion_scm parses an "scm" pseudo-version, which sorts above
// every numeric release.
func ExampleParseVersion_scm() {
	v, _ := deps.ParseVersion("scm-1")
	fmt.Println(v.IsSCM, v.Raw)
	// Output: true scm-1
}

// ExampleCompare orders two versions component-by-component numerically, so
// 1.10 sorts above 1.2 (not lexically).
func ExampleCompare() {
	a, _ := deps.ParseVersion("2.0")
	b, _ := deps.ParseVersion("1.0")
	fmt.Println(deps.Compare(a, b)) // a > b

	// Numeric component compare, not lexical: 1.10 > 1.2.
	x, _ := deps.ParseVersion("1.2")
	y, _ := deps.ParseVersion("1.10")
	fmt.Println(deps.Compare(x, y))
	// Output:
	// 1
	// -1
}

// ExampleEqual contrasts order-equality with strict equality: Compare
// zero-pads operands, Equal requires an identical component count.
func ExampleEqual() {
	a, _ := deps.ParseVersion("5.1")
	b, _ := deps.ParseVersion("5.1.0")
	// Compare zero-pads and calls them order-equal; Equal requires an
	// identical component count, so 5.1 and 5.1.0 are NOT equal.
	fmt.Println(deps.Compare(a, b), deps.Equal(a, b))
	// Output: 0 false
}

// ExampleParseConstraint parses a single constraint; a bare version implies
// the "==" operator.
func ExampleParseConstraint() {
	c, _ := deps.ParseConstraint(">= 1.2.3")
	fmt.Printf("%s %s\n", c.Op, c.Version.Raw)
	d, _ := deps.ParseConstraint("1.0") // no operator ⇒ implicit "=="
	fmt.Printf("%s %s\n", d.Op, d.Version.Raw)
	// Output:
	// >= 1.2.3
	// == 1.0
}

// ExampleParseConstraints parses a comma-separated list of constraints.
func ExampleParseConstraints() {
	cs, _ := deps.ParseConstraints(">= 1.2.3, < 2.0")
	fmt.Println(len(cs), cs[0].Op, cs[1].Op)
	// Output: 2 >= <
}

// ExampleMatch tests a version against a constraint set (logical AND).
func ExampleMatch() {
	cs, _ := deps.ParseConstraints(">= 1.2.3, < 2.0")
	hit, _ := deps.ParseVersion("1.5")
	miss, _ := deps.ParseVersion("2.5")
	fmt.Println(deps.Match(hit, cs), deps.Match(miss, cs))
	// Output: true false
}

// ExampleIsProvided reports whether a dependency is satisfied by the base
// Lua-provided set. VM additions such as "tarantool" are excluded.
func ExampleIsProvided() {
	// IsProvided reports the base Lua-provided set; VM additions such as
	// "tarantool" are deliberately excluded.
	fmt.Println(deps.IsProvided("lua"), deps.IsProvided("tarantool"))
	// Output: true false
}

// exampleIndex is a preloading rocks.RemoteIndex: it already carries each
// candidate's *rocks.Rockspec, so Resolve never needs to fetch.
type exampleIndex struct {
	byName map[string][]rocks.VersionedRock
}

func (e exampleIndex) Query(_ context.Context, name, _ string) ([]rocks.VersionedRock, error) {
	return e.byName[name], nil
}

// ExampleResolve walks a rockspec's dependencies transitively against a
// RemoteIndex, producing a topo-ordered install list.
func ExampleResolve() {
	fooV, _ := deps.ParseVersion("1.0.0-1")
	idx := exampleIndex{byName: map[string][]rocks.VersionedRock{
		"foo": {{
			Name:    "foo",
			Version: fooV,
			URL:     "http://example.org/foo-1.0.0-1.rockspec",
			Spec:    &rocks.Rockspec{Package: "foo", Version: "1.0.0-1"},
		}},
	}}
	cs, _ := deps.ParseConstraints(">= 1.0")
	root := &rocks.Rockspec{
		Package: "app", Version: "0.1-1",
		Dependencies: []rocks.Dep{{Name: "foo", Constraints: cs}},
	}

	steps, err := deps.Resolve(context.Background(), root, idx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(steps), steps[0].Name, steps[0].Version.Raw)
	// Output: 1 foo 1.0.0-1
}

// ExampleResolve_withProvided shows the option surface: WithProvided declares
// a VM-provided version, so a matching dependency is satisfied without an
// install step. Compile-only (no Output) — it documents the call shape.
func ExampleResolve_withProvided() {
	ttVer, _ := deps.ParseVersion("2.11.0-1")
	root := &rocks.Rockspec{
		Package: "app", Version: "0.1-1",
	}
	idx := exampleIndex{byName: map[string][]rocks.VersionedRock{}}

	_, err := deps.Resolve(
		context.Background(), root, idx,
		deps.WithProvided(map[string]rocks.Version{"tarantool": ttVer}),
	)
	if err != nil {
		log.Fatal(err)
	}
}
