package rockspec

import (
	"maps"
	"runtime"

	rocks "github.com/tarantool/go-luarocks"
)

// RuntimePlatforms returns the platform-name slice to feed to MergePlatforms
// for the current runtime.GOOS, ordered least-specific to most-specific.
func RuntimePlatforms() []string {
	return runtimePlatformsFor(runtime.GOOS)
}

// runtimePlatformsFor is the GOOS-injected backing for RuntimePlatforms so
// tests can cover every OS without depending on the host.
//
// The order and membership mirror upstream cfg.each_platform, which yields
// each platform_set member that also appears in platform_order (cfg.lua):
//
//   - darwin  → unix, bsd, macosx. NOT "macos": it is a live platform flag
//     but absent from platform_order, so each_platform never reaches it.
//   - *bsd    → unix, bsd, <os-specific> (freebsd/openbsd/netbsd/dragonfly all
//     appear in platform_order).
func runtimePlatformsFor(goos string) []string {
	switch goos {
	case "linux":
		return []string{"unix", "linux"}
	case "darwin":
		return []string{"unix", "bsd", "macosx"}
	case "freebsd", "openbsd", "netbsd", "dragonfly":
		return []string{"unix", "bsd", goos}
	default:
		return []string{"unix"}
	}
}

// MergePlatforms folds every per-platform overlay for each name in plats, in
// order, mirroring upstream rockspecs.from_persisted_table, which runs
// platform_overrides on build, source, dependencies, build_dependencies,
// test_dependencies and external_dependencies (rockspecs.lua:112-119). Scalars
// overwrite; module/variable maps merge recursively; dependency lists
// index-merge; external entries overwrite by name. After all overlays are
// applied the per-table platform data is cleared (upstream: `tbl.platforms = nil`).
//
// Platform names not present in an overlay map are ignored — undeclared
// platforms in the rockspec are valid; they are inert for our target.
func MergePlatforms(spec *rocks.Rockspec, plats []string) {
	if spec == nil {
		return
	}

	for _, name := range plats {
		if overlay, ok := spec.Build.Platforms[name]; ok {
			mergeBuild(&spec.Build, &overlay)
		}

		if overlay, ok := spec.Source.Platforms[name]; ok {
			mergeSource(&spec.Source, &overlay)
		}

		if overlay, ok := spec.DependenciesPlatforms[name]; ok {
			spec.Dependencies = mergeDepList(spec.Dependencies, overlay)
		}

		if overlay, ok := spec.BuildDependenciesPlatforms[name]; ok {
			spec.BuildDependencies = mergeDepList(spec.BuildDependencies, overlay)
		}

		if overlay, ok := spec.TestDependenciesPlatforms[name]; ok {
			spec.TestDependencies = mergeDepList(spec.TestDependencies, overlay)
		}

		if overlay, ok := spec.ExternalDependenciesPlatforms[name]; ok {
			spec.ExternalDependencies = mergeExternalDeps(spec.ExternalDependencies, overlay)
		}
	}

	spec.Build.Platforms = nil
	spec.Source.Platforms = nil
	spec.DependenciesPlatforms = nil
	spec.BuildDependenciesPlatforms = nil
	spec.TestDependenciesPlatforms = nil
	spec.ExternalDependenciesPlatforms = nil
}

// mergeSource applies src's non-empty scalar fields onto dst (deep_merge scalar
// overwrite; the zero-value-means-unset convention mergeBuild also uses).
func mergeSource(dst, src *rocks.Source) {
	if src.URL != "" {
		dst.URL = src.URL
	}

	if src.Tag != "" {
		dst.Tag = src.Tag
	}

	if src.Branch != "" {
		dst.Branch = src.Branch
	}

	if src.MD5 != "" {
		dst.MD5 = src.MD5
	}

	if src.File != "" {
		dst.File = src.File
	}

	if src.Dir != "" {
		dst.Dir = src.Dir
	}

	if src.Module != "" {
		dst.Module = src.Module
	}
}

// mergeDepList index-merges an override dependency list into the base, mirroring
// deep_merge over the parsed-query array: override[i] replaces base[i], base
// entries past the override's length survive.
func mergeDepList(base, over []rocks.Dep) []rocks.Dep {
	for i, d := range over {
		if i < len(base) {
			base[i] = d
		} else {
			base = append(base, d)
		}
	}

	return base
}

// mergeExternalDeps overwrites base external-dependency entries by symbolic name
// with the override's entries.
func mergeExternalDeps(base, over map[string]rocks.ExternalDep) map[string]rocks.ExternalDep {
	if base == nil {
		base = map[string]rocks.ExternalDep{}
	}

	maps.Copy(base, over)

	return base
}

// mergeBuild applies src's fields onto dst.
//
// Mirrors util.deep_merge: for tables, merge in place; for scalars (non-zero
// in src), overwrite dst. We adopt the convention that the zero value of a
// scalar means "not set" by the overlay, so it does not clobber the base.
// This is conservative — rockspecs in practice declare a field only when
// they want to override it.
//
// Known limitation (glr-dqb): because a Go rocks.Build always materializes
// every field, the harvester cannot distinguish "field absent from the
// platform block" from "field explicitly set to empty". A deliberate
// reset-to-empty (e.g. platforms.linux.build_command = "") is therefore NOT
// honored — the base value survives. Upstream deep_merge would apply the empty
// override. This is a contrived case no realistic Tarantool rockspec exercises;
// honoring it would require presence-tracking overlays throughout the
// harvester, which is not worth the weight here.
func mergeBuild(dst, src *rocks.Build) {
	if src.Type != "" {
		dst.Type = src.Type
	}

	if src.BuildTarget != "" {
		dst.BuildTarget = src.BuildTarget
	}

	if src.InstallTarget != "" {
		dst.InstallTarget = src.InstallTarget
	}

	if src.BuildCommand != "" {
		dst.BuildCommand = src.BuildCommand
	}

	if src.InstallCommand != "" {
		dst.InstallCommand = src.InstallCommand
	}

	// Index-wise merge, not concatenation: upstream deep_merge treats the list
	// like any table, so override[i] replaces base[i] while base entries past
	// the override's length survive (glr-0j3). For base {doc,samples} and
	// override {extra}, the result is {extra,samples}, not {doc,samples,extra}.
	for i, d := range src.CopyDirectories {
		if i < len(dst.CopyDirectories) {
			dst.CopyDirectories[i] = d
		} else {
			dst.CopyDirectories = append(dst.CopyDirectories, d)
		}
	}

	dst.Modules = mergeModules(dst.Modules, src.Modules)
	dst.Variables = mergeStringMap(dst.Variables, src.Variables)
	dst.BuildVariables = mergeStringMap(dst.BuildVariables, src.BuildVariables)
	dst.InstallVariables = mergeStringMap(dst.InstallVariables, src.InstallVariables)
	dst.Install = mergeInstall(dst.Install, src.Install)
}

func mergeModules(dst, src map[string]rocks.Module) map[string]rocks.Module {
	if len(src) == 0 {
		return dst
	}

	if dst == nil {
		dst = make(map[string]rocks.Module, len(src))
	}

	// Upstream deep_merge recurses into each table-valued module entry, so a
	// per-platform override merges field-by-field with the base module rather
	// than replacing it wholesale (util.lua:121-134).
	for k, s := range src {
		if base, ok := dst[k]; ok {
			dst[k] = mergeModule(base, s)
		} else {
			dst[k] = s
		}
	}

	return dst
}

// mergeModule merges src's fields onto dst under deep_merge semantics: the
// scalar Path overwrites only when set, and the list-valued fields index-merge
// (src[i] replaces dst[i]; any base tail beyond len(src) is preserved).
func mergeModule(dst, src rocks.Module) rocks.Module {
	if src.Path != "" {
		dst.Path = src.Path
	}

	dst.Sources = mergeList(dst.Sources, src.Sources)
	dst.Incdirs = mergeList(dst.Incdirs, src.Incdirs)
	dst.Libdirs = mergeList(dst.Libdirs, src.Libdirs)
	dst.Libraries = mergeList(dst.Libraries, src.Libraries)
	dst.Defines = mergeList(dst.Defines, src.Defines)

	return dst
}

// mergeList index-merges src over dst, mirroring upstream deep_merge over an
// array-table keyed 1..#src: each src[i] replaces dst[i], and any base entries
// beyond len(src) survive.
func mergeList(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}

	out := make([]string, 0, max(len(dst), len(src)))
	out = append(out, src...)

	if len(dst) > len(src) {
		out = append(out, dst[len(src):]...)
	}

	return out
}

func mergeStringMap(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}

	if dst == nil {
		dst = make(map[string]string, len(src))
	}

	maps.Copy(dst, src)

	return dst
}

func mergeInstall(dst, src rocks.BuildInstall) rocks.BuildInstall {
	dst.Lua = mergeStringMap(dst.Lua, src.Lua)
	dst.Lib = mergeStringMap(dst.Lib, src.Lib)
	dst.Bin = mergeStringMap(dst.Bin, src.Bin)
	dst.Conf = mergeStringMap(dst.Conf, src.Conf)

	return dst
}
