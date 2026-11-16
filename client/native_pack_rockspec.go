package client

import (
	"context"
	"crypto/md5" //nolint:gosec // source.md5 is the digest rockspecs give
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/fetch"
	"github.com/tarantool/go-luarocks/internal/safepath"
)

// rockspecSuffix ends the name of a rockspec, which Pack packs into a source
// rock rather than looking up as an installed rock, as upstream pack tells
// the two apart (cmd/pack.lua: rock:match(".*%.rockspec")).
const rockspecSuffix = ".rockspec"

// errNoSourceURL reports a rockspec packed into a source rock that names no
// sources to put in it.
var errNoSourceURL = errors.New("the rockspec has no source.url")

// packRockspec packs the rockspec at specPath, relative to e.cfg.WorkingDir
// unless absolute, into <package>-<version>.src.rock in e.cfg.WorkingDir and
// returns its path, as upstream pack.pack_source_rock does (pack.lua:25-63):
// the archive holds the rockspec, under <package>-<version>.rockspec, and its
// sources fetched as upstream fetch.fetch_sources(rockspec, false) leaves
// them, so a build from it fetches nothing. A git source is cloned at the
// source.tag or source.branch it names, without its .git, into the directory
// named like the repository; a local directory is copied into the directory
// its URL names; any other source, an archive or a single file, is downloaded
// as it is, under source.file when set, and checked against source.md5 when
// set. Those are the places a build from a source rock looks in
// (bundledSource), on either backend.
//
// The archive is written through a temporary file and renamed into place,
// and its entries carry no modification time, so packing the same rockspec at
// the same sources twice gives the same bytes.
func (e *nativeEngine) packRockspec(ctx context.Context, specPath string) (string, error) {
	if !filepath.IsAbs(specPath) {
		specPath = filepath.Join(e.cfg.WorkingDir, specPath)
	}

	spec, err := evalAndPrepare(specPath, e.cfg)
	if err != nil {
		return "", fmt.Errorf("rocks.Pack: %w", err)
	}

	// The archive and the rockspec in it are named after both.
	for _, part := range []string{spec.Package, spec.Version} {
		if err := safepath.Name(part); err != nil {
			return "", fmt.Errorf("rocks.Pack: %w", err)
		}
	}

	if spec.Source.URL == "" {
		return "", fmt.Errorf("rocks.Pack: %s: %w", specPath, errNoSourceURL)
	}

	stage, err := os.MkdirTemp(e.cfg.WorkingDir, "rocks-pack-*")
	if err != nil {
		return "", fmt.Errorf("rocks.Pack: mkdir tmp: %w", err)
	}

	defer func() { _ = os.RemoveAll(stage) }()

	nameVersion := spec.Package + "-" + spec.Version

	err = os.WriteFile(filepath.Join(stage, nameVersion+rockspecSuffix), spec.RawSource, packReadMode)
	if err != nil {
		return "", fmt.Errorf("rocks.Pack: %w", err)
	}

	if err := e.stageSources(ctx, spec, stage); err != nil {
		return "", fmt.Errorf("rocks.Pack: %s %s: %w", spec.Package, spec.Version, err)
	}

	outPath := filepath.Join(e.cfg.WorkingDir, nameVersion+".src.rock")

	if err := zipInto(outPath, stage); err != nil {
		return "", fmt.Errorf("rocks.Pack: zip %s: %w", outPath, err)
	}

	return outPath, nil
}

// stageSources fetches the sources of spec into stage as packRockspec lays
// them out.
func (e *nativeEngine) stageSources(ctx context.Context, spec *rocks.Rockspec, stage string) error {
	url := spec.Source.URL
	scheme, rest, hasScheme := strings.Cut(url, "://")

	if hasScheme && (scheme == "git" || strings.HasPrefix(scheme, "git+")) {
		// The git backend clones into stage/<repository>, the directory
		// upstream git.get_sources names after the URL too.
		_, err := fetch.Sources(ctx, url, stage, fetch.Options{
			InsecureServers: e.cfg.InsecureServers,
			Tag:             spec.Source.Tag,
			Branch:          spec.Source.Branch,
			Version:         spec.Version,
		})
		if err != nil {
			return fmt.Errorf("fetch %s: %w", url, err)
		}

		return nil
	}

	local := url
	if hasScheme {
		local = rest
	}

	if (!hasScheme || scheme == "file") && isDir(local) {
		dir, err := safepath.JoinEntry(stage, deduceBaseDir(url))
		if err != nil {
			return fmt.Errorf("source.url: %w", err)
		}

		if _, err := fetch.Sources(ctx, url, dir, fetch.Options{}); err != nil {
			return fmt.Errorf("fetch %s: %w", url, err)
		}

		return nil
	}

	return e.stageSourceFile(ctx, spec, stage)
}

// stageSourceFile downloads the single file spec.Source.URL names into stage,
// as it is, under source.file when set, and checks it against source.md5 when
// set.
func (e *nativeEngine) stageSourceFile(ctx context.Context, spec *rocks.Rockspec, stage string) error {
	got, err := fetch.File(ctx, spec.Source.URL, stage, fetch.Options{InsecureServers: e.cfg.InsecureServers})
	if err != nil {
		return fmt.Errorf("fetch %s: %w", spec.Source.URL, err)
	}

	if name := spec.Source.File; name != "" && name != filepath.Base(got) {
		renamed, err := safepath.JoinEntry(stage, name)
		if err != nil {
			return fmt.Errorf("source.file: %w", err)
		}

		if err := os.Rename(got, renamed); err != nil {
			return fmt.Errorf("source.file: %w", err)
		}

		got = renamed
	}

	if spec.Source.MD5 != "" {
		if err := checkFileMD5(got, spec.Source.MD5); err != nil {
			return err
		}
	}

	return nil
}

// checkFileMD5 fails unless the file at path has the md5 want, in hex.
func checkFileMD5(path, want string) error {
	f, err := os.Open(path) //nolint:gosec // a file of the pack's own staging dir
	if err != nil {
		return fmt.Errorf("md5: %w", err)
	}

	defer func() { _ = f.Close() }()

	h := md5.New() //nolint:gosec // source.md5 is the digest rockspecs give
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("md5: %w", err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s has md5 %s, and the rockspec gives %s",
			rocks.ErrChecksumMismatch, filepath.Base(path), got, want)
	}

	return nil
}

// isDir reports whether path names a directory.
func isDir(path string) bool {
	st, err := os.Stat(path)

	return err == nil && st.IsDir()
}

// zipInto zips the files of dir into outPath through a temporary file beside
// it, renamed into place once complete.
func zipInto(outPath, dir string) error {
	tmp, err := os.CreateTemp(filepath.Dir(outPath), "."+filepath.Base(outPath)+".*")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()
	_ = tmp.Close()

	if err := zipDir(tmpName, dir); err != nil {
		_ = os.Remove(tmpName)

		return err
	}

	// CreateTemp makes the file 0600; a rock is world-readable.
	if err := os.Chmod(tmpName, packedRockMode); err != nil {
		_ = os.Remove(tmpName)

		return err
	}

	if err := os.Rename(tmpName, outPath); err != nil {
		_ = os.Remove(tmpName)

		return err
	}

	return nil
}
