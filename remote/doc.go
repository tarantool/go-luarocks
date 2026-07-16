// Package remote implements the default rocks.RemoteIndex against an
// HTTP(S) rock server. It is consumed by the Rocks facade's New
// constructor and by deps.Resolve via the rocks.RemoteIndex interface.
//
// HTTPRemoteIndex is the sole type: configure it with the server URL list
// (plus optional InsecureServers and LuaVersion) and call Query to list every
// known version of a rock across all servers, newest-format manifest first
// (manifest-<lua_version>.json, then the Lua-source manifests). Parsed
// manifests are cached per (server, namespace) for the life of the value,
// and each returned rocks.VersionedRock carries the URL of its preferred
// artifact (.rock over .src.rock over bare .rockspec).
//
// The package lives outside the root rocks package because it consumes
// manif (Lua-source manifest parser) which itself imports rocks for the
// shared data types — placing HTTPRemoteIndex at the root would create
// an import cycle.
package remote
