package tree_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tarantool/go-luarocks/tree"
)

func TestPaths(t *testing.T) {
	t.Parallel()

	p := tree.Paths{Tree: "/tmp/r"}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"RocksDir", p.RocksDir(), filepath.Join("/tmp/r", "share", "tarantool", "rocks")},
		{"InstallDir", p.InstallDir("metrics", "0.17.0-1"),
			filepath.Join("/tmp/r", "share", "tarantool", "rocks", "metrics", "0.17.0-1")},
		{"DeployLuaDir", p.DeployLuaDir(), filepath.Join("/tmp/r", "share", "tarantool")},
		{"DeployLibDir", p.DeployLibDir(), filepath.Join("/tmp/r", "lib", "tarantool")},
		{"BinDir", p.BinDir(), filepath.Join("/tmp/r", "bin")},
		{"ConfDir", p.ConfDir("metrics", "0.17.0-1"),
			filepath.Join("/tmp/r", "share", "tarantool", "rocks", "metrics", "0.17.0-1", "conf")},
		{"DocDir", p.DocDir("metrics", "0.17.0-1"),
			filepath.Join("/tmp/r", "share", "tarantool", "rocks", "metrics", "0.17.0-1", "doc")},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.got, c.name)
	}
}
