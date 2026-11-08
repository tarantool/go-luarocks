package luaref

import (
	"fmt"

	rocks "github.com/tarantool/go-luarocks"
)

// DiffVersion describes how the Go version g differs from the upstream
// version l, or returns "" when they agree on every field upstream has: the
// components, exactly, and the revision. Raw is not compared: Go keeps the
// input verbatim where upstream drops leading whitespace, and the deps
// targets compare the string where both sides keep the same thing.
func DiffVersion(g rocks.Version, l Version) string {
	if len(g.Components) != len(l.Components) {
		return fmt.Sprintf("components %v, upstream %v", g.Components, l.Components)
	}

	for i := range g.Components {
		if g.Components[i] != l.Components[i] {
			return fmt.Sprintf("components %v, upstream %v", g.Components, l.Components)
		}
	}

	if g.HasRevision != l.HasRevision || (g.HasRevision && float64(g.Revision) != l.Revision) {
		return fmt.Sprintf("revision %d (set: %v), upstream %v (set: %v)",
			g.Revision, g.HasRevision, l.Revision, l.HasRevision)
	}

	return ""
}

// DiffConstraints describes how the Go constraints g differ from the upstream
// ones l, or returns "" when they agree: the same operators over the same
// versions, each version spelled the same.
//
// The no-upgrade marker is not compared, deliberately: deps.ParseConstraint
// documents that it accepts upstream's "@" prefix and drops it, because the
// library never upgrades a rock on its own.
func DiffConstraints(g []rocks.VersionConstraint, l []Constraint) string {
	if len(g) != len(l) {
		return fmt.Sprintf("%d constraints, upstream %d", len(g), len(l))
	}

	for i := range g {
		if g[i].Op != l[i].Op {
			return fmt.Sprintf("constraint %d: op %q, upstream %q", i, g[i].Op, l[i].Op)
		}

		if g[i].Version.Raw != l[i].Version.String {
			return fmt.Sprintf("constraint %d: version %q, upstream %q", i, g[i].Version.Raw, l[i].Version.String)
		}

		if d := DiffVersion(g[i].Version, l[i].Version); d != "" {
			return fmt.Sprintf("constraint %d: %s", i, d)
		}
	}

	return ""
}
