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
