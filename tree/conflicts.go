package tree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tarantool/go-luarocks/deps"
)

// resolveSpot implements upstream check_spot_if_available (repos.lua:248-268):
// given the item being deployed and the new rock's name/version, it consults
// the tree's current provider (t.Provider) and decides whether the NEW file
// takes the plain (active) path or a versioned name.
//
//   - "nv" (new wins the plain path) when there is no current provider, the new
//     name sorts lexicographically before the current one, or (same name) the
//     new version equals or is greater than the current one. In that case the
//     previously-active file — if it belongs to a DIFFERENT provider — is
//     renamed to that provider's versioned name (demoted), and any untracked
//     file already at the plain path is backed up rather than clobbered.
//   - "v"  (existing keeps priority) otherwise: the new file is written under
//     its own versioned name.
//
// It returns the final on-disk target for the new file.
func (t *Tree) resolveSpot(deployDir, plainTarget, item, newName, newVer string) (string, error) {
	curName, curVer, ok := t.currentProvider(item)

	nv := !ok ||
		newName < curName ||
		(newName == curName && (newVer == curVer || versionGreater(newVer, curVer)))

	if !nv {
		return MungedPath(deployDir, plainTarget, newName, newVer), nil
	}

	// New file wins the plain path. Preserve whatever is already there.
	if _, err := os.Stat(plainTarget); err == nil {
		switch {
		case ok && (curName != newName || curVer != newVer):
			// Demote the different provider's active file to its versioned name.
			demoted := MungedPath(deployDir, plainTarget, curName, curVer)
			if err := os.MkdirAll(filepath.Dir(demoted), dirPerm); err != nil {
				return "", err
			}

			if err := os.Rename(plainTarget, demoted); err != nil {
				return "", fmt.Errorf("demote %q: %w", plainTarget, err)
			}
		case !ok:
			// Untracked file (no provider recorded): back it up rather than lose it.
			if err := os.Rename(plainTarget, plainTarget+"~"); err != nil {
				return "", fmt.Errorf("backup %q: %w", plainTarget, err)
			}
		}
		// Same provider + same version: a reinstall — overwrite in place.
	}

	return plainTarget, nil
}

// currentProvider looks up the active provider of item via t.Provider, with the
// legacy ".init" fallback upstream keeps for backward compatibility (older
// LuaRocks registered "foo.init" files under "foo"; repos.lua:253-256).
func (t *Tree) currentProvider(item string) (name, version string, ok bool) {
	if t.Provider == nil {
		return "", "", false
	}

	if n, v, found := t.Provider(item); found {
		return n, v, true
	}

	if base, cut := strings.CutSuffix(item, ".init"); cut {
		return t.Provider(base)
	}

	return "", "", false
}

// versionGreater reports whether version a is strictly greater than b under the
// LuaRocks version ordering. Unparseable versions compare as not-greater.
func versionGreater(a, b string) bool {
	av, err := deps.ParseVersion(a)
	if err != nil {
		return false
	}

	bv, err := deps.ParseVersion(b)
	if err != nil {
		return false
	}

	return deps.Compare(av, bv) > 0
}

// pathToModule mirrors upstream path.path_to_module: strip a trailing
// extension, turn slashes into dots, and trim leading/trailing dots.
// "foo/bar.lua" → "foo.bar", "metrics/init.lua" → "metrics.init".
func pathToModule(rel string) string {
	name := rel
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[:i]
	}

	name = strings.ReplaceAll(name, "/", ".")

	return strings.Trim(name, ".")
}

// mungedVersion returns the `<pkg>_<ver>` identifier with every `.` and `-`
// replaced by `_`, matching upstream `(name.."_"..version):gsub("%-","_")
// :gsub("%.","_")` (core/path.lua:33).
func mungedVersion(pkg, ver string) string {
	combined := pkg + "_" + ver
	combined = strings.ReplaceAll(combined, "-", "_")
	combined = strings.ReplaceAll(combined, ".", "_")

	return combined
}

// MungedPath returns the versioned_name form of target under deployDir: the
// munged `<pkg>_<ver>` token dash-joined onto the path remainder after the
// deploy-dir prefix (core/path.lua path.versioned_name). Exported because
// installer logic — selecting the active version among munged siblings — needs
// the same string formation that Deploy used.
func MungedPath(deployDir, target, pkg, ver string) string {
	rest := strings.TrimLeft(strings.TrimPrefix(target, deployDir), "/")

	return filepath.Join(deployDir, mungedVersion(pkg, ver)+"-"+rest)
}
