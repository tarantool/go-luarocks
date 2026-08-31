// Package client implements the Rocks facade — the keystone public API that
// composes the manif, rockspec, fetch, build, tree, deps and remote subsystems
// into LuaRocks operations. The native backend implements five write ops —
// Install, Build, Make, Pack, Unpack — and returns
// rocks.ErrNotImplemented for the other thirteen; the lua backend covers the
// full upstream command set. Reads (List, Show, Which) are served by r.store
// regardless of backend; the write set is the Engine interface (engine.go).
//
// Construct via New(cfg, opts...): the default backend is BackendNative,
// WithBackend(BackendLua) selects the embedded gopher-lua VM (booted lazily on
// the first write operation). Per-operation option structs (InstallOpts,
// BuildOpts, SearchOpts, ...) mirror the upstream CLI flags; Exec is the raw
// escape hatch that runs an arbitrary `luarocks` argv through the embedded
// dispatcher (BackendLua only).
//
// Why this lives in a sub-package rather than at the module root:
//
//   - deps/ imports rocks (root) for shared data types (Rockspec, Version,
//     VersionConstraint, …).
//   - The facade needs to invoke deps.Resolve.
//   - A direct rocks → deps import would create a cycle.
//
// The root rocks package retains the data types and interfaces; the
// operational Rocks struct + methods live here in the client package.
// Callers spell it `client.New(cfg)`.
//
// Subsystem references:
//
//   - rockspec.Eval / MergePlatforms / RuntimePlatforms / Validate
//   - fetch.Fetch / FetchWith
//   - build.RunBackend
//   - tree.Open / tree.Tree.Deploy / tree.Tree.Which
//   - manif.FileStore (default ManifestStore)
//   - deps.Resolve
//   - remote.NewIndex / remote.NewOrderedIndex (default RemoteIndex): each
//     entry of Config.Servers is dispatched by its own form, so a rock server
//     may be a local directory (`/srv/rocks`, `file:///srv/rocks`) as well as
//     an HTTP(S) URL, and the list is queried in configuration order,
//     first-found-wins.
package client
