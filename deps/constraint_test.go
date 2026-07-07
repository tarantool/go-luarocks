package deps_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/deps"
)

func TestParseConstraint_Operators(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in     string
		wantOp string
	}{
		{">= 1.0", ">="},
		{">=1.0", ">="},
		{">1.0", ">"},
		{"<= 2.0", "<="},
		{"~> 1.2", "~>"},
		{"~= 1.0", "~="},
		{"!= 1.0", "~="},
		{"== 1.0", "=="},
		{"= 1.0", "=="},
		{"1.0", "=="},
		{" 1.0 ", "=="},
		{"@>= 1.0", ">="}, // @ is accepted and discarded
	}
	for _, tc := range cases {
		c, err := deps.ParseConstraint(tc.in)
		require.NoError(t, err, "ParseConstraint(%q)", tc.in)

		assert.Equal(t, tc.wantOp, c.Op, "ParseConstraint(%q).Op", tc.in)
	}
}

func TestParseConstraint_RejectsBadOperatorAndMissingVersion(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"!! 2.0", ">="} {
		_, err := deps.ParseConstraint(in)
		require.Error(t, err, "ParseConstraint(%q) should fail", in)
	}
}

func TestParseConstraints_Multi(t *testing.T) {
	t.Parallel()

	cs, err := deps.ParseConstraints(">= 1.2.3, < 2.0")
	require.NoError(t, err)
	require.Len(t, cs, 2)
	assert.Equal(t, ">=", cs[0].Op)
	assert.Equal(t, "<", cs[1].Op)
}

func TestParseConstraints_WhitespaceSeparated(t *testing.T) {
	t.Parallel()

	// glr-9wa: constraints may be separated by whitespace, commas, or both.
	for _, in := range []string{">= 1.0 < 2.0", ">= 1.0, < 2.0", ">=1.0 <2.0"} {
		cs, err := deps.ParseConstraints(in)
		require.NoError(t, err, "ParseConstraints(%q)", in)
		require.Len(t, cs, 2, "ParseConstraints(%q)", in)
		assert.Equal(t, ">=", cs[0].Op)
		assert.Equal(t, "<", cs[1].Op)
	}
}

func TestParseConstraints_Empty(t *testing.T) {
	t.Parallel()

	cs, err := deps.ParseConstraints("")
	require.NoError(t, err)
	assert.Empty(t, cs)
}

func TestMatch_BasicOps(t *testing.T) {
	t.Parallel()

	v, _ := deps.ParseVersion("1.2.3")

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{">= 1.0", true},
		{">= 1.2.3", true},
		{">= 1.2.4", false},
		{"> 1.0", true},
		{"> 1.2.3", false},
		{"< 2.0", true},
		{"< 1.2.3", false},
		{"== 1.2.3", true},
		{"== 1.0", false},
		{"~= 1.0", true},
		{"~= 1.2.3", false},
	} {
		cs, err := deps.ParseConstraints(tc.in)
		require.NoError(t, err, "ParseConstraints(%q)", tc.in)
		assert.Equal(t, tc.want, deps.Match(v, cs), "Match(1.2.3, %q)", tc.in)
	}
}

func TestMatch_EqualityRequiresSameComponentCount(t *testing.T) {
	t.Parallel()

	// glr-9sx: "== 5.1" must NOT match candidate 5.1.0 (upstream __eq checks
	// #v1 == #v2 first), while the ordering ops stay order-equivalent.
	v, _ := deps.ParseVersion("5.1.0")
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"== 5.1", false},
		{"~= 5.1", true},
		{"== 5.1.0", true},
		{">= 5.1", true},
		{"<= 5.1", true},
	} {
		cs, err := deps.ParseConstraints(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, deps.Match(v, cs), "Match(5.1.0, %q)", tc.in)
	}
}

func TestMatch_RevisionlessConstraintIgnoresCandidateRevision(t *testing.T) {
	t.Parallel()

	// glr-0eo: "== 1.0" (no revision) matches candidate 1.0-1 because __eq
	// ignores the revision unless BOTH sides carry one.
	v, _ := deps.ParseVersion("1.0-1")
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"== 1.0", true},
		{"~= 1.0", false},
		{"== 1.0-1", true},
		{"== 1.0-2", false},
	} {
		cs, err := deps.ParseConstraints(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, deps.Match(v, cs), "Match(1.0-1, %q)", tc.in)
	}
}

func TestMatch_TwiddleWakka(t *testing.T) {
	t.Parallel()

	cases := []struct {
		v, c string
		want bool
	}{
		{"1.2", "~> 1.2", true},
		{"1.2.1", "~> 1.2", true},
		{"1.2.99", "~> 1.2", true},
		{"1.3", "~> 1.2", false},
		{"2.0", "~> 1.2", false},
		{"1.2.3", "~> 1.2.3", true},
		{"1.2.3-1", "~> 1.2.3", true},
		{"1.2.4", "~> 1.2.3", false},
		{"1.2.3-1", "~> 1.2.3-1", true},
		{"1.2.3-2", "~> 1.2.3-1", false},
		// glr-wik: an explicit "-0" revision is enforced (0 is truthy upstream).
		{"1.2-0", "~> 1.2-0", true},
		{"1.2-5", "~> 1.2-0", false},
	}
	for _, tc := range cases {
		v, _ := deps.ParseVersion(tc.v)
		cs, _ := deps.ParseConstraints(tc.c)
		assert.Equal(t, tc.want, deps.Match(v, cs), "Match(%q, %q)", tc.v, tc.c)
	}
}

func TestMatch_AndComposition(t *testing.T) {
	t.Parallel()

	cs, _ := deps.ParseConstraints(">= 1.0, < 2.0")

	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"1.0", true},
		{"1.5", true},
		{"1.99", true},
		{"2.0", false},
		{"0.9", false},
	} {
		v, _ := deps.ParseVersion(tc.v)
		assert.Equal(t, tc.want, deps.Match(v, cs), "Match(%q, >= 1.0, < 2.0)", tc.v)
	}
}

func TestMatch_EmptyAlwaysTrue(t *testing.T) {
	t.Parallel()

	v, _ := deps.ParseVersion("0.0.0")
	assert.True(t, deps.Match(v, nil), "Match(_, nil) = false, want true")
}
