package fetch

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/internal/transport"
)

// defaultUserAgent is the User-Agent header sent by httpBackend when
// Options.UserAgent is empty.
const defaultUserAgent = "go-luarocks/0.1"

const (
	// httpStatusErrThreshold is the first status code treated as an error;
	// any response at or above it (4xx/5xx) fails the fetch.
	httpStatusErrThreshold = 400

	// httpClientTimeout bounds a single fetch end-to-end.
	httpClientTimeout = 2 * time.Minute

	// maxRedirects caps the redirect chain followed by the HTTP client.
	maxRedirects = 5
)

// httpBackend downloads rawURL via net/http and unpacks recognized
// archive formats (.zip, .tar.gz, .tgz, .tar, .rock, .src.rock) into destDir. Other content
// types are written to destDir/<basename-from-url> and destDir is
// returned as-is.
//
// What it returns is an unpack directory, never a source root
// (Result.SourceRoot is false): an archive was expanded into destDir, so the
// sources typically sit in a versioned subdirectory the caller still has to
// resolve — upstream's fetch.find_base_dir job (fetch.lua:233). A plain
// (non-archive) download lands beside nothing else, and the same resolution
// then correctly keeps destDir itself.
type httpBackend struct{}

// Fetch performs the GET, writes the body to a temp file under destDir,
// then unpacks if the URL's file extension indicates a supported archive.
//
// TLS certificates are verified, except for a request — each redirect hop
// judged by its own host — to a host opts.InsecureServers lists: the opt-in
// for tarantool's rocks server, whose certificate upstream distrusts too. A
// redirect from https to http fails.
func (httpBackend) Fetch(ctx context.Context, rawURL, destDir string, opts Options) (Result, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Result{}, fmt.Errorf("fetch.http: parse %q: %w", rawURL, err)
	}

	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return Result{}, fmt.Errorf("fetch.http: mkdir %q: %w", destDir, err)
	}

	// Upstream names the downloaded file rockspec.source.file when set, else
	// dir.base_name(url) (rockspecs.lua:125). Honoring source.file matters
	// when the URL path has no usable extension (e.g. .../download?ref=v1):
	// the saved name drives the extension-based unpack switch below.
	base := opts.File
	if base == "" {
		base = filepath.Base(u.Path)
	}

	if opts.File == "" && (base == "" || base == "/" || base == ".") {
		base = "download"
	}

	// Upstream writes the download wherever source.file points; here it
	// stays an entry of destDir.
	dst, err := safepath.JoinEntry(destDir, base)
	if err != nil {
		return Result{}, fmt.Errorf("fetch.http: source file: %w", err)
	}

	if err := httpDownload(ctx, rawURL, dst, opts); err != nil {
		return Result{}, err
	}

	// Verify source.md5 before unpacking, matching upstream get_sources.
	if err := checkMD5(dst, opts.MD5); err != nil {
		return Result{}, err
	}

	if err := unpackArchive(dst, destDir); err != nil {
		return Result{}, fmt.Errorf("fetch.http: unpack: %w", err)
	}

	return Result{Path: destDir, SourceRoot: false}, nil
}

// httpDownload GETs rawURL and writes the response body to dst. It is the
// transport half shared by httpBackend.Fetch and File: everything up to
// having the bytes on disk under their final name, and nothing about md5
// verification or unpacking, which are the callers' to decide.
//
// The body lands in a dotted `.part` file beside dst and is renamed into
// place only once it is complete, so an interrupted transfer never leaves a
// truncated artifact under a name a later run would trust.
func httpDownload(ctx context.Context, rawURL, dst string, opts Options) error {
	if _, err := url.Parse(rawURL); err != nil {
		return fmt.Errorf("fetch.http: parse %q: %w", rawURL, err)
	}

	client := buildHTTPClient(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("fetch.http: new request: %w", err)
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch.http: GET %s: %w", rawURL, err)
	}

	defer func() {
		transport.Drain(resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= httpStatusErrThreshold {
		return fmt.Errorf("fetch.http: GET %s: status %d", rawURL, resp.StatusCode)
	}

	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")

	// tmp is built from the caller-provided destination plus a sanitized
	// basename.
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("fetch.http: create %q: %w", tmp, err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return fmt.Errorf("fetch.http: copy body: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("fetch.http: close: %w", err)
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("fetch.http: rename: %w", err)
	}

	return nil
}

// unpackArchive extracts a recognized archive at path into destDir and
// removes the archive on success. When the extension is not a recognized
// archive format it returns nil without touching the file, leaving a plain
// (non-archive) file in place.
//
// The recognized set mirrors upstream fs.unpack_archive: .zip/.rock (zip),
// .tar.gz/.tgz (gzip+tar), .tar.bz2/.tbz2/.tbz (bzip2+tar), and plain .tar.
func unpackArchive(path, destDir string) error {
	lower := strings.ToLower(filepath.Base(path))

	var extract func(string, string) error

	switch {
	// `.rock`/`.src.rock` archives are zip files under a different extension —
	// upstream luarocks unpacks them the same way (a .src.rock bundles the
	// rockspec plus the source).
	case strings.HasSuffix(lower, ".zip"), strings.HasSuffix(lower, ".rock"):
		extract = unzip
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		extract = untargz
	case strings.HasSuffix(lower, ".tar.bz2"),
		strings.HasSuffix(lower, ".tbz2"),
		strings.HasSuffix(lower, ".tbz"):
		extract = untarbz2
	case strings.HasSuffix(lower, ".tar"):
		extract = untar
	default:
		return nil
	}

	if err := extract(path, destDir); err != nil {
		return err
	}

	_ = safepath.Remove(destDir, path)

	return nil
}

// buildHTTPClient returns the client of a download: each request, every
// redirect hop included, is verified unless its own host is one of
// opts.InsecureServers, and a redirect from https to http fails.
func buildHTTPClient(opts Options) *http.Client {
	return transport.NewClient(opts.InsecureServers, httpClientTimeout, maxRedirects)
}

// entryPath is the path the archive entry name extracts to under dst: inside
// it, with a leading "/" dropped as tar and unzip drop it, and an error
// wrapping rocks.ErrUnsafePath for a name that reaches above dst.
func entryPath(dst, name string) (string, error) {
	p, err := safepath.Join(dst, strings.TrimLeft(filepath.FromSlash(name), string(filepath.Separator)))
	if err != nil {
		return "", fmt.Errorf("archive entry: %w", err)
	}

	return p, nil
}

// The extractors below create every directory and file through safepath, so
// an entry beneath a symlink pointing out of dst — one dst already held, since
// they extract no symlink themselves — fails instead of writing outside, and
// they keep only the permission bits of an entry, never setuid, setgid or
// sticky.

// unzip extracts as Info-ZIP's unzip does, a file keeping the permission
// bits an archive made on Unix records (safepath.Unzip).
func unzip(archive, dst string) error {
	return safepath.Unzip(archive, dst, dirPerm)
}

func untargz(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}

	defer func() { _ = gz.Close() }()

	return extractTar(tar.NewReader(gz), dst)
}

func untarbz2(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	// The stdlib bzip2 package is decompress-only, which is all extraction
	// needs; there is no gz-style Close to defer.
	return extractTar(tar.NewReader(bzip2.NewReader(f)), dst)
}

func untar(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	return extractTar(tar.NewReader(f), dst)
}

func extractTar(tr *tar.Reader, dst string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		target, err := entryPath(dst, hdr.Name)
		if err != nil {
			return err
		}

		// hdr.Mode is a tar mode word; only its permission bits are kept.
		perm := os.FileMode(uint32(hdr.Mode)).Perm()

		switch hdr.Typeflag {
		case tar.TypeDir:
			err := safepath.MkdirAll(dst, target, perm|ownerSearchBit)
			if err != nil {
				return err
			}
		case tar.TypeReg:
			if err := safepath.MkdirAll(dst, filepath.Dir(target), dirPerm); err != nil {
				return err
			}

			out, err := safepath.Create(dst, target, perm)
			if err != nil {
				return err
			}

			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()

				return err
			}

			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Skip links: rocks sources shouldn't need them in v1, and
			// blindly creating symlinks crosses safety lines for archive
			// extraction.
			continue
		}
	}
}
