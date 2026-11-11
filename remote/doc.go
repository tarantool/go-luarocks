// Package remote implements the default rocks.RemoteIndex against a rock
// server. It is consumed by the Rocks facade's New constructor and by
// deps.Resolve via the rocks.RemoteIndex interface.
//
// A rock server is either an HTTP(S) endpoint or a local directory, and the
// server string's form decides which — the same choice upstream makes in
// dir.split_url (core/dir.lua:41-51), which is what lets
// `luarocks --only-server=/path/to/repo` serve an offline install. Call
// NewIndex (or NewIndexes for a whole configured list) to get the right one
// without deciding by hand; LocalServerPath states the rule.
//
//   - HTTPRemoteIndex fetches manifests over HTTP/HTTPS, with an optional
//     per-host TLS opt-out, and merges several servers into one query.
//   - FileRemoteIndex reads them out of a directory and resolves artifacts to
//     paths under it, so a local mirror needs no server.
//   - OrderedIndex composes any mix of the two, first-found-wins.
//
// Search is the other question a rock server answers, and it takes the
// opposite rule: it walks EVERY server and reports everything matching a
// pattern, because a search says what exists while an install picks one. It
// reuses the same loaders, so it needs no index of its own.
//
// A server may publish a tt-rocks-index.json (RocksIndexFile, RocksIndex)
// instead of manifests, and every index, and Search, reads it first: it
// names each file by URL, so the rocks may live elsewhere — on release
// pages, say — and gives every file's sha256 and each version's rockspec,
// which the rows of a Query carry (rocks.VersionedRock) for the native
// backend to verify downloads with and resolve from. Only the native backend
// reads it; upstream LuaRocks, and so the lua backend, reads manifests only.
//
// MakeManifest is the other side of a local server: it writes the manifests
// (and optionally index.html) that turn a directory of rock files into one,
// byte-equal to `luarocks-admin make_manifest`, without a Lua VM except for
// the rockspecs it reads.
//
// The indexes share everything but the transport: manifest probe order (the
// zipped and plain manifest-<lua_version>, then manifest, then the
// Go-specific manifest-<lua_version>.json), decoding, the arch filter, the
// artifact preference (.rock over .src.rock over bare .rockspec), manifest
// caching per (server, namespace) for the life of the value — and beyond it
// when IndexOptions.Cache hands several indexes one ManifestCache, as the
// client does for its lifetime — and, because callers branch on it, error
// classification: a rock the manifest does not list is an empty, error-free
// result, while an unusable server is a non-nil error.
//
// The package lives outside the root rocks package because it consumes
// manif (Lua-source manifest parser) which itself imports rocks for the
// shared data types — placing the indexes at the root would create an import
// cycle.
package remote
