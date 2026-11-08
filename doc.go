// Package rocks is the root of go-luarocks, a Go library that manages rocks
// inside a Tarantool installation. It ships two backends, selectable per
// client: a pure-Go native engine (the default) implementing the LuaRocks
// subset Tarantool needs, and vendored upstream LuaRocks 3.9.2 run through
// gopher-lua.
//
// The native backend deploys the same files as upstream LuaRocks 3.9.2
// would for the same rock at the same version against a Tarantool target:
// everything outside share/tarantool/rocks/ is byte-equal, compiled modules
// aside. A rock installed by this library is queryable by upstream
// LuaRocks's `luarocks show` and vice versa.
//
// This root package holds the shared vocabulary the sub-packages exchange:
//
//   - Data types: Config (with TarantoolConfig), Rockspec and its parts
//     (Source, Build, Description, Dep), Manifest / RockManifest, Version /
//     VersionConstraint, InstallStep, InstalledRock, ShowInfo.
//   - Interfaces: Fetcher, Builder, RemoteIndex, ManifestStore — the seams
//     the facade composes and tests fake.
//   - Error sentinels: ErrNotImplemented, ErrUnsupportedRockspecFeature,
//     ErrMissingTarantoolHeaders, ErrMissingTarantoolBinary,
//     ErrUnsupportedCommand — discriminate them with errors.Is.
//
// The facade — `client.New(rocks.Config{...}).Install(ctx, "metrics", opts)`
// — lives in the sub-package `github.com/tarantool/go-luarocks/client`
// rather than at this root: it calls `deps.Resolve` and composes the `remote`
// index, both of which transitively import this root package for shared data
// types, so a facade here would form an import cycle.
package rocks
