package build_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestDeriveFlags_Linux(t *testing.T) {
	t.Setenv("CC", "")
	t.Setenv("CFLAGS", "")

	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/tt",
			IncludeDir: "/opt/tt/include/tarantool",
		},
	}
	f := build.DeriveFlagsFor(cfg, "linux")
	assert.Equal(t, "cc", f.CC)

	// CFLAGS unset → defaults to -O2, then -fPIC and the -I entry (glr-naf).
	wantCFLAGS := []string{"-O2", "-fPIC", "-I/opt/tt/include/tarantool"}
	assert.Equal(t, wantCFLAGS, f.CFLAGS)

	wantLIBFLAG := []string{"-shared"}
	assert.Equal(t, wantLIBFLAG, f.LIBFLAG)
	assert.Equal(t, ".so", f.Ext)
	assert.True(t, f.GccRpath, "GccRpath want true on linux") // glr-b7a
	assert.Equal(t, "/opt/tt/include/tarantool", f.LuaIncDir)
	assert.Equal(t, "/opt/tt/lib", f.LuaLibDir)
	assert.Equal(t, "/opt/tt/bin", f.LuaBinDir)
}

func TestDeriveFlags_CFLAGSFromEnv(t *testing.T) {
	t.Setenv("CC", "")
	t.Setenv("CFLAGS", "-DENABLE_FOO -O0")

	cfg := rocks.Config{Tarantool: rocks.TarantoolConfig{IncludeDir: "/x"}}
	f := build.DeriveFlagsFor(cfg, "linux")
	// glr-naf: env CFLAGS are preserved, -fPIC appended, -I last.
	assert.Equal(t, []string{"-DENABLE_FOO", "-O0", "-fPIC", "-I/x"}, f.CFLAGS)
}

func TestDeriveFlags_LDFLAGSFromEnv(t *testing.T) {
	t.Setenv("CC", "")
	t.Setenv("CFLAGS", "")
	t.Setenv("LDFLAGS", "-Wl,--no-undefined -L/opt/lib")

	f := build.DeriveFlagsFor(rocks.Config{}, "linux")
	// glr-08l: env LDFLAGS flow into f.LDFLAGS (upstream cfg.lua:377).
	assert.Equal(t, []string{"-Wl,--no-undefined", "-L/opt/lib"}, f.LDFLAGS)
}

func TestDeriveFlags_LDFLAGSEmptyWhenUnset(t *testing.T) {
	t.Setenv("CC", "")
	t.Setenv("LDFLAGS", "")

	f := build.DeriveFlagsFor(rocks.Config{}, "linux")
	assert.Empty(t, f.LDFLAGS)
}

func TestDeriveFlags_Darwin(t *testing.T) {
	t.Setenv("CC", "")

	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/tt",
			IncludeDir: "/opt/tt/include/tarantool",
		},
	}
	f := build.DeriveFlagsFor(cfg, "darwin")
	wantLIBFLAG := []string{"-bundle", "-undefined", "dynamic_lookup", "-all_load"}
	assert.Equal(t, wantLIBFLAG, f.LIBFLAG)
	assert.False(t, f.GccRpath, "GccRpath want false on darwin") // glr-b7a
	// Resolved UK2: macOS output extension is still .so, not .dylib.
	assert.Equal(t, ".so", f.Ext, "Ext want .so on darwin")
	// No -Wl,-rpath on darwin.
	for _, ldf := range f.LDFLAGS {
		if ldf != "" && (len(ldf) > 5 && ldf[:5] == "-Wl,-") {
			t.Errorf("unexpected LDFLAGS entry on darwin: %q", ldf)
		}
	}
}

func TestDeriveFlags_CCOverride(t *testing.T) {
	t.Setenv("CC", "clang")

	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{IncludeDir: "/x"},
	}
	f := build.DeriveFlagsFor(cfg, "linux")
	assert.Equal(t, "clang", f.CC, "CC want clang (from env)")
}

func TestDeriveFlags_NoIncludeDir(t *testing.T) {
	t.Setenv("CC", "")

	t.Setenv("CFLAGS", "")

	cfg := rocks.Config{} // no Tarantool fields set
	f := build.DeriveFlagsFor(cfg, "linux")
	// CFLAGS defaults to -O2 + -fPIC but no -I.
	assert.Equal(t, []string{"-O2", "-fPIC"}, f.CFLAGS)
	assert.Empty(t, f.LuaIncDir, "LuaIncDir want empty")
	assert.Empty(t, f.LuaLibDir, "LuaLibDir want empty")
}
