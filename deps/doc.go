// Package deps implements version parsing, constraint matching and the
// transitive dependency resolver used by the Rocks facade.
//
// The surface splits in two layers:
//
//   - Version algebra: ParseVersion, Compare, Equal, ParseConstraint /
//     ParseConstraints, Match. Versions order component-by-component
//     numerically (1.10 > 1.2), with `scm-`/`dev-` pseudo-versions sorting
//     above every numeric release.
//   - Resolution: Resolve walks a rockspec's dependency closure against a
//     rocks.RemoteIndex and returns a topologically ordered []rocks.InstallStep.
//     Options tune it: WithProvided declares VM-provided rocks (e.g.
//     "tarantool" at the configured version), WithSpecFetcher supplies
//     on-demand rockspec evaluation for bare index rows, and WithInstalled
//     skips dependencies already present in the tree. IsProvided answers the
//     base Lua-provided set.
//
// Upstream references:
//
//   - luarocks/src/luarocks/core/vers.lua — Version parsing + comparator.
//   - luarocks/src/luarocks/queries.lua   — Constraint grammar.
//   - luarocks/src/luarocks/search.lua    — Remote search / pick-latest.
//
// The on-the-wire shapes (rocks.Version, rocks.VersionConstraint) live in
// the root rocks package so callers can talk about versions without
// depending on deps.
package deps
