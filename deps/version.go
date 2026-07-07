// Package deps implements version parsing, constraint matching and the
// transitive dependency resolver used by the Rocks facade.
//
// Upstream references:
//
//   - luarocks/src/luarocks/core/vers.lua — Version parsing + comparator.
//   - luarocks/src/luarocks/queries.lua   — Constraint grammar.
//   - luarocks/src/luarocks/search.lua    — Remote search / pick-latest.
//
// The on-the-wire shapes (rocks.Version, rocks.VersionConstraint) live in
// the root rocks package so callers can talk about versions without
// depending on deps.
package deps

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// Numeric deltas applied to the version-component slot when a token is one
// of the recognized keywords (upstream core/vers.lua:9-17). `scm` and `dev`
// sort ABOVE numeric releases by virtue of these large positive deltas.
const (
	deltaDev   = 120000000
	deltaSCM   = 110000000
	deltaCVS   = 100000000
	deltaRC    = -1000
	deltaPre   = -10000
	deltaBeta  = -100000
	deltaAlpha = -1000000

	// asciiFallbackDivisor scales the first byte of an unknown alpha token
	// into a small delta (upstream's `token[0]/1000` ASCII fallback).
	asciiFallbackDivisor = 1000

	// componentFraction is the divisor upstream's add_token uses when a
	// numeric token merges into a slot already opened by a word
	// (`version[i] + number/100000`).
	componentFraction = 100000
)

var keywordDeltas = map[string]int{
	"dev":   deltaDev,
	"scm":   deltaSCM,
	"cvs":   deltaCVS,
	"rc":    deltaRC,
	"pre":   deltaPre,
	"beta":  deltaBeta,
	"alpha": deltaAlpha,
}

// digitsRe matches a leading numeric token; matches upstream's
// `^(%d+)[%.%-%_]*(.*)`.
var digitsRe = regexp.MustCompile(`^([0-9]+)[._-]*(.*)$`)

// alphaRe matches a leading alpha token; matches upstream's
// `^(%a+)[%.%-%_]*(.*)`.
var alphaRe = regexp.MustCompile(`^([A-Za-z]+)[._-]*(.*)$`)

// revisionRe matches the trailing `-N` revision suffix.
var revisionRe = regexp.MustCompile(`^(.*)-([0-9]+)$`)

// ParseVersion parses a rock version string (e.g. "1.2.3-4", "scm-1",
// "dev-1") into a rocks.Version.
//
// The semantics match upstream `core/vers.parse_version`:
//
//   - Strip surrounding whitespace.
//   - Strip a trailing `-N` digit revision and store as Version.Revision.
//   - Walk the remainder splitting on `.`, `-`, `_`. Numeric tokens become
//     components; recognized keywords are mapped via keywordDeltas and
//     contribute at the current slot; unknown alpha tokens fall back to
//     `token[0]/1000` per upstream's ASCII fallback.
//   - "scm" and "dev" set IsSCM / IsDev as user-facing booleans; the
//     numeric Components encode the actual ordering.
func ParseVersion(s string) (rocks.Version, error) {
	raw := s

	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return rocks.Version{}, errors.New("deps: empty version string")
	}

	v := rocks.Version{Raw: raw}

	// Trailing -N revision.
	if m := revisionRe.FindStringSubmatch(trimmed); m != nil {
		// Heuristic: keywords like "scm-1" have m[1] == "scm" — that's still
		// the version body, with revision == 1.
		rev, err := strconv.Atoi(m[2])
		if err != nil {
			return rocks.Version{}, fmt.Errorf("deps: parse revision in %q: %w", s, err)
		}

		trimmed = m[1]
		v.Revision = rev
		v.HasRevision = true
	}

	// Mirror upstream parse_version's slot discipline: `i` indexes the
	// component slot and is advanced ONLY by numeric tokens (add_token).
	// A word/keyword writes into version[i] WITHOUT advancing i, so a
	// keyword that follows a number lands in its OWN fresh slot; a number
	// that follows a word merges into that word's slot as a small fraction
	// (number/100000). See core/vers.lua add_token.
	i := 0

	cur := trimmed
	for len(cur) > 0 {
		if m := digitsRe.FindStringSubmatch(cur); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return rocks.Version{}, fmt.Errorf("deps: parse %q in %q: %w", m[1], s, err)
			}

			// add_token: version[i] = version[i] and version[i]+n/100000 or n.
			if i < len(v.Components) {
				v.Components[i] += float64(n) / componentFraction
			} else {
				v.Components = append(v.Components, float64(n))
			}

			i++
			cur = m[2]

			continue
		}

		m := alphaRe.FindStringSubmatch(cur)
		if m == nil {
			return rocks.Version{}, fmt.Errorf("deps: cannot parse remainder %q of version %q", cur, s)
		}

		// Upstream's deltas table is CASE-SENSITIVE (deltas[token] with the raw
		// %a+ match): "SCM"/"Dev" are NOT keywords and fall to the byte
		// fallback. Do not lowercase (glr-y4b).
		tok := m[1]

		var delta float64
		if d, ok := keywordDeltas[tok]; ok {
			delta = float64(d)
		} else {
			// Upstream fallback: token's first byte divided by 1000 — a
			// FLOAT division (Lua `token:byte() / 1000`).
			delta = float64(tok[0]) / asciiFallbackDivisor
		}

		switch tok {
		case "scm":
			v.IsSCM = true
		case "dev":
			v.IsDev = true
		}

		// version[i] = delta, WITHOUT advancing i — open the slot if the
		// numeric run has not reached it yet.
		for len(v.Components) <= i {
			v.Components = append(v.Components, 0)
		}

		v.Components[i] = delta

		cur = m[2]
	}

	if len(v.Components) == 0 {
		// All-keyword input ("scm") still needs a placeholder slot to
		// participate in comparisons. Upstream would have appended the delta.
		v.Components = append(v.Components, 0)
	}

	return v, nil
}

// Equal reports whether a and b are equal under upstream's __eq semantics —
// the relation the ==/~= constraint operators route through. Unlike Compare
// (which zero-pads the shorter component list for ordering), __eq requires an
// IDENTICAL component count: 5.1 and 5.1.0 are order-equivalent but NOT equal
// (vers.lua:27). Revisions are compared only when BOTH sides carry one.
func Equal(a, b rocks.Version) bool {
	if len(a.Components) != len(b.Components) {
		return false
	}

	for i := range a.Components {
		if a.Components[i] != b.Components[i] {
			return false
		}
	}

	if a.HasRevision && b.HasRevision {
		return a.Revision == b.Revision
	}

	return true
}

// Compare returns -1 if a < b, 0 if equal, 1 if a > b.
//
// Comparison is component-wise; missing trailing components on either side
// are treated as 0 (matches upstream __lt). When all components are equal the
// revision breaks the tie ONLY when both operands carry an explicit revision
// (mirrors `if v1.revision and v2.revision` in vers.lua:54); otherwise the
// versions are order-equal. Note Compare is an ordering (5.1 and 5.1.0 tie at
// 0); use Equal for the stricter ==/~= identity relation.
func Compare(a, b rocks.Version) int {
	n := max(len(b.Components), len(a.Components))
	for i := range n {
		var ai, bi float64
		if i < len(a.Components) {
			ai = a.Components[i]
		}

		if i < len(b.Components) {
			bi = b.Components[i]
		}

		if ai != bi {
			if ai < bi {
				return -1
			}

			return 1
		}
	}
	// All components equal — tie-break on revision only when BOTH sides
	// carry an explicit one (upstream `if v1.revision and v2.revision`).
	if a.HasRevision && b.HasRevision && a.Revision != b.Revision {
		if a.Revision < b.Revision {
			return -1
		}

		return 1
	}

	return 0
}
