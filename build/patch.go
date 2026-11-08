package build

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/safepath"
)

// This file is a port of upstream's patch tool, luarocks/tools/patch.lua
// (David Manura's LuaPatch, after python-patch 8.06-1), which applies the
// build.patches of a rockspec. It keeps the tool's semantics, quirks
// included, as the differential test against the vendored Lua module checks:
// lines keep their terminators, a hunk is looked for at its line, then at
// every line from the top with up to two lines of fuzz, a file whose hunks do
// not all match is accepted when it reads as already patched, and the last
// context line of a hunk is never compared. Every file it reads, writes,
// renames or removes is resolved under the source dir and stays inside it,
// where upstream follows any name the patch gives.

// errPatchCrash marks a condition under which the Lua tool raises a runtime
// error (indexing a missing hunk, reading past the end of the file being
// patched) rather than reporting a failure.
var errPatchCrash = errors.New("patch tool error")

// patchHunk is one hunk of a patch: the line ranges of its header and its
// lines, each starting with ' ', '-', '+' or '\'.
type patchHunk struct {
	startsrc, linessrc, starttgt, linestgt int
	invalid                                bool
	text                                   []string
}

// clone is table_copy of a hunk: a shallow copy sharing nothing a later
// change to the original could reach (the tool gives the original a new
// text table for each hunk).
func (h *patchHunk) clone() *patchHunk {
	c := *h

	return &c
}

// lineEnds counts the line terminators of a file or a hunk.
type lineEnds struct{ lf, crlf, cr int }

// patchSet is what read_patch returns: per file, its source and target
// names, whether either carried an epoch timestamp, and its hunks.
type patchSet struct {
	source, target []string
	epoch          map[int]bool // 1-based file index
	hunks          [][]*patchHunk
	hunkends       []lineEnds
}

// splitLines is file_lines: s split after each "\r\n", "\n" or "\r", each
// line keeping its terminator; a last line without one is kept as it is.
func splitLines(s string) []string {
	var lines []string

	start := 0

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			lines = append(lines, s[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}

			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}

	if start < len(s) {
		lines = append(lines, s[start:])
	}

	return lines
}

// endlstrip is the tool's endlstrip: s without its trailing "\r" and "\n"
// characters.
func endlstrip(s string) string {
	return strings.TrimRight(s, "\r\n")
}

var (
	// The hunk header forms match_linerange tries, in order.
	rangeFull  = regexp.MustCompile(`^@@ -(\d+),(\d+) \+(\d+),(\d+)`)
	rangeNoSrc = regexp.MustCompile(`^@@ -(\d+) \+(\d+),(\d+)`)
	rangeNoTgt = regexp.MustCompile(`^@@ -(\d+),(\d+) \+(\d+)`)
	rangeBare  = regexp.MustCompile(`^@@ -(\d+) \+(\d+)`)
	// sourceRe and targetRe are the file name lines; a name ends at the
	// first blank, as GNU patch takes it.
	sourceRe = regexp.MustCompile(`^--- ([^ \t\r\n]+)((?s).*)`)
	targetRe = regexp.MustCompile(`^\+\+\+ ([^ \t\r\n]+)((?s).*)$`)
	// epochRe is match_epoch: a 1969 or 1970 year in the timestamp.
	epochRe = regexp.MustCompile(`[^0-9]19(69|70)[^0-9]`)
	// blankRe matches a line of line terminators only.
	blankRe = regexp.MustCompile(`^[\r\n]*$`)
)

// matchLinerange is match_linerange: the start and length fields of a hunk
// header, a missing length as "".
func matchLinerange(line string) (m1, m2, m3, m4 string, ok bool) {
	if m := rangeFull.FindStringSubmatch(line); m != nil {
		return m[1], m[2], m[3], m[4], true
	}

	if m := rangeNoSrc.FindStringSubmatch(line); m != nil {
		return m[1], "", m[2], m[3], true
	}

	if m := rangeNoTgt.FindStringSubmatch(line); m != nil {
		return m[1], m[2], m[3], "", true
	}

	if m := rangeBare.FindStringSubmatch(line); m != nil {
		return m[1], "", m[2], "", true
	}

	return "", "", "", "", false
}

// luaNumber is tonumber of a string of digits, saturating where Lua would
// lose precision.
func luaNumber(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return int(^uint(0) >> 1)
	}

	return n
}

// patchState is the region of the patch read_patch is in.
type patchState int

const (
	// stateHeader is the comments before the patch body.
	stateHeader patchState = iota
	// stateFilenames is the "---" and "+++" lines.
	stateFilenames
	// stateHunkHead is an "@@ -R +R @@" line.
	stateHunkHead
	// stateHunkBody is the lines of a hunk.
	stateHunkBody
	// stateHunkSkip is after a hunk, or skipping an invalid one.
	stateHunkSkip
)

// count counts the line terminator of line.
func (e *lineEnds) count(line string) {
	switch {
	case strings.HasSuffix(line, "\r\n"):
		e.crlf++
	case strings.HasSuffix(line, "\n"):
		e.lf++
	case strings.HasSuffix(line, "\r"):
		e.cr++
	}
}

// patchReader is the state of read_patch.
type patchReader struct {
	state                patchState
	allOK                bool
	files                *patchSet
	nextfileno           int
	nexthunkno           int
	hunkinfo             *patchHunk
	actualSrc, actualTgt int
}

// readPatch is read_patch: it parses the unified diff data and reports
// whether all of it was well formed.
func readPatch(data string) (*patchSet, bool, error) {
	r := &patchReader{
		state:    stateHeader,
		allOK:    true,
		files:    &patchSet{epoch: map[int]bool{}},
		hunkinfo: &patchHunk{},
	}

	for _, line := range splitLines(data) {
		if err := r.line(line); err != nil {
			return nil, false, err
		}
	}

	if r.state != stateHunkSkip {
		r.allOK = false
	}

	return r.files, r.allOK, nil
}

// line reads one line of the patch, passing it through the states in the
// order the tool's loop body does.
func (r *patchReader) line(line string) error {
	if r.state == stateHeader && strings.HasPrefix(line, "--- ") {
		r.state = stateFilenames
	}

	if r.state == stateHunkBody {
		if err := r.hunkBody(line); err != nil {
			return err
		}
	}

	if r.state == stateHunkSkip {
		if _, _, _, _, ok := matchLinerange(line); ok {
			r.state = stateHunkHead
		} else if strings.HasPrefix(line, "--- ") {
			r.state = stateFilenames
		}
	}

	advance := false

	if r.state == stateFilenames {
		advance = r.filenames(line)
	}

	if !advance && r.state == stateHunkHead {
		r.hunkHead(line)
	}

	return nil
}

// hunkBody reads a line of a hunk.
func (r *patchReader) hunkBody(line string) error {
	if blankRe.MatchString(line) {
		// An empty line is a context line.
		line = " " + line
	}

	if line != "" && strings.ContainsRune("- +\\", rune(line[0])) {
		if r.nextfileno < 1 {
			return errPatchCrash
		}

		r.files.hunkends[r.nextfileno-1].count(line)

		switch line[0] {
		case '-':
			r.actualSrc++
		case '+':
			r.actualTgt++
		case '\\':
		default:
			r.actualSrc++
			r.actualTgt++
		}

		r.hunkinfo.text = append(r.hunkinfo.text, line)
	} else {
		if err := r.files.addHunk(r.nextfileno, r.nexthunkno, r.hunkinfo, true); err != nil {
			return err
		}

		r.allOK = false
		r.state = stateHunkSkip
	}

	// The exit conditions are checked whatever the line was.
	switch {
	case r.actualSrc > r.hunkinfo.linessrc || r.actualTgt > r.hunkinfo.linestgt:
		if err := r.files.addHunk(r.nextfileno, r.nexthunkno, r.hunkinfo, true); err != nil {
			return err
		}

		r.state = stateHunkSkip
	case r.hunkinfo.linessrc == r.actualSrc && r.hunkinfo.linestgt == r.actualTgt:
		if err := r.files.addHunk(r.nextfileno, r.nexthunkno, r.hunkinfo, false); err != nil {
			return err
		}

		r.state = stateHunkSkip
	}

	return nil
}

// filenames reads a "---" or "+++" line and reports whether it was a target
// line, which the hunk header state must not see again.
func (r *patchReader) filenames(line string) bool {
	switch {
	case strings.HasPrefix(line, "--- "):
		// The tool's check for a repeated source line compares names with a
		// number, so it never fires.
		m := sourceRe.FindStringSubmatch(line)
		if m == nil {
			r.allOK = false
			r.state = stateHeader

			return false
		}

		if epochRe.MatchString(m[2]) {
			r.files.epoch[r.nextfileno+1] = true
		}

		r.files.source = append(r.files.source, m[1])
	case !strings.HasPrefix(line, "+++ "):
		// "skipping invalid target patch", without failing the read.
		r.state = stateHeader
	default:
		m := targetRe.FindStringSubmatch(line)
		if m == nil {
			r.allOK = false
			r.state = stateHeader

			return false
		}

		r.files.target = append(r.files.target, m[1])
		r.nextfileno++

		if epochRe.MatchString(m[2]) {
			r.files.epoch[r.nextfileno] = true
		}

		r.nexthunkno = 0
		r.files.hunks = append(r.files.hunks, nil)
		r.files.hunkends = append(r.files.hunkends, lineEnds{})
		r.state = stateHunkHead

		return true
	}

	return false
}

// hunkHead reads a hunk header line.
func (r *patchReader) hunkHead(line string) {
	m1, m2, m3, m4, ok := matchLinerange(line)
	if !ok {
		// "skipping invalid patch with no hunks": the tool's check for hunks
		// compares tables with a number, so it always fails the read.
		r.allOK = false
		r.state = stateHeader

		return
	}

	if m2 == "" {
		m2 = "1"
	}

	if m4 == "" {
		m4 = "1"
	}

	r.hunkinfo = &patchHunk{
		startsrc: luaNumber(m1), linessrc: luaNumber(m2),
		starttgt: luaNumber(m3), linestgt: luaNumber(m4),
	}
	r.actualSrc, r.actualTgt = 0, 0
	r.state = stateHunkBody
	r.nexthunkno++
}

// addHunk appends a copy of h to the hunks of file fileno (1-based) and,
// for invalid, marks hunk hunkno of the file invalid, as the tool does by
// index; a missing one is a Lua runtime error there.
func (files *patchSet) addHunk(fileno, hunkno int, h *patchHunk, invalid bool) error {
	if fileno < 1 || fileno > len(files.hunks) {
		return errPatchCrash
	}

	files.hunks[fileno-1] = append(files.hunks[fileno-1], h.clone())

	if invalid {
		if hunkno < 1 || hunkno > len(files.hunks[fileno-1]) {
			return errPatchCrash
		}

		files.hunks[fileno-1][hunkno-1].invalid = true
	}

	return nil
}

// patchFS is the source dir the tool works in: every name resolves under
// root and stays there.
type patchFS struct{ root string }

// path resolves name, relative to the source dir or absolute inside it.
func (p patchFS) path(name string) (string, error) {
	if filepath.IsAbs(name) {
		if _, err := safepath.Rel(p.root, name); err != nil {
			return "", err
		}

		return filepath.Clean(name), nil
	}

	return safepath.Join(p.root, name)
}

// exists is the tool's exists: whether name opens for reading, which a
// directory does too.
func (p patchFS) exists(name string) (bool, error) {
	if name == "" {
		return false, nil
	}

	full, err := p.path(name)
	if err != nil {
		return false, err
	}

	f, err := safepath.Open(p.root, full)
	if err != nil {
		if errors.Is(err, rocks.ErrUnsafePath) {
			return false, err
		}

		return false, nil
	}

	_ = f.Close()

	return true, nil
}

// read returns the contents of name; a directory reads as empty, as the
// tool's reads of one fail.
func (p patchFS) read(name string) (string, error) {
	full, err := p.path(name)
	if err != nil {
		return "", err
	}

	f, err := safepath.Open(p.root, full)
	if err != nil {
		return "", err
	}

	defer func() { _ = f.Close() }()

	b, err := io.ReadAll(f)
	if err != nil {
		if _, ok := errors.AsType[*fs.PathError](err); ok {
			return "", nil
		}

		return "", err
	}

	return string(b), nil
}

// write creates or truncates name with data; its directory must exist, as
// io.open needs it to.
func (p patchFS) write(name, data string) error {
	full, err := p.path(name)
	if err != nil {
		return err
	}

	f, err := safepath.Create(p.root, full, filePerm)
	if err != nil {
		return err
	}

	if _, err := io.WriteString(f, data); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}

// absoluteName is fs.absolute_name: name under the source dir.
func (p patchFS) absoluteName(name string) string {
	if filepath.IsAbs(name) {
		return name
	}

	return filepath.Join(p.root, name)
}

// filePerm is the mode io.open creates a file with, before the umask.
const filePerm os.FileMode = 0o666

// stripDirs is strip_dirs: name without its first strip leading
// directories.
func stripDirs(name string, strip int) string {
	for range strip {
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
	}

	return name
}

// applyPatchSet is patch.apply_patch: it patches every file of ps, stripping
// strip leading directories from the names, and reports whether all of them
// applied. createDelete lets a patch create a file from /dev/null or an
// epoch timestamp and delete one to an epoch timestamp, as rockspec format
// 3.0 allows.
func (p patchFS) applyPatchSet(ps *patchSet, strip int, createDelete bool) (bool, error) {
	allOK := true

	for i, source := range ps.source {
		if i >= len(ps.target) || i >= len(ps.hunks) {
			return false, errPatchCrash
		}

		ok, err := p.patchFile(source, ps.target[i], ps.epoch[i+1], ps.hunks[i], strip, createDelete)
		if err != nil {
			return false, err
		}

		allOK = allOK && ok
	}

	return allOK, nil
}

// patchFile is patch_file.
func (p patchFS) patchFile(source, target string, epoch bool, hunks []*patchHunk, strip int, createDelete bool) (bool, error) {
	if createDelete {
		isSrcEpoch := epoch && len(hunks) == 1 && hunks[0].startsrc == 0 && hunks[0].linessrc == 0
		if isSrcEpoch || source == "/dev/null" {
			if len(hunks) == 0 {
				return false, errPatchCrash
			}

			return p.writeNewFile(p.absoluteName(stripDirs(target, strip)), hunks[0])
		}
	}

	source = stripDirs(source, strip)
	f2patch := source

	found, err := p.exists(f2patch)
	if err != nil {
		return false, err
	}

	if !found {
		f2patch = p.absoluteName(stripDirs(target, strip))

		if found, err = p.exists(f2patch); err != nil || !found {
			return false, err
		}
	}

	source = f2patch

	content, err := p.read(source)
	if err != nil {
		return false, err
	}

	file := splitLines(content)

	if createDelete && epoch && len(hunks) == 1 && hunks[0].starttgt == 0 && hunks[0].linestgt == 0 {
		full, err := p.path(source)
		if err != nil {
			return false, err
		}

		return safepath.Remove(p.root, full) == nil, nil
	}

	for hno, h := range hunks {
		findHunk(file, h, hno+1)
	}

	canpatch, ok := matchHunks(file, hunks)
	if !ok {
		return false, nil
	}

	if !canpatch {
		return true, nil
	}

	backup := source + ".orig"

	if exists, err := p.exists(backup); err != nil || exists {
		return false, err
	}

	srcPath, err := p.path(source)
	if err != nil {
		return false, err
	}

	backupPath, err := p.path(backup)
	if err != nil {
		return false, err
	}

	if err := safepath.Rename(p.root, srcPath, backupPath); err != nil {
		return false, nil //nolint:nilerr // the tool reports a failed rename as a failed patch
	}

	patched, err := patchHunks(file, hunks)
	if err != nil {
		return false, err
	}

	if err := p.write(source, patched); err != nil {
		return false, err
	}

	_ = safepath.Remove(p.root, backupPath)

	return true, nil
}

// matchHunks is the validation loop of patch_file: it walks the file and
// checks each hunk where find_hunk placed it. It reports whether the file
// is ready to be patched and, when it is not, whether that is still a
// success — the file reads as already patched.
func matchHunks(file []string, hunks []*patchHunk) (canpatch, ok bool) {
	hunkno := 1

	var (
		hunk       *patchHunk
		hunkfind   []string
		hunklineno int
		validhunks int
	)

	if len(hunks) > 0 {
		hunk = hunks[0]
	}

	processLine := func(line string, lineno int) bool {
		if hunk == nil || lineno < hunk.startsrc {
			return false
		}

		if lineno == hunk.startsrc {
			hunkfind = nil

			for _, x := range hunk.text {
				if x[0] == ' ' || x[0] == '-' {
					hunkfind = append(hunkfind, endlstrip(x[1:]))
				}
			}

			hunklineno = 1
		}

		last := hunk.startsrc + len(hunkfind) - 1

		if lineno < last {
			if hunklineno >= 1 && hunklineno <= len(hunkfind) && endlstrip(line) == hunkfind[hunklineno-1] {
				hunklineno++
			} else {
				// The file may be patched already; the other hunks are
				// checked anyway.
				hunkno++
				if hunkno > len(hunks) {
					return true
				}

				hunk = hunks[hunkno-1]

				return false
			}
		}

		if lineno == last {
			hunkno++
			validhunks++

			if hunkno <= len(hunks) {
				hunk = hunks[hunkno-1]
			} else if validhunks == len(hunks) {
				canpatch = true

				return true
			}
		}

		return false
	}

	done := false

	for i, line := range file {
		if done = processLine(line, i+1); done {
			break
		}
	}

	if !done && hunkno <= len(hunks) {
		// "premature end of source file".
		return false, false
	}

	if validhunks < len(hunks) && !checkPatched(file, hunks) {
		// "source file is different".
		return false, false
	}

	return canpatch, true
}

// maxFuzz is how many context lines find_hunk may drop from each end of a
// hunk.
const maxFuzz = 2

// findHunk is find_hunk: it looks for h at its line, then at every line
// from the top, with no, one and two lines of fuzz, and moves h to where it
// matched, dropping the fuzz lines from both its ends.
func findHunk(file []string, h *patchHunk, _ int) bool {
	for fuzz := range maxFuzz + 1 {
		lineno := h.startsrc

		for i := 0; i <= len(file); i++ {
			found := true
			location := lineno

			for l, hline := range h.text {
				if l+1 <= fuzz || (hline[0] != ' ' && hline[0] != '-') {
					continue
				}

				if lineno < 1 || lineno > len(file) || file[lineno-1] == "" {
					found = false

					break
				}

				line := file[lineno-1]
				lineno++

				if endlstrip(line) != endlstrip(hline[1:]) {
					found = false

					break
				}
			}

			if found {
				offset := location - h.startsrc - fuzz
				h.startsrc = location
				h.starttgt += offset

				for range fuzz {
					if len(h.text) > 0 {
						h.text = h.text[1:]
					}

					if len(h.text) > 0 {
						h.text = h.text[:len(h.text)-1]
					}
				}

				return true
			}

			lineno = i
		}
	}

	return false
}

// checkPatched is check_patched: whether every hunk's target side is in the
// file already. A hunk reaching past the end of the file raises an error in
// the tool, which its pcall reports as a match.
func checkPatched(file []string, hunks []*patchHunk) bool {
	if len(file) == 0 {
		return false
	}

	for _, h := range hunks {
		if len(file) < h.starttgt {
			return false
		}

		lineno := h.starttgt

		for _, hline := range h.text {
			if hline[0] == '-' || hline[0] == '\\' {
				continue
			}

			if lineno < 1 || lineno > len(file) {
				return true
			}

			line := file[lineno-1]
			lineno++

			if endlstrip(line) != endlstrip(hline[1:]) {
				return false
			}
		}
	}

	return true
}

// patchHunks is patch_hunks: the patched contents of the file whose lines
// are src. A line the patch adds takes the file's line terminator when the
// lines copied before it used one kind only.
func patchHunks(src []string, hunks []*patchHunk) (string, error) {
	var out strings.Builder

	next := 0
	readline := func() (string, bool) {
		if next >= len(src) {
			return "", false
		}

		next++

		return src[next-1], true
	}

	srclineno := 1

	var ends lineEnds

	for _, h := range hunks {
		for srclineno < h.startsrc {
			line, ok := readline()
			if !ok {
				return "", errPatchCrash
			}

			ends.count(line)

			out.WriteString(line)

			srclineno++
		}

		for _, hline := range h.text {
			if hline[0] == '-' || hline[0] == '\\' {
				readline()

				srclineno++

				continue
			}

			if hline[0] != '+' {
				readline()

				srclineno++
			}

			out.WriteString(withLineEnd(hline[1:], ends))
		}
	}

	for {
		line, ok := readline()
		if !ok {
			break
		}

		out.WriteString(line)
	}

	return out.String(), nil
}

// withLineEnd returns line with the one line terminator ends counts, or as
// it is when ends counts none or several kinds.
func withLineEnd(line string, ends lineEnds) string {
	kinds := 0
	newline := ""

	for _, k := range []struct {
		n  int
		nl string
	}{{ends.lf, "\n"}, {ends.crlf, "\r\n"}, {ends.cr, "\r"}} {
		if k.n > 0 {
			kinds++
			newline = k.nl
		}
	}

	if kinds != 1 {
		return line
	}

	return endlstrip(line) + newline
}

// writeNewFile is write_new_file: it creates name from the lines of hunk.
func (p patchFS) writeNewFile(name string, hunk *patchHunk) (bool, error) {
	full, err := p.path(name)
	if err != nil {
		return false, err
	}

	f, err := safepath.Create(p.root, full, filePerm)
	if err != nil {
		if errors.Is(err, rocks.ErrUnsafePath) {
			return false, err
		}

		return false, nil
	}

	defer func() { _ = f.Close() }()

	for _, hline := range hunk.text {
		if c := hline[0]; c != '+' && c != '-' && c != ' ' {
			// "malformed patch", with what was written so far kept.
			return false, nil
		}

		if _, err := io.WriteString(f, hline[1:]); err != nil {
			return false, err
		}
	}

	return true, nil
}

// applyPatch reads the patch data named name and applies it in srcDir, as
// upstream fs.apply_patch(name, data, createDelete) does, with one leading
// directory stripped from the names in it.
func applyPatch(srcDir, name, data string, createDelete bool) error {
	ps, allOK, err := readPatch(data)

	switch {
	case err != nil:
		return fmt.Errorf("build: reading patch %s: %w", name, err)
	case !allOK:
		return fmt.Errorf("build: failed reading patch %s", name)
	}

	ok, err := patchFS{root: srcDir}.applyPatchSet(ps, 1, createDelete)
	if err != nil {
		return fmt.Errorf("build: failed applying patch %s: %w", name, err)
	}

	if !ok {
		return fmt.Errorf("build: failed applying patch %s", name)
	}

	return nil
}

// patchesMarker is the file upstream's apply_patches leaves in the sources
// once it has applied the patches, so that a second build of the same
// sources (a Make run again in the working copy) applies them no more.
const patchesMarker = ".luarocks.patches.applied"

// applyPatches is upstream build.apply_patches (build.lua:48-81). When the
// rockspec has build.extra_files or build.patches and the sources hold no
// .luarocks.patches.applied marker, it writes each extra file and each patch
// file into srcDir under its name, applies the patches in name order —
// letting them create and delete files for rockspec_format 3.0 — and leaves
// the marker. A patch that does not apply fails the build, with the patches
// before it applied and no marker written. Every name is resolved under
// srcDir, and one that leaves it fails with rocks.ErrUnsafePath, where
// upstream writes and patches wherever the name leads.
func applyPatches(spec *rocks.Rockspec, srcDir string) error {
	if len(spec.Build.ExtraFiles) == 0 && len(spec.Build.Patches) == 0 {
		return nil
	}

	marker := filepath.Join(srcDir, patchesMarker)
	if _, err := safepath.Lstat(srcDir, marker); err == nil {
		return nil
	}

	for _, files := range []map[string]string{spec.Build.ExtraFiles, spec.Build.Patches} {
		for _, name := range slices.Sorted(maps.Keys(files)) {
			if err := (patchFS{root: srcDir}).write(name, files[name]); err != nil {
				return fmt.Errorf("build: write %s: %w", name, err)
			}
		}
	}

	createDelete := rockspecFormatAtLeast3(spec.RockspecFormat)

	for _, name := range slices.Sorted(maps.Keys(spec.Build.Patches)) {
		if err := applyPatch(srcDir, name, spec.Build.Patches[name], createDelete); err != nil {
			return err
		}
	}

	f, err := safepath.Create(srcDir, marker, filePerm)
	if err != nil {
		return fmt.Errorf("build: write %s: %w", patchesMarker, err)
	}

	return f.Close()
}
