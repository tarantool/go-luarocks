package tree

// White-box tests of the tree manifest bookkeeping, which moved here from
// package client with the helpers they cover.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// TestAddProvider_UpstreamOrder: provider lists keep upstream's sort_pkgs
// order — rock name first, then newest version first — whatever the order the
// rocks were installed in, so index 0 is always the provider Deploy gave the
// plain path. Recording a newly installed rock at the front made a downgrade
// (or a second rock sorting after the first) the "active" provider of files it
// had deployed under versioned names.
func TestAddProvider_UpstreamOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		list     []string
		provider string
		want     []string
	}{
		{"upgrade", []string{"demo/1.0-1"}, "demo/2.0-1", []string{"demo/2.0-1", "demo/1.0-1"}},
		{"downgrade", []string{"demo/2.0-1"}, "demo/1.0-1", []string{"demo/2.0-1", "demo/1.0-1"}},
		{"reinstall", []string{"demo/1.0-1"}, "demo/1.0-1", []string{"demo/1.0-1"}},
		{"middle version", []string{"demo/3.0-1", "demo/1.0-1"}, "demo/2.0-1", []string{"demo/3.0-1", "demo/2.0-1", "demo/1.0-1"}},
		{"versions compare as versions", []string{"demo/9.0-1"}, "demo/10.0-1", []string{"demo/10.0-1", "demo/9.0-1"}},
		{"smaller name first", []string{"bbb/1.0-1"}, "aaa/1.0-1", []string{"aaa/1.0-1", "bbb/1.0-1"}},
		{"larger name after", []string{"aaa/1.0-1"}, "bbb/2.0-1", []string{"aaa/1.0-1", "bbb/2.0-1"}},
		{"first provider", nil, "demo/1.0-1", []string{"demo/1.0-1"}},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, addProvider(c.list, c.provider), c.name)
	}
}

// TestManifestProviders_KeepsModulesAndCommandsApart: a module and a command
// of the same name are looked up in their own index, as upstream
// manif.get_providers does.
func TestManifestProviders_KeepsModulesAndCommandsApart(t *testing.T) {
	t.Parallel()

	m := &rocks.Manifest{
		Modules:  map[string][]string{"demo": {"demo/2.0-1", "demo/1.0-1"}},
		Commands: map[string][]string{"demo": {"cli/3.0-1"}, "tool": {"cli/3.0-1"}},
	}
	p := manifestProviders(m)

	assert.Equal(t, []string{"demo/2.0-1", "demo/1.0-1"}, p(ModuleItem, "demo"))
	assert.Equal(t, []string{"cli/3.0-1"}, p(CommandItem, "demo"))
	assert.Equal(t, []string{"cli/3.0-1"}, p(CommandItem, "tool"))
	assert.Empty(t, p(ModuleItem, "tool"))
	assert.Empty(t, p(ModuleItem, "absent"))
}

// TestResolveInstalledDeps pins go-luarocks' own per-entry repo.dependencies:
// the rock's direct dependencies, each at the newest installed version its
// constraints accept, without the rock itself or what is not installed. It
// picked the installed version with the greatest string instead, so "1.6-1"
// beat "1.10-1", and a version the constraint excludes was recorded.
func TestResolveInstalledDeps(t *testing.T) {
	t.Parallel()

	m := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"lib":     {"1.5-1": {Arch: "installed"}, "1.6-1": {Arch: "installed"}, "1.10-1": {Arch: "installed"}},
			"pinned":  {"1.5-1": {Arch: "installed"}, "2.0-1": {Arch: "installed"}, "bad": {Arch: "installed"}},
			"toonew":  {"3.0-1": {Arch: "installed"}},
			"self":    {"2.0-1": {Arch: "installed"}},
			"bounded": {"1.9-1": {Arch: "installed"}, "1.10-1": {Arch: "installed"}, "2.0-1": {Arch: "installed"}},
		},
	}
	constraints := func(s string) []rocks.VersionConstraint {
		cs, err := deps.ParseConstraints(s)
		require.NoError(t, err)

		return cs
	}
	spec := &rocks.Rockspec{
		Package: "self",
		Dependencies: []rocks.Dep{
			{Name: "lib"}, // newest by version order: 1.10-1
			{Name: "pinned", Constraints: constraints("< 2.0")},        // 2.0-1 is excluded
			{Name: "toonew", Constraints: constraints("~> 2")},         // nothing satisfies → omitted
			{Name: "bounded", Constraints: constraints(">= 1.9, < 2")}, // 1.10-1
			{Name: "self"},         // self → excluded
			{Name: "notinstalled"}, // absent → omitted
		},
	}
	got := resolveInstalledDeps(spec, m)
	assert.Equal(t, map[string]string{"lib": "1.10-1", "pinned": "1.5-1", "bounded": "1.10-1"}, got)
}

func TestRemoveProvider(t *testing.T) {
	t.Parallel()

	idx := map[string][]string{
		"mod":  {"demo/2.0-1", "demo/1.0-1"},
		"only": {"demo/1.0-1"},
	}
	removeProvider(idx, "demo/1.0-1")
	assert.Equal(t, []string{"demo/2.0-1"}, idx["mod"], "provider removed, active survivor kept")
	_, ok := idx["only"]
	assert.False(t, ok, "list emptied of all providers must be deleted")
}
