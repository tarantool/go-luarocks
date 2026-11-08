package rockspec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

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

// rockspecFormats are the rockspec_format values upstream declares a schema
// for (type/rockspec.lua declare_schemas); any other is unknown.
var rockspecFormats = map[string]bool{"1.0": true, "1.1": true, "3.0": true}

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

// errPackageNotPathComponent is wrapped by the error Validate returns for a
// package name that would not stay a single directory under the rocks dir.
var errPackageNotPathComponent = errors.New("package name is not a single path component")

// checkPackageName rejects a package name that would escape or alias its
// directory once joined into <rocks-dir>/<package>/<version>. Upstream
// asserts that the name has no "/" (path.lua:68, path.install_dir, which
// loading a rockspec reaches through configure_paths). Go rejects "\\", a
// separator on Windows, and the names "." and "..", which upstream misses,
// too: "." installs into the rocks dir itself and ".." above it.
func checkPackageName(name string) error {
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("rockspec: invalid package %q: %w", name, errPackageNotPathComponent)
	}

	return nil
}

// Validate performs presence and feature-allowlist checks on a harvested
// Rockspec. It also rejects, with an error wrapping rocks.ErrUnsafePath, a
// path-valued field that would leave the directory it is joined under —
// absolute, holding a ".." element, empty, or "." where a file is meant — in
// source.file and source.dir, build.install keys and values, build.modules
// paths and sources, copy_directories and their platform overrides, which
// upstream accepts. It deliberately does NOT re-do every type assertion the upstream
// `type_check.lua` schema enforces — the harvest in Eval already routes each
// field through a typed path. Fail loud on unknown build types.
func Validate(spec *rocks.Rockspec) error {
	if spec == nil {
		return errors.New("rockspec: nil spec")
	}

	if spec.Package == "" {
		return errors.New("rockspec: missing required field 'package'")
	}

	if err := checkPackageName(spec.Package); err != nil {
		return err
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

	if err := checkSourcePaths("source", spec.Source); err != nil {
		return err
	}

	if err := checkBuildPaths("build", spec.Build); err != nil {
		return err
	}

	// Reject a future/unknown rockspec format before feature gating, mirroring
	// rockspecs.from_persisted_table (rockspecs.lua:87), which checks format
	// support before type-checking.
	if spec.RockspecFormat != "" && formatExceeds(spec.RockspecFormat, SupportedRockspecFormat) {
		return fmt.Errorf("rockspec: Rockspec format %s is not supported, please upgrade LuaRocks", spec.RockspecFormat)
	}

	// A format that is not newer must still name one of the schemas, by its
	// exact spelling: upstream looks the string up in the schema table
	// (type/rockspec.lua check_rockspec_using_version), so "2.0", "3" or
	// "0" are unknown.
	if spec.RockspecFormat != "" && !rockspecFormats[spec.RockspecFormat] {
		return fmt.Errorf("rockspec: unknown rockspec format %s", spec.RockspecFormat)
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
