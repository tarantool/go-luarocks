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
// The indexes share everything but the transport: manifest probe order (the
// zipped and plain manifest-<lua_version>, then manifest, then the
// Go-specific manifest-<lua_version>.json), decoding, the arch filter, the
// artifact preference (.rock over .src.rock over bare .rockspec), manifest
// caching per (server, namespace) for the life of the value, and — because
// callers branch on it — error classification: a rock the manifest does not
// list is an empty, error-free result, while an unusable server is a non-nil
// error.
//
// The package lives outside the root rocks package because it consumes
// manif (Lua-source manifest parser) which itself imports rocks for the
// shared data types — placing the indexes at the root would create an import
// cycle.
package remote
