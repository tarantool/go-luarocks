package deps

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// defaultProvided is the set of dependency names the Tarantool VM supplies
// itself, each keyed to the version the VM provides. A dep on one of these is
// satisfied WITHOUT a fetch only when the provided version meets the dep's
// constraints (upstream match_dep over util.get_rocks_provided). Tarantool
// embeds LuaJIT 5.1, so `lua` is provided at 5.1-1.
//
// Callers add VM-specific entries (notably `tarantool`, whose version is only
// known at runtime) via WithProvided.
func defaultProvided() map[string]rocks.Version {
	return map[string]rocks.Version{
		"lua":      mustVersion("5.1-1"),
		"luajit":   mustVersion("2.1-1"),
		"luabitop": mustVersion("1.0.2-1"),
	}
}

// mustVersion parses a known-good version literal, panicking on error. Used
// only for the compile-time-constant provided-rock versions.
func mustVersion(s string) rocks.Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic("deps: bad provided version literal " + s + ": " + err.Error())
	}

	return v
}

// IsProvided reports whether name is a rock the Tarantool VM supplies by
// default (ignoring VM-specific additions like `tarantool`). Retained for
// callers building their own resolution over the same registry.
func IsProvided(name string) bool {
	_, ok := defaultProvided()[name]

	return ok
}

// Resolve performs a greedy depth-first walk over root.Dependencies,
// choosing the newest version of each dep that satisfies its constraints
// and recursing into the chosen rock's transitive dependencies before
// moving on to its siblings.
//
// The result is in topological order (deepest-dep first), suitable for
// passing to Rocks.Install in sequence. The root rock itself is NOT
// included in the result — callers install it separately after their
// chosen pre-requisites are in place.
//
// Conflicts (two branches demanding incompatible versions of the same
// dep) and cycles are reported as errors with the dependency chain
// included in the message.
//
// `lua` and other VM-provided names short-circuit: they are silently
// dropped from the install list.
func Resolve(
	ctx context.Context, root *rocks.Rockspec, idx rocks.RemoteIndex, opts ...Option,
) ([]rocks.InstallStep, error) {
	if root == nil {
		return nil, errors.New("deps.Resolve: nil root rockspec")
	}

	r := &resolver{
		idx:      idx,
		chosen:   map[string]*rocks.InstallStep{},
		inFlight: map[string]bool{},
		provided: defaultProvided(),
	}

	for _, opt := range opts {
		opt(r)
	}

	if err := r.walk(ctx, root.Package, root.Dependencies, nil); err != nil {
		return nil, err
	}
	// Topo order: by recording when each name leaves the recursion (post-
	// order), the resulting slice already has deepest deps first. r.order
	// preserves that order.
	out := make([]rocks.InstallStep, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, *r.chosen[name])
	}

	return out, nil
}

// SpecFetcher supplies a picked rock's rockspec on demand. See WithSpecFetcher.
type SpecFetcher func(ctx context.Context, rock rocks.VersionedRock) (*rocks.Rockspec, error)

// Option configures Resolve.
type Option func(*resolver)

// WithSpecFetcher makes Resolve fetch a chosen rock's rockspec on demand when
// the index did not preload it. After pickNewest selects a version, if that
// version has no Spec, Resolve calls fetch on the picked rock only (never on
// the rejected candidates) and continues the transitive walk with the result.
//
// This is what lets a non-preloading index (e.g. remote.HTTPRemoteIndex, which
// returns bare name/version/URL rows) drive a full transitive resolution
// without fetching every candidate's rockspec: one fetch per chosen rock. A
// fetch error aborts the resolution. Without this option Resolve keeps its
// preload-only behavior — it recurses only into versions whose Spec the index
// already populated.
func WithSpecFetcher(fetch SpecFetcher) Option {
	return func(r *resolver) { r.fetch = fetch }
}

// WithProvided adds VM-provided rocks (name → provided version) on top of the
// default set. The canonical use is registering `tarantool` at the running
// VM's version so a `tarantool >= X` dependency is satisfied without a fetch —
// but only when the provided version meets the constraint.
func WithProvided(provided map[string]rocks.Version) Option {
	return func(r *resolver) {
		if r.provided == nil {
			r.provided = map[string]rocks.Version{}
		}

		maps.Copy(r.provided, provided)
	}
}

// InstalledLookup returns the versions of `name` already installed in the tree
// the resolver should consult. See WithInstalled.
type InstalledLookup func(name string) []rocks.Version

// WithInstalled makes Resolve skip a dependency that an already-installed rock
// satisfies, mirroring upstream match_dep finding the rock in the consulted
// tree (deps.lua:217-221) — so an install does not re-fetch/re-install deps
// that are already present. Which tree(s) are scanned (the deps_mode
// all/one/order/none selection) is the caller's concern: for deps_mode "none"
// simply do not pass this option.
func WithInstalled(lookup InstalledLookup) Option {
	return func(r *resolver) { r.installed = lookup }
}

type resolver struct {
	idx       rocks.RemoteIndex
	fetch     SpecFetcher
	provided  map[string]rocks.Version
	installed InstalledLookup
	chosen    map[string]*rocks.InstallStep
	inFlight  map[string]bool
	order     []string // post-order — deepest first
}

// installedSatisfies reports whether an already-installed version of d.Name
// meets d's constraints.
func (r *resolver) installedSatisfies(d rocks.Dep) bool {
	if r.installed == nil {
		return false
	}

	for _, iv := range r.installed(d.Name) {
		if Match(iv, d.Constraints) {
			return true
		}
	}

	return false
}

// walk visits every entry in deps. parent is included in cycle errors.
// chain tracks the dep-name stack for diagnostics.
func (r *resolver) walk(ctx context.Context, parent string, deps []rocks.Dep, chain []string) error {
	for _, d := range deps {
		if err := ctx.Err(); err != nil {
			return err
		}

		// A VM-provided dep is satisfied without a fetch ONLY when the
		// provided version meets the constraints; otherwise it is genuinely
		// unsatisfiable (upstream match_dep over the single provided version).
		if pv, ok := r.provided[d.Name]; ok {
			if Match(pv, d.Constraints) {
				continue
			}

			return fmt.Errorf(
				"deps.Resolve: could not satisfy dependency %s %s (VM provides %s)",
				d.Name, formatConstraints(d.Constraints), pv.Raw)
		}

		// An already-installed rock satisfying the constraint is skipped — not
		// re-queried, re-fetched, or reinstalled (upstream match_dep).
		if r.installedSatisfies(d) {
			continue
		}

		if r.inFlight[d.Name] {
			return fmt.Errorf("deps.Resolve: cycle detected on %q in chain %v", d.Name, append(chain, d.Name))
		}

		if existing, ok := r.chosen[d.Name]; ok {
			// Already chose a version for this name. Make sure the version
			// also satisfies the current constraints; if not, that's a
			// hard conflict.
			if !Match(existing.Version, d.Constraints) {
				return fmt.Errorf(
					"deps.Resolve: conflict for %q: previously chose %s but %s requires %v",
					d.Name, existing.Version.Raw, parent, formatConstraints(d.Constraints))
			}

			continue
		}

		r.inFlight[d.Name] = true

		candidates, err := r.idx.Query(ctx, d.Name, d.Namespace)
		if err != nil {
			return fmt.Errorf("deps.Resolve: query %q: %w", d.Name, err)
		}

		picked, ok := pickNewest(candidates, d.Constraints)
		if !ok {
			return fmt.Errorf(
				"deps.Resolve: no version of %q satisfies %s (parent=%s, %d candidates)",
				d.Name, formatConstraints(d.Constraints), parent, len(candidates))
		}
		// Preload the picked rock's rockspec on demand when the index did not
		// carry one, so the transitive walk below can proceed. Only the chosen
		// version is fetched — never the rejected candidates.
		if picked.Spec == nil && r.fetch != nil {
			spec, err := r.fetch(ctx, picked)
			if err != nil {
				return fmt.Errorf("deps.Resolve: fetch rockspec for %q: %w", d.Name, err)
			}

			picked.Spec = spec
		}

		step := &rocks.InstallStep{
			Name:     picked.Name,
			Version:  picked.Version,
			URL:      picked.URL,
			Rockspec: picked.Spec,
		}
		r.chosen[d.Name] = step

		// Recurse into the picked version's deps. With a spec fetcher the
		// picked version now carries its rockspec; without one we only have
		// transitive info when a candidate already carries a *Rockspec. No
		// index in this repo preloads; WithSpecFetcher populates picked.Spec above.
		if picked.Spec != nil {
			child := append(append([]string{}, chain...), d.Name)
			if err := r.walk(ctx, d.Name, picked.Spec.Dependencies, child); err != nil {
				return err
			}
		}

		delete(r.inFlight, d.Name)
		r.order = append(r.order, d.Name)
	}

	return nil
}

// pickNewest selects the highest version in candidates whose Version
// matches all of cs.
func pickNewest(candidates []rocks.VersionedRock, cs []rocks.VersionConstraint) (rocks.VersionedRock, bool) {
	filtered := make([]rocks.VersionedRock, 0, len(candidates))

	for _, c := range candidates {
		if Match(c.Version, cs) {
			filtered = append(filtered, c)
		}
	}

	if len(filtered) == 0 {
		return rocks.VersionedRock{}, false
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		return Compare(filtered[i].Version, filtered[j].Version) > 0
	})

	return filtered[0], true
}

func formatConstraints(cs []rocks.VersionConstraint) string {
	if len(cs) == 0 {
		return "(any)"
	}

	var out strings.Builder

	for i, c := range cs {
		if i > 0 {
			out.WriteString(", ")
		}

		out.WriteString(c.Op + " " + c.Version.Raw)
	}

	return out.String()
}
