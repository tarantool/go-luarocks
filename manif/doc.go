// Package manif reads and writes LuaRocks tree- and rock-manifest files.
//
// Write serializes a Go value into upstream's "assignments" persist format;
// Parse decodes it back into native Go values (map[string]any, []any, int64,
// string, bool). FileStore is the rocks.ManifestStore implementation the
// facade uses: ReadTree / WriteTree round-trip the typed *rocks.Manifest at
// <tree-rocks-dir>/manifest, ReadRock reads a per-rock rock_manifest.
// ReadTreeManifest is the path-level convenience. Every parse failure wraps
// the ErrParse sentinel; discriminate with errors.Is.
//
// The writer is a byte-for-byte reimplementation of upstream
// `luarocks/src/luarocks/persist.lua`'s `save_from_table_to_string`
// pipeline. Goldens under testdata/persist/out are generated from upstream
// (see gen_goldens.sh) and are the canonical regression evidence.
//
// The parser is a small hand-rolled recursive-descent reader for the
// restricted Lua-table-literal subset that persist.lua emits. It deliberately
// does NOT depend on a Lua VM and fails loud on unsupported syntax.
package manif
