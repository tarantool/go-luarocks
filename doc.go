// Package rocks is a pure-Go implementation of the LuaRocks subset needed
// to manage rocks inside a Tarantool installation.
//
// The library produces on-disk artifacts (tree layout, manifest files,
// .rock archives) that are byte-equal to what upstream LuaRocks 3.9.2 would
// produce for the same rock at the same version against a Tarantool target.
// A rock installed by lib/luarocks is queryable by upstream
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
