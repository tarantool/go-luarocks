package testenv_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/internal/testenv"
)

//nolint:paralleltest // Hermetic sets the environment, which rules out t.Parallel.
func TestHermetic(t *testing.T) {
	home, homeSet := os.LookupEnv("HOME")
	tmp := os.Getenv("TMPDIR")

	// Nothing in this binary changes HOME before Hermetic does, so Original
	// must agree with the live environment here.
	orig, origSet := testenv.Original("HOME")
	assert.Equal(t, homeSet, origSet, "Original reports whether HOME was set")
	assert.Equal(t, home, orig, "Original(HOME)")

	t.Run("overrides", func(t *testing.T) {
		d := testenv.Hermetic(t)

		for key, dir := range map[string]string{
			"HOME":                d.Home,
			"XDG_CACHE_HOME":      d.CacheHome,
			"XDG_CONFIG_HOME":     d.ConfigHome,
			"XDG_DATA_HOME":       d.DataHome,
			"LUAROCKS_SYSCONFDIR": d.SysconfDir,
		} {
			assert.Equal(t, dir, os.Getenv(key), "%s", key)
			assert.DirExists(t, dir, "%s", key)
			assert.NotEqual(t, home, dir, "%s must not be the real home", key)
		}

		assert.True(t, strings.HasPrefix(d.CacheHome, d.Home+string(filepath.Separator)),
			"the XDG directories live in the hermetic home")

		entries, err := os.ReadDir(d.SysconfDir)
		require.NoError(t, err, "read SysconfDir")
		assert.Empty(t, entries, "SysconfDir holds no config file")

		assert.Equal(t, tmp, os.Getenv("TMPDIR"), "TMPDIR is left alone")

		orig, _ := testenv.Original("HOME")
		assert.Equal(t, home, orig, "Original still answers with the real home")
	})

	got, ok := os.LookupEnv("HOME")
	assert.Equal(t, homeSet, ok, "HOME is restored after the test")
	assert.Equal(t, home, got, "HOME is restored after the test")
}
