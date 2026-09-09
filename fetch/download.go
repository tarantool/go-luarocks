package fetch

// Raw single-file retrieval: upstream download.lua's get_file, which is
// deliberately not the source-fetch path the rest of this package implements.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// File retrieves the single file at rawURL into destDir under the basename
// the URL gives it, and returns the path it wrote. It is a port of
// download.lua's get_file (download.lua:11-25): a local path is copied with
// fs.copy, anything else is downloaded with fetch.fetch_url, and either way
// the file lands in the target directory under its own name.
//
// It is NOT Fetch, and the difference is the whole point. Fetch and Sources
// exist to make a rock's SOURCES available, so they expand every recognized
// archive into destDir and delete the archive — and a `.rock` is a zip, so
// fetching one leaves an unpacked tree and no rock file at all. `luarocks
// download` has to deliver the artifact itself, so File never unpacks and
// never inspects the extension.
//
// Only two Options fields are consulted, both about the transport:
// InsecureServers and UserAgent. File, MD5, Tag, Branch and Version describe
// a rockspec's `source` table, and there is no rockspec here — the name comes
// from the URL and nothing is verified against a digest the server did not
// supply.
//
// The accepted forms mirror get_file's dir.split_url dispatch: a schemeless
// path or a file:// URL is copied off disk, http(s) is downloaded. Any other
// scheme is ErrUnsupportedRockspecFeature — a git repository has no single
// file to hand back.
func File(ctx context.Context, rawURL, destDir string, opts Options) (string, error) {
	if rawURL == "" {
		return "", errors.New("fetch.File: empty URL")
	}

	if destDir == "" {
		return "", errors.New("fetch.File: empty destination directory")
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return "", fmt.Errorf("fetch.File: mkdir %q: %w", destDir, err)
	}

	// A string with no "://" is a local path, exactly as dir.split_url reads
	// it (core/dir.lua:41-51) — the form a local rock server's artifacts take.
	if !strings.Contains(rawURL, "://") {
		return copyFileInto(rawURL, destDir)
	}

	scheme, err := schemeOf(rawURL)
	if err != nil {
		return "", err
	}

	switch scheme {
	case "file":
		_, pathname, _ := strings.Cut(rawURL, "://")

		return copyFileInto(pathname, destDir)
	case "http", "https":
		return downloadFileInto(ctx, rawURL, destDir, opts)
	default:
		return "", fmt.Errorf("fetch.File: scheme %q: %w", scheme, rocks.ErrUnsupportedRockspecFeature)
	}
}

// copyFileInto copies one existing file into destDir under its basename,
// which is fs.copy(pathname, fs.current_dir(), "read") — fs.copy resolves a
// directory destination to a file of the source's own name (fs/lua.lua:424).
func copyFileInto(src, destDir string) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("fetch.File: stat %q: %w", src, err)
	}

	if st.IsDir() {
		return "", fmt.Errorf("fetch.File: %q is a directory, not a file", src)
	}

	dst := filepath.Join(destDir, filepath.Base(src))

	// Upstream refuses this outright rather than truncating the file it is
	// about to read (fs/lua.lua:427). Copying a file onto itself would empty
	// it, so a downloading into the server's own directory has to fail loudly.
	if dstStat, serr := os.Stat(dst); serr == nil && os.SameFile(st, dstStat) {
		return "", fmt.Errorf("fetch.File: the source and destination are the same file: %s", dst)
	}

	if err := copyOneFile(src, dst, st.Mode().Perm()); err != nil {
		return "", fmt.Errorf("fetch.File: %w", err)
	}

	return dst, nil
}

// downloadFileInto GETs rawURL and saves it in destDir under the last segment
// of the URL path, which is what fs.download does when fetch_url passes it no
// explicit filename (fetch.lua:136 derives dir.base_name(url)).
func downloadFileInto(ctx context.Context, rawURL, destDir string, opts Options) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("fetch.File: parse %q: %w", rawURL, err)
	}

	// path.Base, not filepath.Base: a URL path is slash-separated whatever the
	// host OS calls a separator.
	base := path.Base(u.Path)
	if base == "" || base == "/" || base == "." {
		return "", fmt.Errorf("fetch.File: URL %q names no file", rawURL)
	}

	dst := filepath.Join(destDir, base)
	if err := httpDownload(ctx, rawURL, dst, opts); err != nil {
		return "", err
	}

	return dst, nil
}
