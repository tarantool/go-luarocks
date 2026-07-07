package manif

import rocks "github.com/tarantool/go-luarocks"

// This file bridges unexported identifiers to the external `manif_test`
// package so white-box tests can exercise them while the test files
// themselves stay in `_test` packages (testpackage).

// LoadDepList exposes loadDepList for external tests.
func LoadDepList(v any) ([]rocks.Dep, error) { return loadDepList(v) }

// MixedTable is a read-only projection of the internal mixed-key table
// type, letting external tests assert on its numeric and string entries
// without reaching into unexported fields.
type MixedTable struct {
	NumKeys []int64
	Num     map[int64]any
	Str     map[string]any
}

// AsMixedTable reports whether v is the internal mixed-key table and, if
// so, returns a projection of it.
func AsMixedTable(v any) (MixedTable, bool) {
	t, ok := v.(*table)
	if !ok {
		return MixedTable{}, false
	}

	return MixedTable{NumKeys: t.numKeys, Num: t.num, Str: t.str}, true
}
