package deps

import (
	"fmt"
	"regexp"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// operatorAliases maps the surface forms accepted by upstream
// queries.lua:84-96 to their canonical operator strings used in
// rocks.VersionConstraint.Op.
var operatorAliases = map[string]string{
	"==": "==",
	"~=": "~=",
	">":  ">",
	"<":  "<",
	">=": ">=",
	"<=": "<=",
	"~>": "~>",
	"":   "==",
	"=":  "==",
	"!=": "~=",
}

// constraintRe matches one constraint segment "<op><version>". The op group
// is greedy over `[<>=~!]*` so multi-character operators like ">=" win
// over a bare ">". Trailing whitespace + comma are stripped by the caller
// when splitting segments.
var constraintRe = regexp.MustCompile(`^\s*(@?)([<>=~!]*)\s*([0-9A-Za-z._-]+)\s*$`)

// constraintTokenRe matches ONE leading constraint and captures the remaining
// tail in group 4, so ParseConstraints can consume constraints separated by
// whitespace, commas, or both — mirroring upstream parse_constraint's [%s,]*
// trailing separator class (queries.lua:114).
var constraintTokenRe = regexp.MustCompile(`^\s*(@?)([<>=~!]*)\s*([0-9A-Za-z._-]+)[\s,]*(.*)$`)

// ParseConstraint parses one constraint segment (e.g. ">= 1.2.3", "~> 1.2",
// "1.0" with implicit "==") into a rocks.VersionConstraint.
//
// Whitespace is tolerated around the operator and version. An empty
// operator defaults to "==". An `@` prefix is accepted for upstream
// compatibility (no-upgrade marker) and silently dropped — the engine does
// not model no_upgrade since the facade does not perform automatic upgrades.
func ParseConstraint(s string) (rocks.VersionConstraint, error) {
	m := constraintRe.FindStringSubmatch(s)
	if m == nil {
		return rocks.VersionConstraint{}, fmt.Errorf("deps: cannot parse constraint %q", s)
	}

	op, ok := operatorAliases[m[2]]
	if !ok {
		return rocks.VersionConstraint{}, fmt.Errorf("deps: unknown constraint operator %q in %q", m[2], s)
	}

	v, err := ParseVersion(m[3])
	if err != nil {
		return rocks.VersionConstraint{}, fmt.Errorf("deps: constraint %q: %w", s, err)
	}

	return rocks.VersionConstraint{Op: op, Version: v}, nil
}

// ParseConstraints parses a full constraint expression (e.g.
// ">= 1.2.3, < 2.0") into the AND'd list of constraints upstream stores
// under queries[i].constraints.
//
// An empty input yields an empty (non-nil) slice — i.e. "no constraints,
// accept any version".
func ParseConstraints(s string) ([]rocks.VersionConstraint, error) {
	out := []rocks.VersionConstraint{}

	rest := strings.TrimSpace(s)
	for rest != "" {
		m := constraintTokenRe.FindStringSubmatch(rest)
		if m == nil {
			return nil, fmt.Errorf("deps: cannot parse constraint %q", rest)
		}

		op, ok := operatorAliases[m[2]]
		if !ok {
			return nil, fmt.Errorf("deps: unknown constraint operator %q in %q", m[2], s)
		}

		v, err := ParseVersion(m[3])
		if err != nil {
			return nil, fmt.Errorf("deps: constraint %q: %w", s, err)
		}

		out = append(out, rocks.VersionConstraint{Op: op, Version: v})
		rest = strings.TrimSpace(m[4])
	}

	return out, nil
}

// Match reports whether v satisfies every constraint in cs. An empty
// constraint list always matches (matches upstream — no constraints means
// any version is acceptable).
func Match(v rocks.Version, cs []rocks.VersionConstraint) bool {
	for _, c := range cs {
		if !matchOne(v, c) {
			return false
		}
	}

	return true
}

func matchOne(v rocks.Version, c rocks.VersionConstraint) bool {
	cv := c.Version

	cmp := Compare(v, cv)

	switch c.Op {
	case "==":
		return Equal(v, cv)
	case "~=":
		return !Equal(v, cv)
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case "~>":
		return partialMatch(v, cv)
	default:
		// Unknown op — fail closed. The constructor rejects unknown
		// ops, but if a hand-constructed VersionConstraint reaches us we'd
		// rather refuse the match than silently allow it.
		return false
	}
}

// partialMatch implements the `~>` pessimistic operator: every component
// of `requested` must match `version`. Trailing components of `version`
// beyond requested's length are wildcards. If requested carries an explicit
// revision, that revision must match exactly too (upstream gates on
// requested.revision being present, not merely non-zero).
//
// Examples:
//
//   - `~> 1.2` matches 1.2, 1.2.1, 1.2.99; does NOT match 1.3.
//   - `~> 1.2.3` matches 1.2.3, 1.2.3-1; does NOT match 1.2.4.
func partialMatch(version, requested rocks.Version) bool {
	for i, ri := range requested.Components {
		var vi float64
		if i < len(version.Components) {
			vi = version.Components[i]
		}

		if ri != vi {
			return false
		}
	}

	// Upstream gates on `if requested.revision then` (vers.lua:172): since 0 is
	// truthy in Lua, an explicit "-0" revision IS enforced. HasRevision
	// distinguishes an explicit "-0" from an absent revision, so "~> 1.2-0"
	// correctly demands candidate revision 0 (glr-wik).
	if requested.HasRevision {
		return version.Revision == requested.Revision
	}

	return true
}
