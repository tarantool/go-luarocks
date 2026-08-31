package remote

import (
	"context"
	"errors"
	"fmt"

	rocks "github.com/tarantool/go-luarocks"
)

// IndexOptions carries the per-server settings shared by the index
// implementations. Fields that do not apply to the selected index are
// ignored: a local directory has no TLS and no User-Agent.
type IndexOptions struct {
	// InsecureServers lists hostnames whose TLS certificates the HTTP index
	// should not verify.
	InsecureServers []string

	// UserAgent overrides the User-Agent header the HTTP index sends.
	UserAgent string

	// LuaVersion is the Lua dialect used to build the manifest filename.
	// Empty defaults to "5.1".
	LuaVersion string

	// Arch is the host arch string used to accept host-built binary rocks.
	// Empty defaults to the detected host arch.
	Arch string
}

// NewIndex builds the index appropriate for one configured rock server,
// selected by the server's form: FileRemoteIndex when the string names a
// local directory (see LocalServerPath for the rule), HTTPRemoteIndex
// otherwise.
//
// This is the constructor to call instead of assuming HTTP — it is what lets
// an offline mirror be configured anywhere an `https://` server is, the way
// upstream `luarocks --only-server=/path/to/repo` already allows.
//
//nolint:ireturn // dispatch factory: returns the RemoteIndex chosen for the server form
func NewIndex(server string, opts IndexOptions) rocks.RemoteIndex {
	if _, ok := LocalServerPath(server); ok {
		return &FileRemoteIndex{
			Server:     server,
			LuaVersion: opts.LuaVersion,
			Arch:       opts.Arch,
		}
	}

	return &HTTPRemoteIndex{
		Servers:         []string{server},
		InsecureServers: opts.InsecureServers,
		UserAgent:       opts.UserAgent,
		LuaVersion:      opts.LuaVersion,
		Arch:            opts.Arch,
	}
}

// NewIndexes builds one index per server, each selected by NewIndex, so a
// server list holding any mix of HTTP servers and local directories can be
// consulted in configuration order — typically by handing the result to
// NewOrderedIndex.
func NewIndexes(servers []string, opts IndexOptions) []rocks.RemoteIndex {
	out := make([]rocks.RemoteIndex, 0, len(servers))
	for _, server := range servers {
		out = append(out, NewIndex(server, opts))
	}

	return out
}

// OrderedIndex queries single-server indexes in order and returns the first
// server's results for a name (first-found-wins) rather than merging across
// servers the way a multi-server HTTPRemoteIndex does. A server that errors
// (unreachable host, missing directory) is skipped, and the last error is
// surfaced only when no server yielded a rock — so one dead mirror in the
// list cannot hide a rock the next one has.
//
// Nothing about the ordering depends on the transport: it composes
// rocks.RemoteIndex values, so mixing a local directory with HTTP servers
// needs no change here beyond building the members with NewIndexes.
type OrderedIndex struct {
	indexes []rocks.RemoteIndex
}

// NewOrderedIndex builds an ordered index over the given per-server indexes.
func NewOrderedIndex(indexes ...rocks.RemoteIndex) *OrderedIndex {
	return &OrderedIndex{indexes: indexes}
}

// Compile-time check that OrderedIndex satisfies rocks.RemoteIndex, so an
// aggregate can be nested inside another one.
var _ rocks.RemoteIndex = (*OrderedIndex)(nil)

// Query asks each index in order and returns the first non-empty result.
//
// An OrderedIndex with no members is a configuration error, not an empty
// answer: it stands for the configured server list, so reporting "no such
// rock" for a client that has nowhere to look would send the caller hunting a
// missing rock instead of a missing server. Same guard, and the same reason,
// as HTTPRemoteIndex's empty Servers.
func (o *OrderedIndex) Query(
	ctx context.Context, name, namespace string,
) ([]rocks.VersionedRock, error) {
	if len(o.indexes) == 0 {
		return nil, errors.New("remote.OrderedIndex: no servers configured")
	}

	var lastErr error

	for _, index := range o.indexes {
		found, err := index.Query(ctx, name, namespace)
		if err != nil {
			lastErr = err

			continue
		}

		if len(found) > 0 {
			return found, nil
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("query %q across servers: %w", name, lastErr)
	}

	return nil, nil
}
