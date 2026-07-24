package build_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestBuiltin_CCInvocation_SingleSource(t *testing.T) {
	// Drive the CC builder through a stub `cc` and verify the argv.
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	stubCC := `#!/bin/sh
echo "args: $@" > "` + dir + `/cc.log"
# Touch the output file so the test can detect success.
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then mkdir -p "$(dirname "$out")" && : > "$out"; fi
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cc"), stubCC))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cc")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "src/foo/bar.c"), "/* stub */\n"))
	destDir := filepath.Join(dir, "dest")

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo.bar": {Path: "src/foo/bar.c"},
			},
		},
	}
	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{IncludeDir: "/opt/tt/include/tarantool"},
	}

	t.Setenv("PATH", stubDir)
	t.Setenv("CC", "") // force default "cc" so the stub catches it

	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, cfg), "builtin failed")

	data, err := build.ReadFile(filepath.Join(dir, "cc.log"))
	require.NoError(t, err, "missing cc log")

	log := string(data)

	mustContain := []string{
		"-fPIC",
		"-I/opt/tt/include/tarantool",
		"-o " + filepath.Join(destDir, "lib", "foo", "bar.so"),
		filepath.Join(srcDir, "src/foo/bar.c"),
	}

	for _, m := range mustContain {
		assert.Contains(t, log, m, "cc log missing %q", m)
	}
	// No -llua / -lluajit linkage (resolved UK2).
	assert.NotContains(t, log, "-llua ", "unexpected -llua linkage")
	assert.NotContains(t, log, "-llua\n", "unexpected -llua linkage")
	assert.NotContains(t, log, "-lluajit", "unexpected -lluajit linkage")
}

func TestBuiltin_CCInvocation_TableForm(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	stubCC := `#!/bin/sh
echo "args: $@" > "` + dir + `/cc.log"
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cc"), stubCC))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cc")))

	srcDir := filepath.Join(dir, "src")
	for _, s := range []string{"a.c", "b.c"} {
		require.NoError(t, build.WriteFile(filepath.Join(srcDir, s), "/* */\n"))
	}

	destDir := filepath.Join(dir, "dest")

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"thing": {
					Sources:   []string{"a.c", "b.c"},
					Incdirs:   []string{"/extra/inc", "$(LUA_INCDIR)/sub"},
					Libdirs:   []string{"/extra/lib"},
					Libraries: []string{"crypto"},
					Defines:   []string{"DEBUG=1"},
				},
			},
		},
	}
	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{IncludeDir: "/opt/tt/include/tarantool"},
	}

	t.Setenv("PATH", stubDir)
	t.Setenv("CC", "")

	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, cfg), "builtin failed")

	data, _ := build.ReadFile(filepath.Join(dir, "cc.log"))

	log := string(data)
	for _, m := range []string{
		"-DDEBUG=1",
		"-I/extra/inc",
		"-I/opt/tt/include/tarantool/sub", // glr-0uv: $(LUA_INCDIR) expanded
		"-L/extra/lib",
		"-lcrypto",
		filepath.Join(srcDir, "a.c"),
		filepath.Join(srcDir, "b.c"),
	} {
		assert.Contains(t, log, m, "cc log missing %q", m)
	}

	// glr-b7a: each libdir gets a -Wl,-rpath, entry on unix/linux (gcc_rpath
	// on) but not on macOS.
	if runtime.GOOS == "darwin" {
		assert.NotContains(t, log, "-Wl,-rpath,", "no rpath on darwin")
	} else {
		assert.Contains(t, log, "-Wl,-rpath,/extra/lib", "rpath expected for libdir on unix")
	}
}

func TestBuiltin_LuaModuleOnlyDoesNotRunCC(t *testing.T) {
	t.Parallel()

	// .lua-only modules must not invoke cc — proves we don't gate on
	// IncludeDir when nothing needs it.
	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "lib/foo.lua"), "return 1\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo": {Path: "lib/foo.lua"},
			},
		},
	}
	// No IncludeDir set — should not error since no compile happens.
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "lua-only build must not need headers")
	assert.True(t, build.FileExists(filepath.Join(destDir, "lua/foo.lua")), "missing destDir/lua/foo.lua")
}

func TestBuiltin_InitLuaModulePreservesPackageLayout(t *testing.T) {
	t.Parallel()

	// Upstream builtin.lua keeps an init.lua source at <name>/init.lua rather
	// than collapsing it into <name>.lua (glr-kxg).
	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "foo/init.lua"), "return 1\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo": {Path: "foo/init.lua"},
			},
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}))
	assert.True(t, build.FileExists(filepath.Join(destDir, "lua/foo/init.lua")), "expected destDir/lua/foo/init.lua")
	assert.False(t, build.FileExists(filepath.Join(destDir, "lua/foo.lua")), "must not collapse init.lua into foo.lua")
}

func TestBuiltin_AutodetectModules(t *testing.T) {
	t.Parallel()

	// glr-gcu: a format-3.0 rockspec with no build.modules autodetects modules
	// by scanning the source tree (src/ prefix), plus copy_directories.
	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "src/foo.lua"), "return 1\n"))
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "src/foo/bar.lua"), "return 2\n"))
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "doc/README"), "readme\n"))

	spec := &rocks.Rockspec{
		RockspecFormat: "3.0",
		Build:          rocks.Build{Type: "builtin"}, // no modules
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}))

	assert.True(t, build.FileExists(filepath.Join(destDir, "lua/foo.lua")), "module foo not autodetected")
	assert.True(t, build.FileExists(filepath.Join(destDir, "lua/foo/bar.lua")), "module foo.bar not autodetected")
	assert.Contains(t, spec.Build.CopyDirectories, "doc", "doc must be autodetected as a copy_directory")
}

func TestBuiltin_AutodetectSkippedPreFormat3(t *testing.T) {
	t.Parallel()

	// Autodetection only kicks in for rockspec_format >= 3.0.
	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "src/foo.lua"), "return 1\n"))

	spec := &rocks.Rockspec{Build: rocks.Build{Type: "builtin"}} // format 1.0
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}))

	assert.False(t, build.FileExists(filepath.Join(destDir, "lua/foo.lua")), "must not autodetect under format 1.0")
}

func TestBuiltin_InstallLibKeepsSourceBasename(t *testing.T) {
	t.Parallel()

	// Upstream install_to (is_module_path) places a prebuilt lib under
	// module_to_path(key) keeping the source basename — no forced .so, no
	// rename to the module's last component (glr-jf1).
	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "prebuilt/libc.so"), "\x7fELF stub\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Install: rocks.BuildInstall{
				Lib: map[string]string{"a.b": "prebuilt/libc.so"},
			},
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}))
	assert.True(t, build.FileExists(filepath.Join(destDir, "lib/a/libc.so")), "expected destDir/lib/a/libc.so (source basename preserved)")
	assert.False(t, build.FileExists(filepath.Join(destDir, "lib/a/b.so")), "must not rename to module last-segment .so")
}

func TestBuiltin_StringModuleNonCExtensionCompiles(t *testing.T) {
	// A string module whose extension is not .lua compiles as a single C/C++
	// source (glr-iet); .cpp/.cc/.cxx must not fall through to "empty entry".
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	stubCC := `#!/bin/sh
echo "args: $@" > "` + dir + `/cc.log"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then mkdir -p "$(dirname "$out")" && : > "$out"; fi
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cc"), stubCC))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cc")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "src/foo.cpp"), "// stub\n"))
	destDir := filepath.Join(dir, "dest")

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo": {Path: "src/foo.cpp"},
			},
		},
	}
	cfg := rocks.Config{Tarantool: rocks.TarantoolConfig{IncludeDir: "/opt/tt/include/tarantool"}}

	t.Setenv("PATH", stubDir)
	t.Setenv("CC", "")

	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, cfg), "cpp string module must compile")

	data, err := build.ReadFile(filepath.Join(dir, "cc.log"))
	require.NoError(t, err, "cc was not invoked for .cpp module")
	assert.Contains(t, string(data), filepath.Join(srcDir, "src/foo.cpp"), "cc log missing the .cpp source")
}
