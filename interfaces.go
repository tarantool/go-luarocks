package rocks

import "context"

// Interfaces let the facade compose default implementations from the
// subsystem packages while tests inject fakes. No global state.

// Fetcher pulls a source at url into destDir and returns the on-disk path
// to the unpacked working tree.
//
// No type in this module implements it: the client calls the package fetch
// functions (FetchWith, Sources) directly, and fetch.Fetch is the matching
// plain function. They switch on URL scheme: http(s) via net/http, git* via
// the go-git library (no git binary), file:// directly.
type Fetcher interface {
	Fetch(ctx context.Context, url, destDir string) (string, error)
}

// Builder runs the build backend declared in spec.Build.Type against
// srcDir (the unpacked source) and writes the build output under destDir
// (`<tree>/share/tarantool/rocks/<name>/<ver>/build`).
//
// No type in this module implements it: the client calls build.RunBackend,
// which takes the same arguments plus the rocks.Config and dispatches on
// spec.Build.Type ∈ {"builtin","cmake","make","command","none"}.
type Builder interface {
	Build(ctx context.Context, spec *Rockspec, srcDir, destDir string) error
}

// RemoteIndex queries a rock server (or aggregate of servers) for every
// known version of `name`. It is the seam the deps resolver uses to
// discover candidate versions to install.
//
// The default implementation is a remote.OrderedIndex over one index per
// entry of cfg.Servers (remote.NewIndexes), constructed by client.New(cfg).
// Tests inject fakes via this interface.
type RemoteIndex interface {
	// Query returns candidate versions of `name`. A non-empty namespace selects
	// the server's per-namespace manifest (/manifests/<namespace>/) and filters
	// results to that namespace, mirroring upstream manifest_search.
	Query(ctx context.Context, name, namespace string) ([]VersionedRock, error)
}

// ManifestStore reads and writes the on-disk manifest files in a tree.
//
//   - ReadTree / WriteTree handle `<tree>/share/tarantool/rocks/manifest`
//     (the top-level index); path is the directory holding it.
//   - ReadRock / WriteRock handle `<tree>/share/tarantool/rocks/<name>/<ver>/rock_manifest`
//     (the per-rock file index).
//
// The default implementation is manif.FileStore.
type ManifestStore interface {
	ReadTree(path string) (*Manifest, error)
	WriteTree(path string, m *Manifest) error
	ReadRock(path string) (*RockManifest, error)
	WriteRock(path string, rm *RockManifest) error
}
