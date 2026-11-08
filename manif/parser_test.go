package manif_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/manif"
)

// TestParseRoundTrip — Parse must be a lossless inverse of Write for every
// golden file under testdata/persist/out/*.out. Concretely:
//
//  1. Read the golden (assignments mode).
//  2. Parse it → Go tree.
//  3. Walk every top-level entry through Write and concatenate.
//  4. Demand byte equality with the golden.
//
// This catches drift between Parse and Write (e.g. a parser bug that loses
// numeric keys, or a writer regression on a value type the writer test
// doesn't otherwise exercise).
func TestParseRoundTrip(t *testing.T) {
	t.Parallel()

	goldens, err := filepath.Glob(filepath.Join("testdata", "persist", "out", "*.out"))
	require.NoError(t, err, "glob goldens")
	require.NotEmpty(t, goldens, "no goldens — run manif/gen_goldens.sh")

	for _, g := range goldens {
		name := strings.TrimSuffix(filepath.Base(g), ".out")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(g) //nolint:gosec // golden path from a controlled testdata glob
			require.NoError(t, err, "read")

			parsed, err := manif.Parse(raw)
			require.NoError(t, err, "Parse")

			var buf bytes.Buffer

			require.NoError(t, manif.Write(&buf, parsed), "Write")
			assert.Equal(t, raw, buf.Bytes(), "round-trip mismatch")
		})
	}
}

// TestParseRejectsUnsupported — the parser MUST fail loud on anything
// outside the persist subset.
func TestParseRejectsUnsupported(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{"function_literal", `x = function() end`},
		{"arithmetic_expr", `x = 1 + 2`},
		{"string_concat", `x = "a" .. "b"`},
		{"undefined_ident", `x = y`},
		{"return_statement", `return x`},
		{"unterminated_string", `x = "hello`},
		{"missing_equals", `x 5`},
		{"missing_value", `x =`},
		{"bad_escape", `x = "\q"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, err := manif.Parse([]byte(c.src))
			require.Error(t, err, "Parse(%q) succeeded; want error", c.src)
		})
	}
}

// TestParseScalars — quick sanity on individual value forms outside the
// golden corpus.
func TestParseScalars(t *testing.T) {
	t.Parallel()

	src := `
i = 42
f = 3.14
neg = -7
truthy = true
falsy = false
s = "hi"
long = [[
multi
line]]
mix = {
   1, 2, 3, name = "x"
}
`
	v, err := manif.Parse([]byte(src))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok, "top is %T, want map", v)
	assert.Equal(t, int64(42), m["i"], "i")
	assert.InEpsilon(t, 3.14, m["f"], 1e-9, "f")
	assert.Equal(t, int64(-7), m["neg"], "neg")
	assert.Equal(t, true, m["truthy"], "truthy")
	assert.Equal(t, false, m["falsy"], "falsy")
	assert.Equal(t, "hi", m["s"], "s")
	assert.Equal(t, "multi\nline", m["long"], "long")
	tbl, ok := manif.AsMixedTable(m["mix"])
	require.True(t, ok, "mix is %T, want mixed table", m["mix"])
	assert.True(t, len(tbl.NumKeys) == 3 && tbl.Num[1] == int64(1), "mix.num: %v / %v", tbl.NumKeys, tbl.Num)
	assert.Equal(t, "x", tbl.Str["name"], "mix.name")
}

// TestParseLongBracketNormalizesEOL — the Lua lexer collapses every interior
// EOL sequence inside a long-bracket string to a single \n (glr-aoc).
func TestParseLongBracketNormalizesEOL(t *testing.T) {
	t.Parallel()

	// Leading newline (\r\n form) is dropped; interior \r\n / lone \r → \n.
	src := "note = [[\r\nline1\r\nline2\rline3]]\n"

	v, err := manif.Parse([]byte(src))
	require.NoError(t, err, "Parse")

	m, ok := v.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "line1\nline2\nline3", m["note"], "interior CR/CRLF must normalize to LF")
}

// TestParseRejectsNumberRunningIntoName checks that a number followed at once
// by a name byte or a dot is one malformed number, as the Lua lexer reads it.
func TestParseRejectsNumberRunningIntoName(t *testing.T) {
	t.Parallel()

	for _, src := range []string{"A=0X=0\n", "x = 1a = 2\n", "x = { 1.5.3 }\n", "x = 0x10\n", "x = 1_\n"} {
		_, err := manif.Parse([]byte(src))
		require.ErrorIs(t, err, manif.ErrParse, "Parse(%q)", src)
	}

	v, err := manif.Parse([]byte("x = { 1,2;-3.5e1 }\n"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"x": []any{int64(1), int64(2), -35.0}}, v)
}

// TestParseRejectsKeywordKeys checks that a Lua keyword is no key, at the
// top level or in a table, as in Lua.
func TestParseRejectsKeywordKeys(t *testing.T) {
	t.Parallel()

	for _, src := range []string{"end = 1\n", "x = { end = 1 }\n", "x = { nil = 1 }\n", "x = { true = 1 }\n"} {
		_, err := manif.Parse([]byte(src))
		require.ErrorIs(t, err, manif.ErrParse, "Parse(%q)", src)
	}

	v, err := manif.Parse([]byte("x = { [\"end\"] = 1, ends = 2, true }\n"))
	require.NoError(t, err)

	m, ok := v.(map[string]any)
	require.True(t, ok)

	tbl, ok := manif.AsMixedTable(m["x"])
	require.True(t, ok)
	assert.Equal(t, map[string]any{"end": int64(1), "ends": int64(2)}, tbl.Str)
	assert.Equal(t, map[int64]any{1: true}, tbl.Num)
}

// TestParseCRIsLineBreak checks that a raw CR breaks a line as LF does: it
// ends a comment and cannot sit inside a short string.
func TestParseCRIsLineBreak(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte("-- comment\rx = 1\n"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"x": int64(1)}, v)

	for _, src := range []string{"x = \"a\rb\"\n", "x = 'a\r'\n"} {
		_, err := manif.Parse([]byte(src))
		require.ErrorIs(t, err, manif.ErrParse, "Parse(%q)", src)
	}
}

// TestParseKeysWithoutGoShape covers the table keys Lua allows but the Go
// shapes cannot hold: Parse must report them, never panic.
func TestParseKeysWithoutGoShape(t *testing.T) {
	t.Parallel()

	for _, src := range []string{
		"x = { [true] = 1 }\n",
		"x = { [false] = 1, a = 2 }\n",
		"x = { [{}] = 1 }\n",
		"x = { [{ 1 }] = 1, [1] = 2 }\n",
		"x = { [1.5] = 1, [\"1.5\"] = 2 }\n",
		"x = { [0.5] = 1, [1] = 2, [\"1\"] = 3 }\n",
		"x = " + strings.Repeat("{", 101) + strings.Repeat("}", 101) + "\n",
	} {
		_, err := manif.Parse([]byte(src))
		require.ErrorIs(t, err, manif.ErrParse, "Parse(%q)", src)
	}
}

// TestParseShapesByKeyValue checks that a table's shape follows its keys as
// Lua sees them, not their order or spelling: integer keys 1..N in any order
// make an array, an integral number key is that integer, and a repeated key
// keeps the last value.
func TestParseShapesByKeyValue(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		src  string
		want any
	}{
		{"x = { [2] = \"b\", [1] = \"a\" }\n", []any{"a", "b"}},
		{"x = { [1.0] = \"a\" }\n", []any{"a"}},
		{"x = { [2] = \"b\", \"a\" }\n", []any{"a", "b"}},
		{"x = { [1] = \"a\", [1] = \"b\" }\n", []any{"b"}},
		{"x = { [-0.0] = \"z\", [3.0] = \"c\" }\n", map[string]any{"0": "z", "3": "c"}},
	} {
		v, err := manif.Parse([]byte(c.src))
		require.NoError(t, err, "Parse(%q)", c.src)

		m, ok := v.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, c.want, m["x"], "Parse(%q)", c.src)
	}

	v, err := manif.Parse([]byte("x = { [2.0] = 2, [1] = 1, k = \"v\" }\n"))
	require.NoError(t, err)

	m, ok := v.(map[string]any)
	require.True(t, ok)

	tbl, ok := manif.AsMixedTable(m["x"])
	require.True(t, ok, "x is %T, want mixed table", m["x"])
	assert.Equal(t, []int64{1, 2}, tbl.NumKeys)
	assert.Equal(t, map[string]any{"k": "v"}, tbl.Str)
}

// TestParseReportsPositionalOverlap checks the report the differential test
// relies on: a positional entry and an explicit key on the same index, in
// either order, with the value given last kept.
func TestParseReportsPositionalOverlap(t *testing.T) {
	t.Parallel()

	for src, want := range map[string]bool{
		"x = { 0, [1] = 1 }\n":           true,
		"x = { [1.0] = 1, 0 }\n":         true,
		"x = { [1] = 0, [1] = 1 }\n":     false,
		"x = { [2] = 1, 0 }\n":           false,
		"x = { y = { \"a\", [1] = 1 } }": true,
	} {
		_, overlap, err := manif.ParseReportingOverlap([]byte(src))
		require.NoError(t, err, "Parse(%q)", src)
		assert.Equal(t, want, overlap, "Parse(%q)", src)
	}

	v, err := manif.Parse([]byte("x = { 0, [1] = 1 }\n"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"x": []any{int64(1)}}, v)
}

// TestParseMixedTableWithFloatKey checks that a table mixing integer, string
// and non-integral number keys, which the mixed table cannot hold, falls back
// to stringified keys like any other non-array table.
func TestParseMixedTableWithFloatKey(t *testing.T) {
	t.Parallel()

	v, err := manif.Parse([]byte("x = { [1] = 1, a = 2, [1.5] = 3 }\n"))
	require.NoError(t, err)

	m, ok := v.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"1": int64(1), "a": int64(2), "1.5": int64(3)}, m["x"])

	v, err = manif.Parse([]byte("x = " + strings.Repeat("{", 100) + strings.Repeat("}", 100) + "\n"))
	require.NoError(t, err, "100 nested tables are within the bound")
	assert.NotNil(t, v)
}
