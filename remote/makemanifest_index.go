package remote

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// extURLTarget is admin/index.lua's ext_url_target.
const extURLTarget = ` target="_blank"`

var (
	// dollarWordRe is the "$(%w+)" pattern the templates are filled with.
	dollarWordRe = regexp.MustCompile(`\$[A-Za-z0-9]+`)
	// luaSpaceRunRe is "%s+" in the C locale.
	luaSpaceRunRe = regexp.MustCompile(`[ \t\n\v\f\r]+`)
	// detailedURLRe is the URL pattern index.lua links in the detailed
	// description, "(https?://[a-zA-Z0-9%.%%-_%+%[%]=%?&/$@;:]+)", whose class
	// holds '%', '-' and '_' as literal characters, as LuaJIT reads it.
	detailedURLRe = regexp.MustCompile(`https?://[a-zA-Z0-9.%\-_+\[\]=?&/$@;:]+`)
)

// indexHTML mirrors index.make_index (admin/index.lua:121-176) over the
// unfiltered index of a rock server. luaVersions are the manifests the footer
// links, util.lua_versions upstream.
func indexHTML(ctx context.Context, dir string, idx repoIndex, luaVersions []string) ([]byte, error) {
	var page strings.Builder

	page.WriteString(indexHeader)

	var ls *lua.LState

	defer func() {
		if ls != nil {
			ls.Close()
		}
	}()

	for _, pkg := range slices.Sorted(maps.Keys(idx)) {
		output, latest := indexVersions(pkg, idx[pkg])

		if latest == "" {
			var err error

			output, err = indexWithoutRockspec(output, pkg)
			if err != nil {
				return nil, fmt.Errorf("index.html: %s: %w", pkg, err)
			}

			page.WriteString(output)

			continue
		}

		if ls == nil {
			ls = newRockspecState()
		}

		vars, err := indexVars(ctx, ls, dir, pkg, latest)
		if err != nil {
			return nil, fmt.Errorf("index.html: %w", err)
		}

		output, err = gsubVars(output, vars)
		if err != nil {
			return nil, fmt.Errorf("index.html: %s: %w", pkg, err)
		}

		page.WriteString(output)
	}

	page.WriteString(indexFooterBegin)

	for _, lv := range luaVersions {
		line, err := luaGsubPlain(indexManifestVer, "$VER", lv)
		if err != nil {
			return nil, fmt.Errorf("index.html: %w", err)
		}

		page.WriteString(line)
	}

	page.WriteString(indexFooterEnd)

	return []byte(page.String()), nil
}

// indexVersions renders the versions of pkg, newest first, each with its
// arches in byte order, onto the row template, and returns the file name of
// the newest rockspec, or "" when no version has one.
func indexVersions(pkg string, versions map[string][]string) (output, latest string) {
	var b strings.Builder

	b.WriteString(indexPackageBegin)

	for _, version := range versionsNewestFirst(versions) {
		b.WriteString(version + ":&nbsp;")

		arches := slices.Clone(versions[version])
		sort.Strings(arches)

		links := make([]string, 0, len(arches))

		for _, a := range arches {
			var file string

			if a == archRockspec {
				file = pkg + "-" + version + ".rockspec"
				if latest == "" {
					latest = file
				}
			} else {
				file = pkg + "-" + version + "." + a + ".rock"
			}

			links = append(links, `<a href="`+file+`">`+a+`</a>`)
		}

		b.WriteString(strings.Join(links, ",&nbsp;") + "<br/>")
	}

	b.WriteString(indexPackageEnd)

	return b.String(), latest
}

// versionsNewestFirst orders the versions as util.sortedpairs(version_list,
// vers.compare_versions) does. Versions that compare equal keep byte order;
// upstream's table.sort leaves their order unspecified.
func versionsNewestFirst(versions map[string][]string) []string {
	keys := slices.Sorted(maps.Keys(versions))

	parsed := make(map[string]rocks.Version, len(keys))
	for _, k := range keys {
		parsed[k] = luaParseVersion(k)
	}

	slices.SortStableFunc(keys, func(a, b string) int {
		return deps.Compare(parsed[b], parsed[a])
	})

	return keys
}

// indexWithoutRockspec fills the row of a rock that has no rockspec, as the
// else branch of make_index does: its name for $anchor and $package, and
// nothing for every other $word.
func indexWithoutRockspec(output, pkg string) (string, error) {
	output, err := luaGsubPlain(output, "$anchor", pkg)
	if err != nil {
		return "", err
	}

	output, err = luaGsubPlain(output, "$package", pkg)
	if err != nil {
		return "", err
	}

	return dollarWordRe.ReplaceAllLiteralString(output, ""), nil
}

// indexVars loads the rockspec latest of pkg as persist.load_into_table
// does, with no platform overrides, and returns the values make_index fills
// the row of pkg with.
func indexVars(ctx context.Context, ls *lua.LState, dir, pkg, latest string) (map[string]lua.LValue, error) {
	path := filepath.Join(dir, latest)

	src, err := readInside(dir, path)
	if err != nil {
		return nil, fmt.Errorf("rockspec %s: %w", path, err)
	}

	spec, msg := runRockspec(ctx, ls, path, src)
	if spec == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		return nil, raisef("rockspec %s does not load (%s)", path, msg)
	}

	descript := spec.RawGetString("description")
	if !lua.LVAsBool(descript) {
		descript = ls.NewTable()
	}

	field := func(k string) (lua.LValue, error) {
		v, err := luaIndex(descript, lua.LString(k))
		if err != nil {
			return nil, fmt.Errorf("rockspec %s: description: %w", path, err)
		}

		return v, nil
	}

	or := func(v lua.LValue, dflt string) lua.LValue {
		if lua.LVAsBool(v) {
			return v
		}

		return lua.LString(dflt)
	}

	source := spec.RawGetString("source")
	if !lua.LVAsBool(source) {
		return nil, raisef("rockspec %s: attempt to index field 'source' (a %s value)", path, source.Type())
	}

	original, err := luaIndex(source, lua.LString("url"))
	if err != nil {
		return nil, fmt.Errorf("rockspec %s: source: %w", path, err)
	}

	vars := map[string]lua.LValue{
		"anchor":   lua.LString(pkg),
		"package":  spec.RawGetString("package"),
		"original": original,
	}

	for _, f := range []struct{ key, dflt string }{
		{"summary", ""}, {"detailed", ""}, {"license", "N/A"},
	} {
		v, err := field(f.key)
		if err != nil {
			return nil, err
		}

		vars[f.key] = or(v, f.dflt)
	}

	homepage, err := field("homepage")
	if err != nil {
		return nil, err
	}

	vars["homepage"] = lua.LString("")

	if lua.LVAsBool(homepage) {
		h, err := luaConcatString(homepage)
		if err != nil {
			return nil, fmt.Errorf("rockspec %s: description.homepage: %w", path, err)
		}

		vars["homepage"] = lua.LString(`| <a href="` + h + `"` + extURLTarget + `>project homepage</a>`)
	}

	ext, err := formatExternalDependencies(spec.RawGetString("external_dependencies"))
	if err != nil {
		return nil, fmt.Errorf("rockspec %s: external_dependencies: %w", path, err)
	}

	vars["externaldependencies"] = lua.LString(ext)

	detailed, ok := vars["detailed"].(lua.LString)
	if !ok {
		return nil, raisef("rockspec %s: description.detailed is a %s, not a string", path, vars["detailed"].Type())
	}

	d := strings.ReplaceAll(string(detailed), "\n\n", "</p><p>")
	d = luaSpaceRunRe.ReplaceAllLiteralString(d, " ")
	d = detailedURLRe.ReplaceAllStringFunc(d, func(url string) string {
		return `<a href="` + url + `"` + extURLTarget + `>` + url + `</a>`
	})
	vars["detailed"] = lua.LString(d)

	return vars, nil
}

// gsubVars mirrors output:gsub("$(%w+)", vars): a string or a number
// replaces its $word, nil or false keeps it, anything else raises.
func gsubVars(output string, vars map[string]lua.LValue) (string, error) {
	var ferr error

	out := dollarWordRe.ReplaceAllStringFunc(output, func(m string) string {
		v, ok := vars[m[1:]]
		if !ok || !lua.LVAsBool(v) {
			return m
		}

		switch t := v.(type) {
		case lua.LString:
			return string(t)
		case lua.LNumber:
			return luaNumberString(float64(t))
		default:
			if ferr == nil {
				ferr = raisef("invalid replacement value (a %s)", v.Type())
			}

			return m
		}
	})

	return out, ferr
}

// luaGsubPlain mirrors s:gsub(pattern, repl) for a pattern that matches only
// itself and has no captures, such as "$anchor": repl is read as Lua reads
// a replacement string (LuaJIT lib_string.c add_s) — "%" followed by a
// non-digit stands for that character (the NUL byte at the end of the
// string when nothing follows), "%0" and "%1" for the match, any other
// digit raises.
func luaGsubPlain(s, pattern, repl string) (string, error) {
	var b strings.Builder

	for i := 0; i < len(repl); i++ {
		if repl[i] != '%' {
			b.WriteByte(repl[i])

			continue
		}

		i++

		var c byte
		if i < len(repl) {
			c = repl[i]
		}

		switch {
		case c < '0' || c > '9':
			b.WriteByte(c)
		case c == '0' || c == '1':
			b.WriteString(pattern)
		default:
			return "", raisef("invalid capture index in replacement string %q", repl)
		}
	}

	return strings.ReplaceAll(s, pattern, b.String()), nil
}

// formatExternalDependencies mirrors index.format_external_dependencies
// (admin/index.lua:90-119) on the raw external_dependencies of a rockspec.
func formatExternalDependencies(v lua.LValue) (string, error) {
	if !lua.LVAsBool(v) {
		return "", nil
	}

	ext, ok := v.(*lua.LTable)
	if !ok {
		return "", raisef("bad argument #1 to 'pairs' (table expected, got %s)", v.Type())
	}

	var deplist []string

	plats := lua.LNil
	listed := map[string]bool{}

	for _, k := range luaSortedKeys(ext) {
		if k == lua.LString("platforms") {
			plats = ext.RawGet(k)

			continue
		}

		name, ok := k.(lua.LString)
		if !ok {
			return "", raisef("attempt to index local 'name' (a %s value)", k.Type())
		}

		deplist = append(deplist, asciiLower(string(name)))
		listed[string(name)] = true
	}

	if lua.LVAsBool(plats) {
		more, err := platformExternalDependencies(plats, listed)
		if err != nil {
			return "", err
		}

		deplist = append(deplist, more...)
	}

	return "<p><b>External dependencies:</b> " + strings.Join(deplist, ",&nbsp;") + "</p>", nil
}

// platformExternalDependencies lists the external dependencies of
// external_dependencies.platforms that listed does not hold, each with the
// platform it is for.
func platformExternalDependencies(plats lua.LValue, listed map[string]bool) ([]string, error) {
	pt, ok := plats.(*lua.LTable)
	if !ok {
		return nil, raisef("bad argument #1 to 'pairs' (table expected, got %s)", plats.Type())
	}

	var out []string

	for _, plat := range luaSortedKeys(pt) {
		entries, ok := pt.RawGet(plat).(*lua.LTable)
		if !ok {
			return nil, raisef("bad argument #1 to 'pairs' (table expected, got %s)", pt.RawGet(plat).Type())
		}

		for _, k := range luaSortedKeys(entries) {
			name, ok := k.(lua.LString)
			if !ok {
				return nil, raisef("attempt to index local 'name' (a %s value)", k.Type())
			}

			if listed[string(name)] {
				continue
			}

			on, err := luaConcatString(plat)
			if err != nil {
				return nil, err
			}

			out = append(out, asciiLower(string(name))+" (on "+on+")")
		}
	}

	return out, nil
}

// luaSortedKeys returns the keys of t in util.sortedpairs' default order:
// numbers first, ascending, then the others by their tostring.
func luaSortedKeys(t *lua.LTable) []lua.LValue {
	var keys []lua.LValue

	t.ForEach(func(k, _ lua.LValue) { keys = append(keys, k) })

	sort.SliceStable(keys, func(i, j int) bool {
		a, aNum := keys[i].(lua.LNumber)
		b, bNum := keys[j].(lua.LNumber)

		switch {
		case aNum && bNum:
			return a < b
		case aNum:
			return true
		case bNum:
			return false
		default:
			return keys[i].String() < keys[j].String()
		}
	})

	return keys
}
