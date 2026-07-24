package build_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestSubstituteVars(t *testing.T) {
	t.Parallel()

	vars := map[string]string{
		"LUA_INCDIR": "/opt/tt/include",
		"PREFIX":     "/tree/rocks/foo/1.0-1",
	}

	cases := map[string]string{
		"-I$(LUA_INCDIR)":                "-I/opt/tt/include",
		"--prefix=$(PREFIX)":             "--prefix=/tree/rocks/foo/1.0-1",
		"$(LUA_INCDIR)/x $(PREFIX)":      "/opt/tt/include/x /tree/rocks/foo/1.0-1",
		"no placeholders":                "no placeholders",
		"-D$(UNKNOWN)-flag":              "-D-flag", // unmatched → empty (glr-0uv)
		"literal $notvar and ${alsonot}": "literal $notvar and ${alsonot}",
	}
	for in, want := range cases {
		assert.Equal(t, want, build.SubstituteVars(in, vars), "SubstituteVars(%q)", in)
	}
}

func TestBuildVars_InstallPaths(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{Package: "foo", Version: "1.0-1"}
	cfg := rocks.Config{
		Tree:      "/tree",
		Tarantool: rocks.TarantoolConfig{IncludeDir: "/opt/tt/include", Prefix: "/opt/tt"},
	}
	vars := build.BuildVars(spec, cfg)

	assert.Equal(t, "/tree/share/tarantool/rocks/foo/1.0-1", vars["PREFIX"])
	assert.Equal(t, "/tree/share/tarantool/rocks/foo/1.0-1/lib", vars["LIBDIR"])
	assert.Equal(t, "/tree/share/tarantool/rocks/foo/1.0-1/lua", vars["LUADIR"])
	assert.Equal(t, "/opt/tt/include", vars["LUA_INCDIR"])
}
