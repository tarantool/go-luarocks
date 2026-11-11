package remote

import (
	"strings"
	"sync"
)

// ManifestCache shares parsed manifests between indexes, so a server's
// manifest is fetched and decoded once per cache rather than once per index.
// A client keeps one for its lifetime: the indexes built for the server list
// of a single call then reuse what an earlier call loaded, where each would
// otherwise fetch the manifest of every server again — once per rock when a
// resolved closure is installed rock by rock.
//
// It is safe for concurrent use. The zero value is ready to use, and a nil
// *ManifestCache caches nothing.
type ManifestCache struct {
	mu        sync.Mutex
	manifests map[manifestKey]*remoteManifest
}

// manifestKey names one cached manifest: the server as configured, the
// namespace, the Lua version whose manifest variants were probed, and the
// insecure hosts it was read with, which decide whether its certificate was
// checked and which http urls of a tt-rocks-index.json it accepts.
type manifestKey struct {
	server     string
	namespace  string
	luaVersion string
	insecure   string
}

// newManifestKey is the manifestKey of a manifest read with the insecure
// hosts insecure.
func newManifestKey(server, namespace, luaVersion string, insecure []string) manifestKey {
	return manifestKey{
		server:     server,
		namespace:  namespace,
		luaVersion: luaVersion,
		insecure:   strings.Join(insecure, "\x00"),
	}
}

func (c *ManifestCache) get(key manifestKey) (*remoteManifest, bool) {
	if c == nil {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	m, ok := c.manifests[key]

	return m, ok
}

func (c *ManifestCache) put(key manifestKey, m *remoteManifest) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.manifests == nil {
		c.manifests = map[manifestKey]*remoteManifest{}
	}

	c.manifests[key] = m
}
