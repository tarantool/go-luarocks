package deps_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// bareIndex is a RemoteIndex that returns name/version/URL rows without a
// preloaded Spec, like remote.HTTPRemoteIndex.
type bareIndex map[string][]rocks.VersionedRock

func (b bareIndex) Query(_ context.Context, name, _ string) ([]rocks.VersionedRock, error) {
	return b[name], nil
}

func mustVer(t *testing.T, raw string) rocks.Version {
	t.Helper()

	v, err := deps.ParseVersion(raw)
	require.NoError(t, err)

	return v
}

func mustDep(t *testing.T, name, expr string) rocks.Dep {
	t.Helper()

	cs, err := deps.ParseConstraints(expr)
	require.NoError(t, err)

	return rocks.Dep{Name: name, Constraints: cs}
}

// TestResolveWithoutFetcherStopsAtPreloadedSpecs shows the baseline: a bare
// index that preloads nothing resolves only the root's direct dependencies.
func TestResolveWithoutFetcherStopsAtPreloadedSpecs(t *testing.T) {
	t.Parallel()

	idx := bareIndex{
		"a": {{Name: "a", Version: mustVer(t, "1.0.0-1"), URL: "a-1.0.0-1"}},
		"b": {{Name: "b", Version: mustVer(t, "2.0.0-1"), URL: "b-2.0.0-1"}},
	}

	root := &rocks.Rockspec{Package: "root", Dependencies: []rocks.Dep{mustDep(t, "a", ">=1.0")}}

	steps, err := deps.Resolve(context.Background(), root, idx)
	require.NoError(t, err)

	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}

	// Without a fetcher, a's transitive dep on b is never discovered.
	assert.Equal(t, []string{"a"}, names)
}

// TestResolveWithInstalledSkipsSatisfied — glr-n88: a dependency already
// satisfied by an installed rock is skipped (not queried or added to the plan).
func TestResolveWithInstalledSkipsSatisfied(t *testing.T) {
	t.Parallel()

	idx := bareIndex{
		"a": {{Name: "a", Version: mustVer(t, "1.0.0-1"), URL: "a-1.0.0-1"}},
		"b": {{Name: "b", Version: mustVer(t, "2.0.0-1"), URL: "b-2.0.0-1"}},
	}

	root := &rocks.Rockspec{Package: "root", Dependencies: []rocks.Dep{
		mustDep(t, "a", ">=1.0"),
		mustDep(t, "b", ">=1.0"),
	}}

	// b 1.5.0-1 is already installed and satisfies ">=1.0" → b is skipped.
	installed := func(name string) []rocks.Version {
		if name == "b" {
			return []rocks.Version{mustVer(t, "1.5.0-1")}
		}

		return nil
	}

	steps, err := deps.Resolve(context.Background(), root, idx, deps.WithInstalled(installed))
	require.NoError(t, err)

	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}

	assert.Equal(t, []string{"a"}, names, "installed-and-satisfying b must be skipped")
}

// TestResolveWithInstalledStillInstallsUnsatisfied — an installed version that
// does NOT satisfy the constraint does not cause a skip.
func TestResolveWithInstalledStillInstallsUnsatisfied(t *testing.T) {
	t.Parallel()

	idx := bareIndex{
		"b": {{Name: "b", Version: mustVer(t, "2.0.0-1"), URL: "b-2.0.0-1"}},
	}

	root := &rocks.Rockspec{Package: "root", Dependencies: []rocks.Dep{mustDep(t, "b", ">=2.0")}}

	installed := func(name string) []rocks.Version {
		if name == "b" {
			return []rocks.Version{mustVer(t, "1.0.0-1")} // too old for >=2.0
		}

		return nil
	}

	steps, err := deps.Resolve(context.Background(), root, idx, deps.WithInstalled(installed))
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, "b", steps[0].Name, "installed-but-unsatisfying b must still be installed")
}

// TestResolveWithSpecFetcherWalksTransitively shows the fetcher completing the
// closure, fetching only the chosen version of each name.
func TestResolveWithSpecFetcherWalksTransitively(t *testing.T) {
	t.Parallel()

	idx := bareIndex{
		"a": {
			{Name: "a", Version: mustVer(t, "0.5.0-1"), URL: "a-0.5.0-1"},
			{Name: "a", Version: mustVer(t, "1.0.0-1"), URL: "a-1.0.0-1"},
		},
		"b": {{Name: "b", Version: mustVer(t, "2.0.0-1"), URL: "b-2.0.0-1"}},
	}

	specs := map[string]*rocks.Rockspec{
		"a-1.0.0-1": {Package: "a", Dependencies: []rocks.Dep{mustDep(t, "b", ">=1.0")}},
		"b-2.0.0-1": {Package: "b"},
	}

	var fetched []string

	fetcher := func(_ context.Context, rock rocks.VersionedRock) (*rocks.Rockspec, error) {
		fetched = append(fetched, rock.URL)

		spec, ok := specs[rock.URL]
		if !ok {
			return nil, fmt.Errorf("no spec for %s", rock.URL)
		}

		return spec, nil
	}

	root := &rocks.Rockspec{Package: "root", Dependencies: []rocks.Dep{mustDep(t, "a", ">=1.0")}}

	steps, err := deps.Resolve(context.Background(), root, idx, deps.WithSpecFetcher(fetcher))
	require.NoError(t, err)

	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}

	// Full closure, deepest-first topo order.
	assert.Equal(t, []string{"b", "a"}, names)
	// Only the chosen version of each name is fetched: a-1.0.0-1 (not the
	// rejected a-0.5.0-1) and b-2.0.0-1.
	assert.ElementsMatch(t, []string{"a-1.0.0-1", "b-2.0.0-1"}, fetched)
}

// TestResolve_ProvidedTarantoolSatisfiesDep verifies a `tarantool >= X` dep is
// satisfied by the VM-provided version (glr-9pr) and never queried.
func TestResolve_ProvidedTarantoolSatisfiesDep(t *testing.T) {
	t.Parallel()

	root := &rocks.Rockspec{
		Package:      "app",
		Dependencies: []rocks.Dep{mustDep(t, "tarantool", ">= 2.10")},
	}
	// Empty index: if the resolver tried to fetch tarantool it would fail.
	plan, err := deps.Resolve(context.Background(), root, bareIndex{},
		deps.WithProvided(map[string]rocks.Version{"tarantool": mustVer(t, "2.11.0-1")}))
	require.NoError(t, err)
	assert.Empty(t, plan, "provided tarantool must be dropped from the install plan")
}

// TestResolve_ProvidedVersionMustSatisfyConstraint verifies a provided rock is
// NOT silently accepted when its version fails the constraint (glr-pzc):
// Tarantool provides lua 5.1, so `lua == 5.3` must error.
func TestResolve_ProvidedVersionMustSatisfyConstraint(t *testing.T) {
	t.Parallel()

	root := &rocks.Rockspec{
		Package:      "app",
		Dependencies: []rocks.Dep{mustDep(t, "lua", "== 5.3")},
	}
	_, err := deps.Resolve(context.Background(), root, bareIndex{})
	require.Error(t, err, "lua == 5.3 must fail against VM-provided 5.1")
	assert.Contains(t, err.Error(), "could not satisfy dependency lua")
}
