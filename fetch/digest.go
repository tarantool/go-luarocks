package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// sha256HexLen is the length of a sha256 digest written in hex.
const sha256HexLen = 2 * sha256.Size

// errNotAFile is what a fetch that cannot be checked against Options.SHA256
// fails with: a digest names the bytes of one file, and a git repository or a
// local directory is not one.
var errNotAFile = errors.New("a sha256 is expected, but the source is not one file a digest can check")

// wantSHA256 returns opts.SHA256 lowercased, or an error when it is not a
// sha256 digest in hex, so that a malformed expectation fails before
// anything is fetched instead of failing every download as a mismatch. An
// empty one is no expectation.
func wantSHA256(opts Options) (string, error) {
	if opts.SHA256 == "" {
		return "", nil
	}

	want := strings.ToLower(opts.SHA256)

	if _, err := hex.DecodeString(want); err != nil || len(want) != sha256HexLen {
		return "", fmt.Errorf("fetch: Options.SHA256 %q is not a sha256 digest in hex", opts.SHA256)
	}

	return want, nil
}

// matchSHA256 compares the digest got of the file called name with want,
// a lowercase hex digest wantSHA256 returned.
func matchSHA256(name, want string, got []byte) error {
	if gotHex := hex.EncodeToString(got); gotHex != want {
		return fmt.Errorf("fetch: sha256 of %s is %s, want %s: %w", name, gotHex, want, rocks.ErrChecksumMismatch)
	}

	return nil
}

// checkSHA256 verifies the file at path against want, a lowercase hex digest
// wantSHA256 returned, and removes the file when it does not match, so a file
// that failed its check is never left under a name a later step would read.
// An empty want checks nothing.
func checkSHA256(path, want string) error {
	if want == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fetch: sha256 open %q: %w", path, err)
	}

	h := sha256.New()

	_, err = io.Copy(h, f)
	_ = f.Close()

	if err != nil {
		return fmt.Errorf("fetch: sha256 read %q: %w", path, err)
	}

	if err := matchSHA256(filepath.Base(path), want, h.Sum(nil)); err != nil {
		_ = os.Remove(path)

		return err
	}

	return nil
}
