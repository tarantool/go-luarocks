package tree

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/rockspec"
)

// rockManifestName is the file name of a rock's rock_manifest, in its install
// dir and at the top of a binary rock.
const rockManifestName = "rock_manifest"

// StagedOpts tunes Tx.InstallStaged.
type StagedOpts struct {
	// RemoveOtherVersions removes every other installed version of the rock
	// before it is deployed, as upstream install does without --keep; the
	// native client keeps them.
	RemoveOtherVersions bool
	// RequirePlain refuses, with an error wrapping ErrNotPlain and before
	// anything changes, a rock a file of which would not take its plain path
	// because another rock holds it, so that `require` and the tree's bin
	// reach the rock's own files.
	RequirePlain bool
	// NoDisplace refuses, with an error wrapping ErrNotPlain and before
	// anything changes, a rock a file of which would take its plain path from
	// another rock — one of another name, which would keep its file at the
	// versioned path only — so that `require` and the tree's bin reach the
	// files they reached before for every other rock. The rock's own other
	// versions give their plain paths up as without it. With RequirePlain
	// too, every file of the rock is at a plain path no other rock had.
	NoDisplace bool
}

// InstallStaged deploys dir, laid out as an unpacked binary rock, as the rock
// spec.Package/spec.Version, as the native client installs a .rock: lua/ goes
// to DeployLuaDir, lib/ to DeployLibDir and bin/ to BinDir, each file at its
// plain or versioned path by the tree's conflict rules, a Lua command behind
// a wrapper when t.BinWrap is set and the rockspec does not turn wrapping
// off, and every other top-level directory and file into the rock's install
// dir — a rockspec and a rock_manifest at the top excepted. The rock's
// rock_manifest is written from what was deployed, and the rock recorded in
// the tree manifest as Tx.Deploy records it. An installed copy of the same
// version is removed first. It returns the rock_manifest.
//
// spec.RawSource is required: it becomes the installed rockspec, which the
// lua backend reads, and it must evaluate to a rockspec of spec's package and
// version, which must parse. The tree manifest records spec.Dependencies.
// Unlike a build, no default docs are installed: upstream unpacks a binary
// rock whole and copies nothing else.
//
// dir is read only inside itself: a symlink in it leading out fails the
// install with an error wrapping rocks.ErrUnsafePath. A dir that does not
// exist is a rock with no files, as an empty one is: its rockspec alone is
// installed, as for a rock that only gathers its dependencies.
func (tx *Tx) InstallStaged(spec *rocks.Rockspec, dir string, opts StagedOpts) (*rocks.RockManifest, error) {
	if tx.done {
		return nil, errTxDone
	}

	if err := checkStagedSpec(spec); err != nil {
		return nil, fmt.Errorf("tree.InstallStaged: %w", err)
	}

	deploySpec, err := binarySpec(spec, dir)
	if err != nil {
		return nil, fmt.Errorf("tree.InstallStaged: %w", err)
	}

	if opts.RequirePlain || opts.NoDisplace {
		if err := tx.checkStaged(spec, dir, opts); err != nil {
			return nil, fmt.Errorf("tree.InstallStaged: %w", err)
		}
	}

	if opts.RemoveOtherVersions {
		if err := tx.removeOtherVersions(spec.Package, []string{spec.Version}); err != nil {
			return nil, fmt.Errorf("tree.InstallStaged: %w", err)
		}
	}

	var rm *rocks.RockManifest

	err = tx.Deploy(deploySpec, func(t *Tree) (*rocks.RockManifest, error) {
		if err := t.copyTopFiles(deploySpec, dir); err != nil {
			return nil, err
		}

		var err error

		rm, err = t.deploy(deploySpec, dir, dir, false)

		return rm, err
	})
	if err != nil {
		return nil, fmt.Errorf("tree.InstallStaged: %s %s: %w", spec.Package, spec.Version, err)
	}

	return rm, nil
}

// checkStagedSpec checks the spec of InstallStaged: a rock name and version
// that are a directory each, a version that parses, and a RawSource that
// evaluates to a rockspec of the same package and version.
func checkStagedSpec(spec *rocks.Rockspec) error {
	if spec == nil {
		return errors.New("nil spec")
	}

	if err := checkNameVersion(spec.Package, spec.Version); err != nil {
		return err
	}

	if _, err := deps.ParseVersion(spec.Version); err != nil {
		return fmt.Errorf("rock version %q: %w", spec.Version, err)
	}

	if len(spec.RawSource) == 0 {
		return fmt.Errorf("%s %s: the spec has no RawSource to install as its rockspec", spec.Package, spec.Version)
	}

	own, err := rockspec.EvalSource(rockspecName(spec.Package, spec.Version), spec.RawSource, rocks.RockspecConfig{})
	if err != nil {
		return err
	}

	if own.Package != spec.Package || own.Version != spec.Version {
		return fmt.Errorf("%s %s: RawSource is the rockspec of %s %s", spec.Package, spec.Version, own.Package, own.Version)
	}

	return nil
}

// rockspecName is the file name of the rockspec of name/version in its
// install dir and in a rock.
func rockspecName(name, version string) string {
	return name + "-" + version + ".rockspec"
}

// wrapsCommands mirrors upstream repos.should_wrap_bin_scripts: a rock's Lua
// commands get wrappers unless its rockspec sets deploy.wrap_bin_scripts =
// false. (Upstream also honors a cfg-level wrap_bin_scripts, which the
// hardcoded Tarantool configuration lacks.)
func wrapsCommands(spec *rocks.Rockspec) bool {
	if spec.Deploy.WrapBinScripts != nil {
		return *spec.Deploy.WrapBinScripts
	}

	return true
}

// binarySpec returns the spec to deploy a binary rock unpacked into rockDir
// with: its own rockspec without the build it was made with, since its files
// are built already, plus a build.install.bin entry per bin/ script, so the
// scripts get the wrappers a built rock's get, and every other top-level
// directory (doc/, conf/, …) as a copy_directories entry, since upstream
// unpacks the whole rock into the per-rock install dir. Deploy relocates the
// lua/ and lib/ trees, and the commands below bin/, from rockDir as from a
// build dir.
func binarySpec(spec *rocks.Rockspec, rockDir string) (*rocks.Rockspec, error) {
	entries, err := readRockDir(rockDir)
	if err != nil {
		return nil, err
	}

	deploySpec := *spec
	deploySpec.Build = rocks.Build{CopyDirectories: []string{}}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		switch entry.Name() {
		case deployLua, deployLib:
		case deployBin:
			bins, err := os.ReadDir(filepath.Join(rockDir, deployBin))
			if err != nil {
				return nil, fmt.Errorf("read rock bin dir: %w", err)
			}

			deploySpec.Build.Install.Bin = map[string]string{}

			for _, bin := range bins {
				if !bin.IsDir() {
					deploySpec.Build.Install.Bin[bin.Name()] = filepath.Join(deployBin, bin.Name())
				}
			}
		default:
			deploySpec.Build.CopyDirectories = append(deploySpec.Build.CopyDirectories, entry.Name())
		}
	}

	return &deploySpec, nil
}

// readRockDir lists the unpacked binary rock rockDir: nothing when it does
// not exist, which is a rock with no files.
func readRockDir(rockDir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(rockDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read rock dir %s: %w", rockDir, err)
	}

	return entries, nil
}

// copyTopFiles copies every regular file at the top of the unpacked binary
// rock rockDir into the rock's install dir, keeping its permission bits, as
// upstream's unpack of the rock there does — the rockspec and the
// rock_manifest aside, which the deploy writes itself. Deploy's listing of the
// install dir then records them in the rock_manifest.
func (t *Tree) copyTopFiles(spec *rocks.Rockspec, rockDir string) error {
	entries, err := readRockDir(rockDir)
	if err != nil {
		return err
	}

	installDir := t.InstallDir(spec.Package, spec.Version)

	for _, e := range entries {
		if e.IsDir() || e.Name() == rockManifestName || e.Name() == rockspecName(spec.Package, spec.Version) {
			continue
		}

		if err := copyKeepMode(rockDir, filepath.Join(rockDir, e.Name()), t.RocksDir(), filepath.Join(installDir, e.Name())); err != nil {
			return fmt.Errorf("copy %s: %w", e.Name(), err)
		}
	}

	return nil
}

// copyKeepMode copies the file src, read inside srcRoot, to dst inside
// dstRoot, with src's permission bits, creating dst's directory; a symlink to
// a file inside srcRoot is copied as the file. It returns nothing for a
// directory, which it does not copy.
func copyKeepMode(srcRoot, src, dstRoot, dst string) error {
	in, err := safepath.Open(srcRoot, src)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	st, err := in.Stat()
	if err != nil {
		return err
	}

	if st.IsDir() {
		return fmt.Errorf("%s is a link to a directory", src)
	}

	if err := safepath.MkdirAll(dstRoot, filepath.Dir(dst), dirPerm); err != nil {
		return err
	}

	out, err := safepath.Create(dstRoot, dst, st.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	return safepath.Chmod(dstRoot, dst, st.Mode().Perm())
}

// removeOtherVersions removes every installed version of name but those of
// keep.
func (tx *Tx) removeOtherVersions(name string, keep []string) error {
	for _, version := range tx.View().Versions(name) {
		if slices.ContainsFunc(keep, func(k string) bool { return sameVersion(k, version) }) {
			continue
		}

		if err := tx.deleteVersion(name, version); err != nil {
			return fmt.Errorf("remove %s %s: %w", name, version, err)
		}
	}

	return nil
}

// checkStaged checks the binary rock of spec unpacked into dir against the
// plain-path options of opts, as Deploy would give its files their spots in
// the tree the install leaves: without spec's version, and without the
// rock's other versions when opts.RemoveOtherVersions is set.
func (tx *Tx) checkStaged(spec *rocks.Rockspec, dir string, opts StagedOpts) error {
	rm, err := stagedDeployed(dir)
	if err != nil {
		return err
	}

	m := cloneManifest(tx.m)
	for _, version := range tx.View().Versions(spec.Package) {
		if opts.RemoveOtherVersions || sameVersion(version, spec.Version) {
			removeFromManifest(m, spec.Package, version)
		}
	}

	if opts.RequirePlain {
		if err := checkStagedPlain(tx.tree.Paths, m, spec, rm); err != nil {
			return err
		}
	}

	if !opts.NoDisplace {
		return nil
	}

	addToManifest(m, spec, rm)

	return checkDisplaced(tx.tree.Paths, tx.m, m, []incoming{{ref: RockRef{Name: spec.Package, Version: spec.Version}, rm: rm}})
}

// stagedDeployed lists the files of the binary rock unpacked into dir that
// Deploy deploys — those under its lua/, lib/ and bin/ — in the sections of
// a rock_manifest, without their digests.
func stagedDeployed(dir string) (*rocks.RockManifest, error) {
	rm := &rocks.RockManifest{Lua: map[string]string{}, Lib: map[string]string{}, Bin: map[string]string{}}

	for _, sec := range deployedSections(rm) {
		sub := filepath.Join(dir, sec.dt)

		err := filepath.WalkDir(sub, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == sub {
					return nil
				}

				return err
			}

			if d.IsDir() {
				return nil
			}

			rel, err := filepath.Rel(sub, p)
			if err != nil {
				return err
			}

			sec.files[filepath.ToSlash(rel)] = ""

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", sub, err)
		}
	}

	return rm, nil
}

// checkStagedPlain fails with an error wrapping ErrNotPlain when a file of
// rm, the deployed files of spec's rock, would not take its plain path in the
// tree of p whose manifest is m, as Deploy decides it.
func checkStagedPlain(p Paths, m *rocks.Manifest, spec *rocks.Rockspec, rm *rocks.RockManifest) error {
	t := &Tree{Paths: p, Providers: manifestProviders(m)}

	var notPlain []string

	for _, sec := range deployedSections(rm) {
		for _, rel := range sortedKeys(sec.files) {
			if plain, curName, curVer := t.takesPlainPath(sec.dt, rel, spec.Package, spec.Version); !plain {
				notPlain = append(notPlain, sec.dt+"/"+rel+" (held by "+RockRef{Name: curName, Version: curVer}.String()+")")
			}
		}
	}

	if len(notPlain) > 0 {
		return fmt.Errorf("%w: %s %s: %s", ErrNotPlain, spec.Package, spec.Version, strings.Join(notPlain, ", "))
	}

	return nil
}

// deployedSection is a section of a rock_manifest whose files Deploy
// deploys: its deploy type and its files.
type deployedSection struct {
	dt    string
	files map[string]string
}

// deployedSections returns the deployed sections of rm: lua, lib and bin.
func deployedSections(rm *rocks.RockManifest) []deployedSection {
	return []deployedSection{{deployLua, rm.Lua}, {deployLib, rm.Lib}, {deployBin, rm.Bin}}
}

// incoming is a rock about to be installed and its rock_manifest, of which
// the deployed sections count.
type incoming struct {
	ref RockRef
	rm  *rocks.RockManifest
}

// checkPlainAfter fails with an error wrapping ErrNotPlain, naming each file
// and the rock holding it, when a file of a rock of in is not at its plain
// path in the tree of p whose manifest is after, the one installing in
// leaves: the rock is not the first provider of the item, as Deploy reads
// the provider lists.
func checkPlainAfter(p Paths, after *rocks.Manifest, in []incoming) error {
	now := &Tree{Paths: p, Providers: manifestProviders(after)}

	var notPlain []string

	for _, r := range in {
		var files []string

		for _, sec := range deployedSections(r.rm) {
			for _, rel := range sortedKeys(sec.files) {
				if name, version, _ := now.currentProvider(sec.dt, rel); name != r.ref.Name || version != r.ref.Version {
					files = append(files, sec.dt+"/"+rel+" (held by "+RockRef{Name: name, Version: version}.String()+")")
				}
			}
		}

		if len(files) > 0 {
			notPlain = append(notPlain, r.ref.Name+" "+r.ref.Version+": "+strings.Join(files, ", "))
		}
	}

	if len(notPlain) > 0 {
		return fmt.Errorf("%w: %s", ErrNotPlain, strings.Join(notPlain, "; "))
	}

	return nil
}

// checkDisplaced fails with an error wrapping ErrNotPlain, naming each file
// and the rock holding it, when installing the rocks in into the tree of p,
// whose manifest goes from before to after, takes the plain path of a file
// from another rock: one of before, of a name none of in has, that holds the
// plain path of an item a rock of in provides — the item's first provider,
// as Deploy reads the provider lists — and holds it no more after.
func checkDisplaced(p Paths, before, after *rocks.Manifest, in []incoming) error {
	names := map[string]bool{}
	for _, r := range in {
		names[r.ref.Name] = true
	}

	was := &Tree{Paths: p, Providers: manifestProviders(before)}
	now := &Tree{Paths: p, Providers: manifestProviders(after)}

	var taken []string

	for _, r := range in {
		var files []string

		for _, sec := range deployedSections(r.rm) {
			for _, rel := range sortedKeys(sec.files) {
				name, version, ok := was.currentProvider(sec.dt, rel)
				if !ok || names[name] {
					continue
				}

				if n, v, _ := now.currentProvider(sec.dt, rel); n == name && v == version {
					continue
				}

				files = append(files, sec.dt+"/"+rel+" (held by "+RockRef{Name: name, Version: version}.String()+")")
			}
		}

		if len(files) > 0 {
			taken = append(taken, r.ref.Name+" "+r.ref.Version+": "+strings.Join(files, ", "))
		}
	}

	if len(taken) > 0 {
		return fmt.Errorf("%w: other rocks would lose their plain paths: %s", ErrNotPlain, strings.Join(taken, "; "))
	}

	return nil
}

// readRockspec evaluates the rockspec at path, read inside root, as the
// native client evaluates one it installs (rockspec.EvalSource, then
// MergePlatforms for the running platform).
func readRockspec(root, path string) (*rocks.Rockspec, error) {
	f, err := safepath.Open(root, path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	src, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	spec, err := rockspec.EvalSource(path, src, rocks.RockspecConfig{})
	if err != nil {
		return nil, err
	}

	rockspec.MergePlatforms(spec, rockspec.RuntimePlatforms())

	return spec, nil
}
