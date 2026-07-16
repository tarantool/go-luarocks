package manif_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/manif"
)

// --- Parse: lexer / escape / long-bracket edge cases not hit by the golden
// corpus or the existing reject-table --------------------------------------

// TestParseComments — persist.lua never emits comments, but the parser
// tolerates single-line `--` comments in hand-edited input, and treats a
// long-comment opener (`--[[` / `--[=[`) as outside the accepted subset.
func TestParseComments(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte("-- a leading comment\nx = 1 -- trailing comment\n"))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, int64(1), m["x"])

	// A long-comment opener is left unconsumed by skipWS and then fails to
	// parse as a top-level key.
	_, err = manif.Parse([]byte("--[[ long comment ]]\nx = 1\n"))
	require.Error(t, err, "long comments are outside the accepted subset")
}

// TestParseHexAndDecimalEscapes covers the \xHH and \DDD short-string escape
// paths, plus the byte-value overflow guard on the decimal form.
func TestParseHexAndDecimalEscapes(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte(`s = "\x41\x62\099"`))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Abc", m["s"], `\x41 \x62 \099 -> "Abc"`)

	_, err = manif.Parse([]byte(`bad = "\xzz"`))
	require.Error(t, err, "non-hex digits after \\x must fail")

	_, err = manif.Parse([]byte(`bad = "\999"`))
	require.Error(t, err, "decimal escape above 255 must fail")

	_, err = manif.Parse([]byte(`bad = "\q"`))
	require.Error(t, err, "unknown escape letter must fail")
}

// TestParseAllNamedEscapes exercises every named single-letter escape
// persist.lua's short-string writer can emit, plus the escaped-quote and
// line-continuation forms.
func TestParseAllNamedEscapes(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte(`s = "\a\b\f\n\r\t\v\\\"\'"`))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok)

	want := "\a\b\f\n\r\t\v\\\"'"
	assert.Equal(t, want, m["s"])

	// A backslash immediately followed by a literal newline is a line
	// continuation that inserts a bare '\n'.
	v, err = manif.Parse([]byte("s = \"line1\\\nline2\"\n"))
	require.NoError(t, err, "Parse")

	m, ok = v.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "line1\nline2", m["s"])
}

// TestParseShortStringUnterminated covers the two ways a short string can
// fail to close: a literal newline before the closing quote, and running out
// of input entirely (including mid-escape).
func TestParseShortStringUnterminated(t *testing.T) {
	t.Parallel()

	_, err := manif.Parse([]byte("x = \"abc\ndef\"\n"))
	require.Error(t, err, "a literal newline inside a short string must fail")

	_, err = manif.Parse([]byte(`x = "abc`))
	require.Error(t, err, "input ending before the closing quote must fail")

	_, err = manif.Parse([]byte(`x = "abc\`))
	require.Error(t, err, "a dangling backslash at end of input must fail")
}

// TestParseLongBracketVariants covers the long-bracket opener/closer probing
// beyond the golden corpus: a non-bracket '[', an unbalanced '[=' opener,
// each CR/LF/CRLF/LFCR leading-newline-strip form, and an unterminated
// bracket.
func TestParseLongBracketVariants(t *testing.T) {
	t.Parallel()

	_, err := manif.Parse([]byte("x = [notabracket]\n"))
	require.Error(t, err, "'[' not starting a long bracket must fail")

	_, err = manif.Parse([]byte("x = [=notabracket\n"))
	require.Error(t, err, "an unbalanced [= opener must fail")

	_, err = manif.Parse([]byte("x = [[unterminated\n"))
	require.Error(t, err, "a long bracket that never closes must fail")

	cases := map[string]string{
		"CRLF": "x = [[\r\nbody]]\n",
		"LFCR": "x = [[\n\rbody]]\n",
		"CR":   "x = [[\rbody]]\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			v, err := manif.Parse([]byte(src))
			require.NoError(t, err, "Parse(%s)", name)

			m, ok := v.(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "body", m["x"], "leading newline of any form must be stripped")
		})
	}
}

// TestParseNumberEdgeCases covers the float / exponent branches of
// parseNumber and its malformed-number errors.
func TestParseNumberEdgeCases(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte("x = 1.5e2\ny = -1.5e-2\nz = 2E3\n"))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok)
	assert.InEpsilon(t, 150.0, m["x"], 1e-9)
	assert.InEpsilon(t, -0.015, m["y"], 1e-9)
	assert.InEpsilon(t, 2000.0, m["z"], 1e-9)

	_, err = manif.Parse([]byte("x = -\n"))
	require.Error(t, err, "a lone '-' is not a number")

	_, err = manif.Parse([]byte("x = 1e\n"))
	require.Error(t, err, "an exponent marker with no digits must fail strconv.ParseFloat")
}

// TestParseValueDispatchEdgeCases covers the value-position dispatch
// branches not otherwise exercised: a bare `nil` literal and a byte that
// matches none of the value-start cases.
func TestParseValueDispatchEdgeCases(t *testing.T) {
	t.Parallel()

	_, err := manif.Parse([]byte("x = nil\n"))
	require.Error(t, err, "a bare nil literal is a hard error")

	_, err = manif.Parse([]byte("x = @\n"))
	require.Error(t, err, "a byte matching no value-start case must fail")
}

// TestParseTableEdgeCases covers the table-body error branches: a bracketed
// key missing its closing ']', a bracketed key missing '=', a bad value
// after a bracketed key, a bad value after an identifier key, a bad value in
// array position, a missing separator between entries, and the ident-key
// lookahead's '==' guard.
func TestParseTableEdgeCases(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"bracket key missing close":  "x = { [1 = 2 }\n",
		"bracket key missing equals": "x = { [1] 2 }\n",
		"bad value after bracket":    "x = { [1] = @ }\n",
		"bad value after ident key":  "x = { name = @ }\n",
		"bad value in array pos":     "x = { @ }\n",
		"missing separator":          "x = { 1 2 }\n",
		"double-equals not a key":    "x = { name == 1 }\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := manif.Parse([]byte(src))
			require.Error(t, err, "Parse(%q)", src)
		})
	}
}

// TestParseTableKeyShapes covers entriesToValue's key-shape classification:
// a sparse (non-dense) numeric table and a float-valued bracketed key, both
// of which fall through to the plain-map representation.
func TestParseTableKeyShapes(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte("x = { [1] = \"a\", [3] = \"b\" }\n"))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok)
	tbl, ok := m["x"].(map[string]any)
	require.True(t, ok, "sparse numeric table must decode to a plain map, got %T", m["x"])
	assert.Equal(t, "a", tbl["1"])
	assert.Equal(t, "b", tbl["3"])

	v, err = manif.Parse([]byte("y = { [1.5] = \"c\" }\n"))
	require.NoError(t, err, "Parse")

	m, ok = v.(map[string]any)
	require.True(t, ok)
	tbl, ok = m["y"].(map[string]any)
	require.True(t, ok, "float-keyed table must decode to a plain map, got %T", m["y"])
	assert.Equal(t, "c", tbl["1.5"])
}

// --- Write: numeric type coverage, error paths, key/value edge cases ------

// TestWriteNumericTypes drives every Go numeric type writeValue special-cases
// (the integer family renders without a decimal point; float32/64 go through
// formatNumber).
func TestWriteNumericTypes(t *testing.T) {
	t.Parallel()

	in := map[string]any{
		"i":    int(1),
		"i8":   int8(2),
		"i16":  int16(3),
		"i32":  int32(4),
		"i64":  int64(5),
		"u":    uint(6),
		"u8":   uint8(7),
		"u16":  uint16(8),
		"u32":  uint32(9),
		"u64":  uint64(10),
		"f32":  float32(1.5),
		"f64":  2.5,
		"nilv": nil,
	}

	var buf bytes.Buffer

	require.NoError(t, manif.Write(&buf, in))

	got := buf.String()
	for _, want := range []string{
		"i = 1", "i16 = 3", "i32 = 4", "i64 = 5", "i8 = 2",
		"u = 6", "u16 = 8", "u32 = 9", "u64 = 10", "u8 = 7",
		"f32 = 1.5", "f64 = 2.5", "nilv = nil",
	} {
		assert.Contains(t, got, want, "missing %q in:\n%s", want, got)
	}
}

// TestWriteFormatNumberSpecialValues covers formatNumber's NaN/Inf branches
// and the integer-valued-float short form.
func TestWriteFormatNumberSpecialValues(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	require.NoError(t, manif.Write(&buf, map[string]any{
		"nan":     math.NaN(),
		"posinf":  math.Inf(1),
		"neginf":  math.Inf(-1),
		"whole":   float64(3),
		"nonzero": 3.25,
	}))

	got := buf.String()
	assert.Contains(t, got, "nan = nan")
	assert.Contains(t, got, "posinf = inf")
	assert.Contains(t, got, "neginf = -inf")
	assert.Contains(t, got, "whole = 3\n", "an integer-valued float must render without a decimal point")
	assert.Contains(t, got, "nonzero = 3.25")
}

// TestWriteTopLevelErrors covers Write's top-level validation: a non-table
// value, a numeric top-level key, and an invalid plain-identifier top-level
// key (including a bare Lua keyword).
func TestWriteTopLevelErrors(t *testing.T) {
	t.Parallel()

	err := manif.Write(&bytes.Buffer{}, "not a table")
	require.Error(t, err, "a scalar top-level value must be rejected")

	err = manif.Write(&bytes.Buffer{}, []any{"a", "b"})
	require.Error(t, err, "a numeric (array) top-level key must be rejected")
	require.ErrorIs(t, err, manif.ErrInvalidTopLevelKey)

	err = manif.Write(&bytes.Buffer{}, map[string]any{"1bad": "x"})
	require.Error(t, err, "a key starting with a digit is not a valid identifier")
	require.ErrorIs(t, err, manif.ErrInvalidTopLevelKey)

	err = manif.Write(&bytes.Buffer{}, map[string]any{"end": "x"})
	require.Error(t, err, "a bare Lua keyword must be rejected as a top-level key")
	assert.ErrorIs(t, err, manif.ErrInvalidTopLevelKey)
}

// TestWriteMixedKeyMapErrors covers normalizeTable's map[any]any path,
// including its unsupported-key-type error, reached through a nested value
// (top-level keys are always strings, so this only surfaces one level down).
func TestWriteMixedKeyMapErrors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	require.NoError(t, manif.Write(&buf, map[string]any{
		"outer": map[any]any{"a": 1, int64(2): "b", int(3): "c"},
	}), "map[any]any with string/int/int64 keys must normalize cleanly")

	err := manif.Write(&bytes.Buffer{}, map[string]any{
		"outer": map[any]any{3.14: "unsupported key type"},
	})
	require.Error(t, err, "a float key in a map[any]any must be rejected")
}

// TestWriteStringEscaping covers luaQ's control-byte escaping (including the
// three-digit-vs-minimal-digit disambiguation) and the long-bracket fallback
// for strings containing CR/LF, plus its closing-bracket probe growing past
// zero equals signs when the naive marker already appears in the string.
func TestWriteStringEscaping(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	require.NoError(t, manif.Write(&buf, map[string]any{
		"ctrl":       "a\x01b",
		"ctrlDigit":  "a\x019b", // next byte is a digit -> forces 3-digit form
		"multiline":  "line1\nline2",
		"bracketish": "contains ]] already\nand a newline", // \n forces long-bracket; ]] forces widening
	}))

	got := buf.String()
	assert.Contains(t, got, `ctrl = "a\1b"`, "single digit escape when next byte isn't a decimal digit")
	assert.Contains(t, got, `ctrlDigit = "a\0019b"`, "three-digit escape when next byte is a decimal digit")
	assert.Contains(t, got, "multiline = [[\nline1\nline2]]", "CR/LF forces the long-bracket form")
	assert.Contains(t, got, "bracketish = [=[\ncontains ]] already\nand a newline]=]",
		"a string already containing ']]' must widen the bracket level")
}

// --- FileStore: error paths on the read and write sides --------------------

func TestFileStoreReadTree_Errors(t *testing.T) {
	t.Parallel()

	store := manif.FileStore{}

	_, err := store.ReadTree(t.TempDir())
	require.Error(t, err, "a missing manifest file must error")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest"), []byte("not lua {{{"), 0o600))
	_, err = store.ReadTree(dir)
	require.Error(t, err, "an unparsable manifest must error")

	cases := map[string]string{
		"repository not a map":       "repository = \"x\"\n",
		"repository.pkg not a map":   "repository = { foo = \"x\" }\n",
		"empty arch array":           "repository = { foo = { [\"1.0-1\"] = {} } }\n",
		"arch entry not a map":       "repository = { foo = { [\"1.0-1\"] = { \"x\" } } }\n",
		"arch entry missing arch":    "repository = { foo = { [\"1.0-1\"] = { { notarch = \"x\" } } } }\n",
		"modules not a map":          "modules = \"x\"\n",
		"modules.k not an array":     "modules = { foo = \"x\" }\n",
		"modules.k[i] not a string":  "modules = { foo = { 1 } }\n",
		"dependencies not a map":     "dependencies = \"x\"\n",
		"dependencies.pkg not a map": "dependencies = { foo = \"x\" }\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(d, "manifest"), []byte(body), 0o600))
			_, err := store.ReadTree(d)
			require.Error(t, err, "ReadTree(%q)", body)
		})
	}
}

func TestFileStoreWriteTree_NilManifest(t *testing.T) {
	t.Parallel()

	store := manif.FileStore{}
	err := store.WriteTree(t.TempDir(), nil)
	require.Error(t, err, "WriteTree(nil) must error")
}

func TestFileStoreWriteRock_NilManifest(t *testing.T) {
	t.Parallel()

	store := manif.FileStore{}
	err := store.WriteRock(filepath.Join(t.TempDir(), "rock_manifest"), nil)
	require.Error(t, err, "WriteRock(nil) must error")
}

func TestFileStoreReadRock_Errors(t *testing.T) {
	t.Parallel()

	store := manif.FileStore{}

	_, err := store.ReadRock(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err, "a missing rock_manifest file must error")

	dir := t.TempDir()

	badParse := filepath.Join(dir, "bad-parse")
	require.NoError(t, os.WriteFile(badParse, []byte("not lua {{{"), 0o600))
	_, err = store.ReadRock(badParse)
	require.Error(t, err, "an unparsable rock_manifest must error")

	missingKey := filepath.Join(dir, "missing-key")
	require.NoError(t, os.WriteFile(missingKey, []byte("other = {}\n"), 0o600))
	_, err = store.ReadRock(missingKey)
	require.Error(t, err, "a rock_manifest without a rock_manifest key must error")

	notAMap := filepath.Join(dir, "not-a-map")
	require.NoError(t, os.WriteFile(notAMap, []byte("rock_manifest = \"x\"\n"), 0o600))
	_, err = store.ReadRock(notAMap)
	require.Error(t, err, "a rock_manifest key that isn't a table must error")
}

// TestFileStoreWriteTree_MkdirFailure — WriteTree's atomic-write helper must
// surface an error when the target directory cannot be created (here,
// because a plain file already occupies that path component).
func TestFileStoreWriteTree_MkdirFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	store := manif.FileStore{}
	in := &rocks.Manifest{
		Repository:   map[string]map[string]rocks.RepoEntry{},
		Modules:      map[string][]string{},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	err := store.WriteTree(blocker, in)
	require.Error(t, err, "mkdir under a regular file must fail")
}

// TestFileStoreWriteTree_CreateFailure — the atomic-write helper must
// surface an error when the ".tmp" path it needs to create is already
// occupied by a directory.
func TestFileStoreWriteTree_CreateFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifest.tmp"), 0o750))

	store := manif.FileStore{}
	in := &rocks.Manifest{
		Repository:   map[string]map[string]rocks.RepoEntry{},
		Modules:      map[string][]string{},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	err := store.WriteTree(dir, in)
	require.Error(t, err, "create over an existing directory must fail")
}

// TestConstraintVersionFromValue_StringMapForm covers the map[string]any
// branch of constraintVersionFromValue (a version table with no numeric
// components round-trips through the pure string-keyed map representation,
// not the mixed *table form).
func TestConstraintVersionFromValue_StringMapForm(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := []byte(`dependencies = {
   app = {
      ["2.0-1"] = {
         {
            name = "lib",
            constraints = {
               {
                  op = ">=",
                  version = {
                     string = "1.0",
                     revision = 2
                  }
               }
            }
         }
      }
   }
}
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest"), src, 0o600))

	store := manif.FileStore{}
	out, err := store.ReadTree(dir)
	require.NoError(t, err, "ReadTree")

	got := out.Dependencies["app"]["2.0-1"][0].Constraints[0].Version
	assert.Equal(t, "1.0", got.Raw)
	assert.Equal(t, 2, got.Revision)
	assert.True(t, got.HasRevision)
	assert.Empty(t, got.Components, "a components-free version table has no numeric components")
}

// TestLoadDepList_Errors covers loadDepList's malformed-shape branches beyond
// the empty-map fallback already covered by TestLoadDepList_EmptyUpstreamTable.
func TestLoadDepList_Errors(t *testing.T) {
	t.Parallel()

	_, err := manif.LoadDepList("not a list")
	require.Error(t, err, "a bare string is not a dep list")

	_, err = manif.LoadDepList([]any{"not a map"})
	require.Error(t, err, "a dep entry that isn't a map must error")

	_, err = manif.LoadDepList([]any{
		map[string]any{"name": "x", "constraints": []any{"not a map"}},
	})
	require.Error(t, err, "a constraint entry that isn't a map must error")
}

// TestErrParseSentinel is a direct sanity check that every Parse error wraps
// ErrParse, matching the doc comment's contract.
func TestErrParseSentinel(t *testing.T) {
	t.Parallel()

	_, err := manif.Parse([]byte("not valid {{{"))
	require.Error(t, err)
	assert.ErrorIs(t, err, manif.ErrParse)
}
