package safepath

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
)

// The host a zip entry was made on, the high byte of its "version made by"
// (APPNOTE 4.4.2), whose external attributes hold a Unix mode in their high
// 16 bits.
const (
	zipHostUnix   = 3
	zipHostMacOSX = 19
	// zipHostShift is the shift of the host in the "version made by".
	zipHostShift = 8
)

// ownerRWX is the mode bits that let a directory's owner list, enter and
// change it.
const ownerRWX fs.FileMode = 0o700

// Unzip extracts the zip archive at archive inside root, as Info-ZIP's unzip,
// which upstream LuaRocks extracts a rock and a zip source with
// (fs/unix/tools.lua unzip), does, but that every entry stays inside root:
//
//   - an entry's name has its leading slashes dropped, and one that leaves
//     root, by ".." or through a symlink on the way, fails it with an error
//     wrapping ErrEscapes;
//   - a symlink entry is extracted as a regular file holding its target;
//   - a file made on Unix, which records its mode, gets the permission bits
//     it records, the umask not applied, as unzip restores them; a file made
//     elsewhere records none and is created 0666, or 0444 when it is
//     read-only, less the umask, as unzip creates it;
//   - setuid, setgid and sticky bits are never kept, as unzip keeps them
//     only with -K;
//   - a directory is created with dirPerm less the umask, and one the archive
//     holds an entry of made on Unix gets the bits it records once every
//     file is extracted, as unzip sets them last, but that its owner keeps
//     the right to list, enter and change it, so it can be removed.
func Unzip(archive, root string, dirPerm fs.FileMode) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}

	defer func() { _ = r.Close() }()

	type dirMode struct {
		path string
		perm fs.FileMode
	}

	var dirs []dirMode

	for _, f := range r.File {
		target, err := Join(root, strings.TrimLeft(filepath.FromSlash(f.Name), string(filepath.Separator)))
		if err != nil {
			return fmt.Errorf("archive entry: %w", err)
		}

		perm, recorded := zipEntryPerm(&f.FileHeader)

		if f.FileInfo().IsDir() {
			if err := MkdirAll(root, target, dirPerm); err != nil {
				return err
			}

			if recorded {
				dirs = append(dirs, dirMode{target, perm | ownerRWX})
			}

			continue
		}

		if err := MkdirAll(root, filepath.Dir(target), dirPerm); err != nil {
			return err
		}

		if err := unzipFile(root, target, f, perm, recorded); err != nil {
			return err
		}
	}

	for _, d := range dirs {
		if err := Chmod(root, d.path, d.perm); err != nil {
			return err
		}
	}

	return nil
}

// zipEntryPerm returns the permission bits of the zip entry h, setuid, setgid
// and sticky left out, and whether h records them, which only an entry made
// on Unix does: the bits of another are the defaults archive/zip gives it.
func zipEntryPerm(h *zip.FileHeader) (fs.FileMode, bool) {
	host := h.CreatorVersion >> zipHostShift

	return h.Mode().Perm(), host == zipHostUnix || host == zipHostMacOSX
}

// unzipFile writes the file entry f to target, inside root, with perm,
// exactly when recorded says f records it.
func unzipFile(root, target string, f *zip.File, perm fs.FileMode, recorded bool) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}

	defer func() { _ = rc.Close() }()

	out, err := Create(root, target, perm)
	if err != nil {
		return err
	}

	// Archive members are rocks and their sources, of bounded size; copying
	// the full entry is intended. A decompression-bomb limit would break
	// valid rocks.
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()

		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	if !recorded {
		return nil
	}

	return Chmod(root, target, perm)
}
