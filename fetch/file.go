package fetch

import (
	"context"
	"crypto/md5" //nolint:gosec // upstream luarocks source.md5 verification uses md5
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// dirPerm is the mode for directories created while copying a tree:
	// owner rwx, group rx, no world access.
	dirPerm = 0o750

	// ownerSearchBit forces the owner-search (execute) bit on directories
	// so we can descend into them even when the source mode omits it.
	ownerSearchBit = 0o100
)

// fileBackend implements file:// fetches by copying the source tree into
// destDir. We always copy (rather than returning the on-disk path
// directly) so that build steps which scribble into the working tree
// don't corrupt the user's local source — matches upstream luarocks's
// `fetch.fetch_local`.
type fileBackend struct{}

// Fetch strips the `file://` prefix, validates the source path, copies
// the tree into destDir, and returns the destination path.
func (fileBackend) Fetch(ctx context.Context, rawURL, destDir string, opts Options) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	src := strings.TrimPrefix(rawURL, "file://")

	st, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("fetch.file: stat %q: %w", src, err)
	}

	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return "", fmt.Errorf("fetch.file: mkdir %q: %w", destDir, err)
	}

	if !st.IsDir() {
		// Single file (e.g. a rockspec or archive). Copy into destDir under its
		// basename — or source.file when set (upstream fetch.fetch_local names it
		// rockspec.source.file or dir.base_name) — then extract if it is a
		// recognized archive (fetch.fetch_local → fs.unpack_archive).
		base := opts.File
		if base == "" {
			base = filepath.Base(src)
		}

		dst := filepath.Join(destDir, base)

		err := copyOneFile(src, dst, st.Mode().Perm())
		if err != nil {
			return "", err
		}

		if err := checkMD5(dst, opts.MD5); err != nil {
			return "", err
		}

		if err := unpackArchive(dst, destDir); err != nil {
			return "", fmt.Errorf("fetch.file: unpack: %w", err)
		}

		return destDir, nil
	}
	// Copy tree rooted at src into destDir.
	if err := copyDir(ctx, src, destDir); err != nil {
		return "", fmt.Errorf("fetch.file: %w", err)
	}

	return destDir, nil
}

func copyDir(ctx context.Context, src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|ownerSearchBit)
		}

		return copyOneFile(p, target, info.Mode().Perm())
	})
}

// checkMD5 verifies the file at path against the expected md5, mirroring
// upstream fs.check_md5 (fs/lua.lua:158-167): the check passes when the
// expected value is a (lowercase) anchored prefix of the computed hex digest,
// not on strict equality. An empty want skips verification.
func checkMD5(path, want string) error {
	if want == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fetch: md5 open %q: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	h := md5.New() //nolint:gosec // parity with upstream source.md5

	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("fetch: md5 read %q: %w", path, err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.HasPrefix(got, strings.ToLower(want)) {
		return fmt.Errorf("fetch: MD5 check for %s has failed", filepath.Base(path))
	}

	return nil
}

func copyOneFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(dst), err)
	}

	// src is a caller-supplied file:// path being copied into destDir; reading
	// the user's own local source tree is the documented purpose of this backend.
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return fmt.Errorf("copy %q->%q: %w", src, dst, err)
	}

	return out.Close()
}
