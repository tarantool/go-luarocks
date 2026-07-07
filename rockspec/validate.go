package rockspec

import (
	"errors"
	"fmt"
	"regexp"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// allowedBuildTypes mirrors the upstream backend table for the subset we
// implement. Empty string is allowed only for rockspec_format >= 3.0 (it then
// defaults to builtin); the pre-3.0 gating is enforced in Validate.
var allowedBuildTypes = map[string]bool{
	"":        true, // implies builtin (format >= 3.0 only; see Validate)
	"builtin": true,
	"cmake":   true,
	"make":    true,
	"command": true,
	"none":    true,
}

// SupportedRockspecFormat is the newest rockspec_format this port understands,
// tracking upstream type_rockspec.rockspec_format. A rockspec declaring a
// newer format is rejected before feature gating.
const SupportedRockspecFormat = "3.0"

// defaultRockspecFormat is the rockspec_format assumed when a rockspec omits
// the field, matching upstream (rockspec.format_is_at_least treats an absent
// format as "1.0").
const defaultRockspecFormat = "1.0"

// versionRe anchors upstream's version schema pattern `^[%w.]+-[%d]+$`
// (type/rockspec.lua:24, anchored by type_check_item). Lua %w is [A-Za-z0-9]
// (no underscore) and %d is [0-9]: a version body followed by a mandatory
// pure-digit rockspec revision separated by a single '-'.
var versionRe = regexp.MustCompile(`^[0-9A-Za-z.]+-[0-9]+$`)

// formatAtLeast reports whether the rockspec_format (defaulting to "1.0" when
// absent, per upstream) is >= the given minimum. Mirrors
// rockspec:format_is_at_least.
func formatAtLeast(format, minimum string) bool {
	if format == "" {
		format = defaultRockspecFormat
	}

	fv, err := deps.ParseVersion(format)
	if err != nil {
		return false
	}

	mv, err := deps.ParseVersion(minimum)
	if err != nil {
		return false
	}

	return deps.Compare(fv, mv) >= 0
}

// formatExceeds reports whether the rockspec_format (default "1.0") is strictly
// newer than ceiling. Used to reject future/unknown formats.
func formatExceeds(format, ceiling string) bool {
	if format == "" {
		format = defaultRockspecFormat
	}

	fv, err := deps.ParseVersion(format)
	if err != nil {
		return false
	}

	cv, err := deps.ParseVersion(ceiling)
	if err != nil {
		return false
	}

	return deps.Compare(fv, cv) > 0
}

// unsupportedFieldErr mirrors upstream type_check.check_version's message for a
// field whose required schema version is newer than the declared format.
func unsupportedFieldErr(field, format, required string) error {
	if format == "" {
		format = defaultRockspecFormat
	}

	return fmt.Errorf("rockspec: %s is not supported in rockspec format %s (requires version %s), please fix the rockspec_format field accordingly",
		field, format, required)
}

// Validate performs presence and feature-allowlist checks on a harvested
// Rockspec. It deliberately does NOT re-do every type assertion the upstream
// `type_check.lua` schema enforces — the harvest in Eval already routes each
// field through a typed path. Fail loud on unknown build types.
func Validate(spec *rocks.Rockspec) error {
	if spec == nil {
		return errors.New("rockspec: nil spec")
	}

	if spec.Package == "" {
		return errors.New("rockspec: missing required field 'package'")
	}

	if spec.Version == "" {
		return errors.New("rockspec: missing required field 'version'")
	}

	if !versionRe.MatchString(spec.Version) {
		return fmt.Errorf("rockspec: invalid value %q for field version does not match '[%%w.]+-[%%d]+'",
			spec.Version)
	}

	if spec.Source.URL == "" {
		return errors.New("rockspec: missing required field 'source.url'")
	}

	// Reject a future/unknown rockspec format before feature gating, mirroring
	// rockspecs.from_persisted_table (rockspecs.lua:87), which checks format
	// support before type-checking.
	if spec.RockspecFormat != "" && formatExceeds(spec.RockspecFormat, SupportedRockspecFormat) {
		return fmt.Errorf("rockspec: Rockspec format %s is not supported, please upgrade LuaRocks", spec.RockspecFormat)
	}

	// Fields stamped with a schema _version must not appear under an older
	// declared format (type_check.check_version). The 3.0-only fields are
	// build_dependencies/test_dependencies/labels/issues_url; deploy is 1.1.
	if !formatAtLeast(spec.RockspecFormat, "3.0") {
		switch {
		case len(spec.BuildDependencies) > 0:
			return unsupportedFieldErr("build_dependencies", spec.RockspecFormat, "3.0")
		case len(spec.TestDependencies) > 0:
			return unsupportedFieldErr("test_dependencies", spec.RockspecFormat, "3.0")
		case len(spec.Description.Labels) > 0:
			return unsupportedFieldErr("labels", spec.RockspecFormat, "3.0")
		case spec.Description.IssuesURL != "":
			return unsupportedFieldErr("issues_url", spec.RockspecFormat, "3.0")
		}
	}

	if spec.Deploy.WrapBinScripts != nil && !formatAtLeast(spec.RockspecFormat, "1.1") {
		return unsupportedFieldErr("deploy", spec.RockspecFormat, "1.1")
	}

	if !allowedBuildTypes[spec.Build.Type] {
		return fmt.Errorf("%w: build.type=%q (supported: builtin, cmake, make, command, none)",
			rocks.ErrUnsupportedRockspecFeature, spec.Build.Type)
	}

	// Upstream only defaults a missing build table / build.type to builtin for
	// rockspec_format >= 3.0; older formats hard-reject (build.lua:354-370).
	if spec.Build.Type == "" && !formatAtLeast(spec.RockspecFormat, "3.0") {
		if !spec.HasBuild {
			return errors.New("rockspec: build table not specified")
		}

		return errors.New("rockspec: build type not specified")
	}

	return nil
}
