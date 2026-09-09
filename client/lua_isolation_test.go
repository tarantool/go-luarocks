package client_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

// State isolation across dispatches (glr-x6t.10).
//
// Upstream LuaRocks assumes one process per command, so its module-level state
// is per-command state. The engine gives every dispatch its own VM for exactly
// that reason. These tests are the gate on that promise, and they test the
// CLASS rather than the symptoms: each case dirties one audited leak point
// through Contaminate — which runs a statement on a pooled VM and retires it
// the way a real dispatch does — and then asserts a later call cannot see it.
//
// A test here failing means state crossed a dispatch boundary, whatever the
// mechanism. Adding a case costs two lines, which matters because the leak
// points are upstream's and grow with each release.

// TestLuaEngine_StateDoesNotCrossDispatches covers the leak points that are
// readable from inside the VM.
func TestLuaEngine_StateDoesNotCrossDispatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// dirty is executed on one VM, which is then retired.
		dirty string
		// probe is evaluated on a later VM; want is what a clean engine says.
		probe string
		want  string
	}{
		{
			name:  "server list",
			dirty: `table.insert(require('luarocks.core.cfg').rocks_servers, 'http://leaked.example/')`,
			probe: `tostring(#require('luarocks.core.cfg').rocks_servers)`,
			want:  "1",
		},
		{
			name:  "no_manifest flag",
			dirty: `require('luarocks.core.cfg').no_manifest = true`,
			probe: `tostring(require('luarocks.core.cfg').no_manifest)`,
			want:  "false",
		},
		{
			name:  "verbose flag",
			dirty: `require('luarocks.core.cfg').verbose = true`,
			probe: `tostring(require('luarocks.core.cfg').verbose)`,
			want:  "nil",
		},
		{
			name:  "cmdline variables",
			dirty: `require('luarocks.core.cfg').variables.GLR_LEAKED = 'yes'`,
			probe: `tostring(require('luarocks.core.cfg').variables.GLR_LEAKED)`,
			want:  "nil",
		},
		{
			name:  "connection timeout",
			dirty: `require('luarocks.core.cfg').connection_timeout = 999`,
			probe: `tostring(require('luarocks.core.cfg').connection_timeout)`,
			want:  "30",
		},
		{
			name:  "manifest cache",
			dirty: `require('luarocks.core.manif').cache_manifest('http://leaked.example/', '5.1', {repository={}})`,
			probe: `tostring(require('luarocks.core.manif').get_cached_manifest('http://leaked.example/', '5.1'))`,
			want:  "nil",
		},
		{
			name:  "directory stack",
			dirty: `require('luarocks.fs').change_dir('/')`,
			probe: `require('luarocks.fs').current_dir()`,
			// The engine anchors this to WorkingDir; a surviving dir_stack would
			// leave the next call somewhere else entirely.
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			cfg := luaTestCfg(t)
			e := client.NewLuaEngine(cfg, manif.FileStore{}, discardLogger())

			want := c.want
			if c.name == "directory stack" {
				want = cfg.WorkingDir
			}

			// Establish the clean value first, so a case whose "want" is wrong
			// fails here rather than looking like a leak.
			require.Equal(t, want, luaProbe(t, e, c.probe), "baseline before contamination")

			require.NoError(t, e.Contaminate(c.dirty), "contaminate")

			require.Equal(t, want, luaProbe(t, e, c.probe),
				"state survived a dispatch boundary")
		})
	}
}

// TestLuaEngine_ScheduledFunctionsDoNotCrossDispatches covers the one leak
// point that cannot be read from inside the VM: upstream's
// util.run_scheduled_functions executes its list without clearing it
// (util.lua:60-70), so on a reused VM every later dispatch re-runs the cleanup
// and rollback functions of the ones before it. The list is a module-local, so
// the observable is what a re-run would DO — here, create a file.
func TestLuaEngine_ScheduledFunctionsDoNotCrossDispatches(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "rollback-ran")

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, discardLogger())

	require.NoError(t, e.Contaminate(
		`require('luarocks.util').schedule_function(function()
			local f = io.open(`+quoteLuaString(marker)+`, 'w')
			f:write('ran')
			f:close()
		 end)`), "schedule a rollback")

	// `list` is the dispatch to use here, not `help`: run_scheduled_functions
	// is the LAST statement of cmd.run_command, and `help` unwinds through
	// os.exit before reaching it — a test built on `help` cannot fail.
	require.NoError(t, e.Call([]string{"list"}), "later dispatch")

	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist,
		"a rollback scheduled by an earlier dispatch ran again in a later one")
}

// quoteLuaString wraps s as a Lua long-bracket string, which needs no escaping
// for the paths these tests build.
func quoteLuaString(s string) string {
	return "[[" + s + "]]"
}
