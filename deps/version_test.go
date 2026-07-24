package deps_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/deps"
)

func TestParseVersion_Numeric(t *testing.T) {
	t.Parallel()

	v, err := deps.ParseVersion("1.2.3-4")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3-4", v.Raw)
	assert.Equal(t, 4, v.Revision)
	require.Equal(t, []float64{1, 2, 3}, v.Components)
}

func TestParseVersion_NoRevision(t *testing.T) {
	t.Parallel()

	v, err := deps.ParseVersion("1.2.3")
	require.NoError(t, err)
	assert.Equal(t, 0, v.Revision, "Revision want 0 (default)")
}

func TestParseVersion_SCM(t *testing.T) {
	t.Parallel()

	v, err := deps.ParseVersion("scm-1")
	require.NoError(t, err)
	assert.True(t, v.IsSCM, "IsSCM = false, want true")
	assert.Equal(t, 1, v.Revision)
}

func TestParseVersion_Dev(t *testing.T) {
	t.Parallel()

	v, err := deps.ParseVersion("dev-2")
	require.NoError(t, err)
	assert.True(t, v.IsDev, "IsDev = false, want true")
}

func TestCompare_NumericOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.2", "1.2.0", 0},  // missing component treated as 0
		{"1.0", "1.0.1", -1}, // 1.0 < 1.0.1
		{"2.0", "1.99", 1},
	}
	for _, tc := range cases {
		va, _ := deps.ParseVersion(tc.a)
		vb, _ := deps.ParseVersion(tc.b)
		assert.Equal(t, tc.want, deps.Compare(va, vb), "Compare(%q, %q)", tc.a, tc.b)
	}
}

func TestCompare_RevisionTieBreak(t *testing.T) {
	t.Parallel()

	a, _ := deps.ParseVersion("1.0.0-1")
	b, _ := deps.ParseVersion("1.0.0-2")
	assert.Equal(t, -1, deps.Compare(a, b), "Compare(1.0.0-1, 1.0.0-2)")
	assert.Equal(t, 1, deps.Compare(b, a), "Compare(1.0.0-2, 1.0.0-1)")
}

func TestCompare_SCMAboveRelease(t *testing.T) {
	t.Parallel()

	scm, _ := deps.ParseVersion("scm-1")
	rel, _ := deps.ParseVersion("99.99.99")
	assert.Equal(t, 1, deps.Compare(scm, rel), "Compare(scm-1, 99.99.99) want 1 (scm above all releases)")
}

func TestCompare_DevAboveRelease(t *testing.T) {
	t.Parallel()

	dev, _ := deps.ParseVersion("dev-1")
	rel, _ := deps.ParseVersion("99.99.99")
	assert.Equal(t, 1, deps.Compare(dev, rel), "Compare(dev-1, 99.99.99) want 1 (dev above all releases)")
}

func TestCompare_DevAboveSCM(t *testing.T) {
	t.Parallel()

	dev, _ := deps.ParseVersion("dev-1")
	scm, _ := deps.ParseVersion("scm-1")
	assert.Equal(t, 1, deps.Compare(dev, scm), "Compare(dev, scm) want 1 (dev delta > scm delta)")
}

func TestParseVersion_KeywordOpensNewSlot(t *testing.T) {
	t.Parallel()

	// "1.2beta": the keyword must land in its OWN slot, not merge into the
	// preceding numeric component. Upstream => {1, 2, -100000}.
	v, err := deps.ParseVersion("1.2beta")
	require.NoError(t, err)
	require.Equal(t, []float64{1, 2, -100000}, v.Components)
}

func TestCompare_PrereleaseOfHigherMinorBeatsLowerMinor(t *testing.T) {
	t.Parallel()

	// Regression (glr-hum): a beta of 1.2 is newer than 1.1. The old code
	// collapsed "1.2beta" to {1, -99998} and wrongly ranked it below 1.1.
	beta, _ := deps.ParseVersion("1.2beta")
	rel, _ := deps.ParseVersion("1.1")
	assert.Equal(t, 1, deps.Compare(beta, rel), "Compare(1.2beta, 1.1) want 1")
	assert.Equal(t, -1, deps.Compare(rel, beta), "Compare(1.1, 1.2beta) want -1")
}

func TestParseVersion_KeywordLookupIsCaseSensitive(t *testing.T) {
	t.Parallel()

	// glr-y4b: upstream's deltas table is case-sensitive, so "SCM" is NOT the
	// scm keyword — it falls to the byte fallback ('S' = 83 → 0.083) and does
	// not set IsSCM, ranking BELOW a real release.
	upper, err := deps.ParseVersion("SCM")
	require.NoError(t, err)
	assert.False(t, upper.IsSCM, "uppercase SCM must not be treated as scm")
	require.Equal(t, []float64{0.083}, upper.Components)

	rel, _ := deps.ParseVersion("1.0")
	assert.Equal(t, -1, deps.Compare(upper, rel), `"SCM" must sort below 1.0`)

	// The lowercase keyword still works and sorts above releases.
	lower, _ := deps.ParseVersion("scm-1")
	assert.True(t, lower.IsSCM, "lowercase scm keyword recognized")
	assert.Equal(t, 1, deps.Compare(lower, rel), `"scm" sorts above 1.0`)
}

func TestCompare_UnknownWordByteFallback(t *testing.T) {
	t.Parallel()

	// Regression (glr-kbg): unknown alpha tokens fall back to byte/1000 as
	// FLOAT division, so "work" (0.119) sorts above "patch" (0.112) rather
	// than both collapsing to 0.
	work, _ := deps.ParseVersion("work")
	patch, _ := deps.ParseVersion("patch")

	require.Equal(t, []float64{0.119}, work.Components)
	require.Equal(t, []float64{0.112}, patch.Components)
	assert.Equal(t, 1, deps.Compare(work, patch), "Compare(work, patch) want 1")
}

func TestCompare_RCBelowRelease(t *testing.T) {
	t.Parallel()

	rc, _ := deps.ParseVersion("1.0rc1")
	rel, _ := deps.ParseVersion("1.0")
	assert.Equal(t, -1, deps.Compare(rc, rel), "Compare(1.0rc1, 1.0) want -1 (rc below release)")
}
