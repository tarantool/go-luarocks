//go:build unix

package client_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/hostarch"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/remote"
)

// The lua backend named the host by what luarocks.core.sysdetect reads off
// the first architecture of /bin/sh. On macOS on Apple Silicon that is a
// universal binary listing x86_64 first, so cfg.arch was macosx-x86_64: an
// aarch64 rock was refused with "Incompatible architecture macosx-aarch64",
// x86_64 rocks were taken from servers and the rocks it packed were labelled
// x86_64. The tests here make sysdetect misread the host, so they show that
// on any host. They are unix only: a Windows cfg.arch is win32-<processor>,
// which HostArch does not spell.

// misdetectedProcessor is what sysdetect falls back to for a processor it
// cannot tell, and no GOARCH is named.
const misdetectedProcessor = "unknown"

// TestLuaEngine_ArchIsHostArch: cfg.arch of the lua backend is the arch the
// native backend gives the host, remote.HostArch, whatever sysdetect reads.
func TestLuaEngine_ArchIsHostArch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, system, processor string
	}{
		{"another processor", hostarch.System(), misdetectedProcessor},
		{"nothing detected", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, discardLogger())
			e.MisdetectHost(tc.system, tc.processor)

			arch, err := e.Config(context.Background(), client.ConfigOpts{Key: "arch"})
			require.NoError(t, err, "config arch")
			assert.Equal(t, remote.HostArch(), arch, "cfg.arch")
		})
	}
}

// TestLuaEngine_InstallsHostArchRock: the lua backend installs a binary rock
// of the host's arch from its file, as tt rocks install <file> does, when
// sysdetect misreads the processor.
func TestLuaEngine_InstallsHostArchRock(t *testing.T) {
	t.Parallel()

	// Upstream unpacks a .rock with the unzip program.
	if _, err := exec.LookPath("unzip"); err != nil {
		testenv.Missingf(t, "unzip not found on PATH; the lua backend unpacks a .rock with it: %v", err)
	}

	cfg := newLuaStubCfg(t, t.TempDir())
	rock := filepath.Join(t.TempDir(), "nat-1.0-1."+remote.HostArch()+".rock")
	writeZip(t, rock, map[string]string{
		"nat-1.0-1.rockspec": `package = "nat"
version = "1.0-1"
source = { url = "` + unreachableSource + `nat.git" }
build = { type = "builtin", modules = { nat = "nat.c" } }
`,
		"lib/nat.so": "not really ELF\n",
	})

	e := client.NewLuaEngine(cfg, manif.FileStore{}, discardLogger())
	e.MisdetectHost(hostarch.System(), misdetectedProcessor)

	require.NoError(t, e.Install(context.Background(), rock, client.InstallOpts{Deps: client.DepsNone}), "install %s", rock)
	requireFile(t, filepath.Join(cfg.Tree, "lib", "tarantool", "nat.so"), "not really ELF\n")
}
