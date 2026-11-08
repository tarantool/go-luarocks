package luaref_test

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	"github.com/tarantool/go-luarocks/internal/luaref"
)

func newVM(t *testing.T) *luaref.VM {
	t.Helper()

	vm, err := luaref.New()
	require.NoError(t, err)
	t.Cleanup(vm.Close)

	return vm
}

// TestReferenceReproducesTarantoolGoldens runs upstream's persist writer in
// the reference over the manif golden inputs and expects the bytes Tarantool
// produced for them (manif/gen_goldens.sh): the check that the LuaJIT number
// and %q shims match LuaJIT where the goldens reach.
func TestReferenceReproducesTarantoolGoldens(t *testing.T) {
	t.Parallel()

	vm := newVM(t)
	dir := filepath.Join("..", "..", "manif", "testdata", "persist")

	inputs, err := filepath.Glob(filepath.Join(dir, "in", "*.lua"))
	require.NoError(t, err)
	require.NotEmpty(t, inputs)

	for _, in := range inputs {
		src, err := os.ReadFile(in)
		require.NoError(t, err)

		want, err := os.ReadFile(filepath.Join(dir, "out", strings.TrimSuffix(filepath.Base(in), ".lua")+".out"))
		require.NoError(t, err)

		chunk, err := vm.L.LoadString(string(src))
		require.NoError(t, err, in)

		res, err := vm.CallValue(chunk)
		require.NoError(t, err, in)

		got, ok, msg, err := vm.SaveFromTableToString(res[0])
		require.NoError(t, err, in)
		require.True(t, ok, msg)
		assert.Equal(t, string(want), got, in)
	}
}

func TestFormatNumberIsLuaJITToString(t *testing.T) {
	t.Parallel()

	for f, want := range map[float64]string{
		0: "0", 1: "1", -1: "-1", 0.1: "0.1", 1.0 / 3: "0.33333333333333", 123.456: "123.456",
		99999999999999: "99999999999999", 1e14: "1e+14", 1 << 53: "9.007199254741e+15",
		1e-5: "1e-05", 1.5e300: "1.5e+300", math.Inf(1): "inf", math.Inf(-1): "-inf",
	} {
		assert.Equal(t, want, luaref.FormatNumber(f), "%v", f)
	}

	assert.Equal(t, "-0", luaref.FormatNumber(math.Copysign(0, -1)))
	assert.Equal(t, "nan", luaref.FormatNumber(math.NaN()))
}

func TestQuoteIsLuaJITPercentQ(t *testing.T) {
	t.Parallel()

	for s, want := range map[string]string{
		"plain":             `"plain"`,
		`say "hi" \o`:       `"say \"hi\" \\o"`,
		"a\nb":              "\"a\\\nb\"",
		"a\rb":              `"a\13b"`,
		"a\x00b":            `"a\0b"`,
		"a\x001":            `"a\0001"`,
		"\x7f\x1f9":         `"\127\0319"`,
		"\xc3\xa9t\xc3\xa9": "\"\xc3\xa9t\xc3\xa9\"",
	} {
		assert.Equal(t, want, luaref.Quote(s), "%q", s)
	}
}

func TestReferenceStaysOffTheHost(t *testing.T) {
	t.Parallel()

	vm := newVM(t)

	run := func(src string) ([]lua.LValue, error) {
		chunk, err := vm.L.LoadString(src)
		require.NoError(t, err)

		return vm.CallValue(chunk)
	}

	res, err := run(`return io.open("/etc/passwd")`)
	require.NoError(t, err)
	assert.Equal(t, lua.LNil, res[0], "io.open reads only registered files")

	vm.AddFile("mem", "x = 1")

	res, err = run(`local f = io.open("mem"); local s = f:read("*a"); f:close(); return s`)
	require.NoError(t, err)
	assert.Equal(t, lua.LString("x = 1"), res[0])

	res, err = run(`return os.getenv("PATH")`)
	require.NoError(t, err)
	assert.Equal(t, lua.LNil, res[0])

	for _, src := range []string{`io.popen("true")`, `os.execute("true")`, `os.remove("x")`, `dofile("x")`} {
		_, err := run(src)
		require.Error(t, err, src)
	}

	_, err = run(`return require("luarocks.fs").make_dir`)
	assert.ErrorContains(t, err, "luaref stub")
}

func TestParseVersionWarnsEveryTime(t *testing.T) {
	t.Parallel()

	vm := newVM(t)

	for range 2 {
		v, err := vm.ParseVersion("1.0!")
		require.NoError(t, err)
		assert.True(t, v.Warned, "parse_version's memo must not hide the warning")
		assert.Equal(t, []float64{1, 0, 0}, v.Components)
	}

	v, err := vm.ParseVersion("1.2.3-4")
	require.NoError(t, err)
	assert.False(t, v.Warned)
	assert.Equal(t, luaref.Version{String: "1.2.3-4", Components: []float64{1, 2, 3}, Revision: 4, HasRevision: true}, v)
}

func TestTonumberReadsLongDigitRuns(t *testing.T) {
	t.Parallel()

	v, err := newVM(t).ParseVersion("99999999999999999999")
	require.NoError(t, err)
	assert.Equal(t, []float64{1e20}, v.Components)
}

func TestValueRoundTrip(t *testing.T) {
	t.Parallel()

	vm := newVM(t)

	in := map[string]any{
		"s": "x", "b": true, "i": int64(3), "f": 1.5,
		"arr":   []any{"a", "b"},
		"mixed": map[any]any{int64(1): "one", "k": "v"},
	}

	lv, err := vm.ToLua(in)
	require.NoError(t, err)

	out, err := luaref.FromLua(lv)
	require.NoError(t, err)
	assert.Equal(t, map[any]any{
		"s": "x", "b": true, "i": 3.0, "f": 1.5,
		"arr":   map[any]any{1.0: "a", 2.0: "b"},
		"mixed": map[any]any{1.0: "one", "k": "v"},
	}, out)
}

func TestWritableFiles(t *testing.T) {
	t.Parallel()

	vm := newVM(t)
	vm.AddFile("a.txt", "one\ntwo\n")
	vm.AddFile("sub/b.txt", "b")
	require.NoError(t, vm.WritableFiles("/src"))

	chunk, err := vm.L.LoadString(`
		local out = {}
		local f = io.open("a.txt")
		out[#out+1] = f:read(4)
		out[#out+1] = f:read(100)
		out[#out+1] = tostring(f:read(1))
		f:close()
		out[#out+1] = tostring(io.open("missing"))
		out[#out+1] = tostring(io.open("nodir/x", "w"))
		local d = io.open("sub")
		out[#out+1] = tostring(d:read(1))
		local w = io.open("sub/new.txt", "wb")
		w:write("x", "y")
		out[#out+1] = tostring(os.rename("/src/a.txt", "sub/../moved.txt"))
		out[#out+1] = tostring(os.remove("sub"))
		out[#out+1] = tostring(os.remove("sub/b.txt"))
		out[#out+1] = require("luarocks.fs").absolute_name("c/../d")
		io.stderr:write("warned", "\n")
		return table.concat(out, "|")
	`)
	require.NoError(t, err)

	res, err := vm.CallValue(chunk)
	require.NoError(t, err)
	assert.Equal(t, "one\n|two\n|nil|nil|nil|nil|true|nil|true|/src/d", res[0].String())
	assert.Equal(t, map[string]string{"/src/moved.txt": "one\ntwo\n", "/src/sub/new.txt": "xy"}, vm.Files())
	assert.Equal(t, "warned\n", vm.Stderr())
}
