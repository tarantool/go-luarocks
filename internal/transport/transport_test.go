package transport_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/internal/transport"
)

// TestFor pins which transport a host gets: one verifying transport shared by
// every host, and one that skips verification only for the listed hosts.
func TestFor(t *testing.T) {
	t.Parallel()

	insecure := []string{"rocks.tarantool.org"}

	verifying := transport.For("luarocks.org", insecure)
	assert.Same(t, verifying, transport.For("github.com", insecure), "one verifying transport")
	assert.Nil(t, verifying.TLSClientConfig, "the verifying transport keeps the default TLS config")
	assert.NotNil(t, verifying.Proxy, "proxies come from the environment")

	skipping := transport.For("ROCKS.tarantool.org", insecure)
	require.NotNil(t, skipping.TLSClientConfig, "the listed host skips verification")
	assert.True(t, skipping.TLSClientConfig.InsecureSkipVerify, "InsecureSkipVerify")
	assert.Same(t, skipping, transport.For("rocks.tarantool.org", insecure), "one skipping transport")
}
