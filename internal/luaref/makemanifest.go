package luaref

import (
	"errors"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// RepoDir is the absolute path MakeManifest presents the in-memory rock
// server under.
const RepoDir = "/luaref-repo"

// Repo is an in-memory rock server directory for MakeManifest.
type Repo struct {
	// Files maps the name of each file directly in the directory to its
	// content.
	Files map[string]string
	// Dirs maps the name of each subdirectory to the names of the entries it
	// holds, which are neither read nor listed further.
	Dirs map[string][]string
}

// makeManifestSrc is the remote-repository branch of
// admin/cmd/make_manifest.lua's command, without its printouts: the manifests
// written by writer.make_manifest, then index.html when that succeeded.
const makeManifestSrc = `
local writer = require("luarocks.manif.writer")
local index = require("luarocks.admin.index")
return function(repo)
   local ok, err = writer.make_manifest(repo, "one", true)
   if ok then
      index.make_index(repo)
   end
   return ok, err
end`

// MakeManifest runs upstream `luarocks-admin make_manifest` over repo as a
// rock server (not --local-tree) and returns every file it wrote, by name,
// and every line it printed to stderr. ok is false when upstream fails; msg
// is its error then, a returned one or a raised one.
//
// The directory is served by an in-memory luarocks.fs: dir lists it as the
// `ls` of the shell-tool backend the lua engine runs on does, sorted and
// without dot files; is_dir and is_file answer from repo; make_dir succeeds;
// replace_file renames a written file; absolute_name and current_dir keep
// paths as they are. io.open reads repo's files and writes files only inside
// RepoDir, into the returned map. Nothing reaches the host.
func (vm *VM) MakeManifest(repo Repo) (written map[string]string, stderr []string, ok bool, msg string, err error) {
	if err := vm.resetVersionCache(); err != nil {
		return nil, nil, false, "", err
	}

	if err := vm.installRepoFS(); err != nil {
		return nil, nil, false, "", err
	}

	fn, err := vm.chunk("make_manifest", makeManifestSrc)
	if err != nil {
		return nil, nil, false, "", err
	}

	vm.repo = &repo
	vm.written = map[string]string{}
	vm.stderr = nil

	defer func() { vm.repo, vm.written, vm.stderr = nil, nil, nil }()

	res, err := vm.CallValue(fn, lua.LString(RepoDir))
	if err != nil {
		var apiErr *lua.ApiError
		if errors.As(err, &apiErr) && apiErr.Type == lua.ApiErrorRun &&
			!strings.Contains(err.Error(), "luaref") && !strings.Contains(err.Error(), "context") {
			return vm.written, vm.stderr, false, err.Error(), nil
		}

		return nil, nil, false, "", err
	}

	if !lua.LVAsBool(res[0]) {
		return vm.written, vm.stderr, false, errMsg(res), nil
	}

	return vm.written, vm.stderr, true, "", nil
}

// repoPath returns the name of the entry of the repository p names, and the
// name of the subdirectory it is in ("" for the top level). ok is false for
// a path outside the repository.
func repoPath(p string) (dir, name string, ok bool) {
	if p == RepoDir {
		return "", "", true
	}

	rest, found := strings.CutPrefix(p, RepoDir+"/")
	if !found || rest == "" {
		return "", "", false
	}

	if sub, name, found := strings.Cut(rest, "/"); found {
		return sub, name, !strings.Contains(name, "/")
	}

	return "", rest, true
}

// installRepoFS adds the functions of the in-memory luarocks.fs to the loud
// stub, once per VM; the others keep raising.
func (vm *VM) installRepoFS() error {
	fs, err := vm.Require("luarocks.fs")
	if err != nil {
		return err
	}

	if fs.RawGetString("dir") != lua.LNil {
		return nil
	}

	L := vm.L

	set := func(name string, fn lua.LGFunction) {
		fs.RawSetString(name, L.NewFunction(fn))
	}

	set("dir", func(L *lua.LState) int {
		names := vm.listRepo(L.CheckString(1))
		i := 0

		L.Push(L.NewFunction(func(L *lua.LState) int {
			if i >= len(names) {
				L.Push(lua.LNil)

				return 1
			}

			L.Push(lua.LString(names[i]))
			i++

			return 1
		}))

		return 1
	})
	set("is_dir", func(L *lua.LState) int {
		L.Push(lua.LBool(vm.repoIsDir(L.CheckString(1))))

		return 1
	})
	set("is_file", func(L *lua.LState) int {
		L.Push(lua.LBool(vm.repoIsFile(L.CheckString(1))))

		return 1
	})
	set("exists", func(L *lua.LState) int {
		p := L.CheckString(1)
		L.Push(lua.LBool(vm.repoIsDir(p) || vm.repoIsFile(p)))

		return 1
	})
	set("make_dir", func(L *lua.LState) int {
		L.Push(lua.LTrue)

		return 1
	})
	set("replace_file", func(L *lua.LState) int {
		dst, src := L.CheckString(1), L.CheckString(secondArg)

		content, ok := vm.written[src]
		if !ok {
			L.Push(lua.LNil)
			L.Push(lua.LString(src + ": No such file or directory"))

			return 2 //nolint:mnd // nil, message.
		}

		delete(vm.written, src)
		vm.written[dst] = content

		L.Push(lua.LTrue)

		return 1
	})
	set("absolute_name", func(L *lua.LState) int {
		L.Push(lua.LString(L.CheckString(1)))

		return 1
	})
	set("current_dir", func(L *lua.LState) int {
		L.Push(lua.LString("/"))

		return 1
	})

	util, err := vm.Require("luarocks.util")
	if err != nil {
		return err
	}

	util.RawSetString("printerr", L.NewFunction(func(L *lua.LState) int {
		parts := make([]string, 0, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, L.ToStringMeta(L.Get(i)).String())
		}

		vm.stderr = append(vm.stderr, strings.Join(parts, "\t"))

		return 0
	}))

	return nil
}

// listRepo lists the entries of the repository directory at p, as `ls` does.
func (vm *VM) listRepo(p string) []string {
	if vm.repo == nil {
		return nil
	}

	sub, name, ok := repoPath(p)
	if !ok || sub != "" {
		return nil
	}

	set := map[string]bool{}

	if name == "" {
		for n := range vm.repo.Files {
			set[n] = true
		}

		for n := range vm.repo.Dirs {
			set[n] = true
		}

		for full := range vm.written {
			if s, n, ok := repoPath(full); ok && s == "" && n != "" {
				set[n] = true
			}
		}
	} else {
		for _, n := range vm.repo.Dirs[name] {
			set[n] = true
		}
	}

	names := make([]string, 0, len(set))

	for n := range set {
		names = append(names, n)
	}

	out := names[:0]

	for _, n := range names {
		if !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}

	sort.Strings(out)

	return out
}

func (vm *VM) repoIsDir(p string) bool {
	if vm.repo == nil {
		return false
	}

	sub, name, ok := repoPath(p)
	if !ok {
		return false
	}

	if name == "" {
		return true
	}

	if sub != "" {
		return false
	}

	_, isDir := vm.repo.Dirs[name]

	return isDir
}

func (vm *VM) repoIsFile(p string) bool {
	if vm.repo == nil {
		return false
	}

	if _, ok := vm.written[p]; ok {
		return true
	}

	sub, name, ok := repoPath(p)
	if !ok || sub != "" || name == "" {
		return false
	}

	_, isFile := vm.repo.Files[name]

	return isFile
}

// repoFile returns the content io.open reads at p: a file of the repository
// or one written during the call.
func (vm *VM) repoFile(p string) (string, bool) {
	if vm.repo == nil {
		return "", false
	}

	if content, ok := vm.written[p]; ok {
		return content, true
	}

	sub, name, ok := repoPath(p)
	if !ok || sub != "" || name == "" {
		return "", false
	}

	content, isFile := vm.repo.Files[name]

	return content, isFile
}
