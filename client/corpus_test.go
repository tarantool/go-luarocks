//go:build integration_luaengine && integration_corpus

package client_test

// Compatibility corpus: real rocks from rocks.tarantool.org and luarocks.org,
// each installed at a pinned version through both backends into fresh trees
// and compared with the backend-parity helpers (compareDeployed,
// compareRockManifests, and compareResolvedDeps, since the resolved
// dependencies of the tree manifest differ by design: divResolvedDeps), then
// smoke-tested by
// requiring every module the tree provides and running every command it
// deploys with the real tarantool.
//
// It is a scheduled signal, not a pull-request gate: it needs the network,
// takes long, and runs third-party build scripts, which is why it runs in the
// sandbox of scripts/test-in-docker.sh (.github/workflows/corpus.yml). Run it
// with
//
//	scripts/test-in-docker.sh --net go test -count=1 -v -timeout 2h \
//	  -tags integration,integration_luaengine,integration_corpus -run TestCorpus ./client/
//
// A rock that fails to install through both backends is corpus rot — the
// server, the source host or the rock's build broke, not parity — and is
// skipped with both errors recorded. A rock that installs through one backend
// only, or whose trees or smoke-test outcomes differ, fails. In the sandbox
// a rock also fails when an install, the smoke test or the cleanup leaves
// anything in the scratch space outside the trees and temp dirs
// (corpusWatch). Either way the per-rock report printed at the end says what
// happened.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/tree"
)

const (
	// corpusTarantoolServer is the server of the Tarantool ecosystem rocks.
	corpusTarantoolServer = "http://rocks.tarantool.org/"
	// corpusLuarocksServer is the public LuaRocks server.
	corpusLuarocksServer = "https://luarocks.org/"

	// corpusInstallTimeout bounds one Install: a clone plus a C or CMake
	// build of a real rock and its dependencies.
	corpusInstallTimeout = 20 * time.Minute
	// corpusSmokeTimeout bounds one tarantool run of the smoke test.
	corpusSmokeTimeout = 30 * time.Second
	// corpusProbeTimeout bounds one reachability probe.
	corpusProbeTimeout = 15 * time.Second
	// corpusMessageLimit is the length a recorded message is cut to in the
	// report; the full message is in the test log.
	corpusMessageLimit = 400
	// corpusDiffLines is how many changed lines of a testify diff the
	// report keeps.
	corpusDiffLines = 12

	// corpusReportBegin and corpusReportEnd delimit the report on stdout,
	// for the workflow to cut it out of the log.
	corpusReportBegin = "<!-- corpus-report:begin -->"
	corpusReportEnd   = "<!-- corpus-report:end -->"
)

// corpusRock is one rock of the corpus.
type corpusRock struct {
	name    string
	version string // pinned, installed as `luarocks install <name> <version>`
	// servers are the servers of the install, in order. The Tarantool rocks
	// come from rocks.tarantool.org alone, so their dependencies resolve
	// there; the luarocks.org ones get tt's default list, rocks.tarantool.org
	// then luarocks.org, so a dependency resolves across two servers.
	servers []string
	build   string // what the install does: the artifact and build type
	why     string // the shape it covers
}

// corpusRocks is the corpus. The top-level version is pinned; dependencies
// resolve to the newest matching version on the servers, as for a user, so
// they drift as the servers publish — on rocks.tarantool.org that is mostly
// scm-1, built from the default branch of the rock's repository.
//
// To add a rock: read its rockspec (and which arches the server's manifest
// lists for the version) to fill build, say in why what shape it covers that
// the others do not, and pin a released version.
func corpusRocks() []corpusRock {
	tnt := []string{corpusTarantoolServer}
	both := []string{corpusTarantoolServer, corpusLuarocksServer}

	return []corpusRock{
		{"checks", "3.4.1-1", tnt, "rockspec, cmake (Lua only), git source",
			"the most common dependency; pure Lua installed by CMake install rules into TARANTOOL_INSTALL_LUADIR"},
		{"checks", "3.1.0-1", tnt, "binary .all.rock",
			"a packed rock: unpacked and deployed with no build"},
		{"errors", "2.2.1-1", tnt, "binary .all.rock (cmake rockspec)",
			"a .all.rock built from a cmake rockspec; a dependency of crud"},
		{"luatest", "1.4.5-1", tnt, "binary .all.rock with bin/luatest",
			"a command wrapper deployed from a packed rock; depends on checks"},
		{"metrics", "1.8.3-1", tnt, "rockspec, cmake (Lua only), git source",
			"many Lua modules in nested directories; depends on checks"},
		{"http", "1.9.1-1", tnt, "source .src.rock, builtin with a C module",
			"builtin C module against $(TARANTOOL_INCDIR) from external_dependencies; source in the rock"},
		{"avro-schema", "3.1.0-1", tnt, "rockspec, cmake with C, git source",
			"CMake building a .so via TARANTOOL_DIR and installing to TARANTOOL_INSTALL_LIBDIR"},
		{"ldecnumber", "1.1.3-1", tnt, "rockspec, cmake with C, git source",
			"CMake compiling a bundled C library (decNumber) into the module, CMAKE_BUILD_TYPE set"},
		{"tuple-keydef", "0.0.4-1", tnt, "source .src.rock, cmake with C",
			"a C module against tarantool/module.h"},
		{"vshard", "1.0.0-1", tnt, "binary .all.rock",
			"a large Lua rock from a packed rock; a dependency of crud"},
		{"crud", "1.8.0-1", tnt, "rockspec, cmake (Lua only), git source",
			"a dependency chain: checks, errors and vshard (a .all.rock) installed first"},
		{"ddl", "1.7.1-1", tnt, "binary .all.rock",
			"depends on the virtual tarantool rock, satisfied by Tarantool.Version"},
		{"expirationd", "1.7.0-1", tnt, "binary .all.rock (builtin rockspec)",
			"modules under several top-level namespaces (cartridge.roles, roles); depends on checks"},
		{"tracing", "0.1.1-1", tnt, "rockspec, make, git source",
			"make with build_target, install_pass = false, build.install.lua and copy_directories"},
		{"luacheck", "0.26.0-1", tnt, "rockspec, builtin (Lua only), git source",
			"~60 modules and a Lua command wrapper; depends on argparse"},
		{"inspect", "3.1.3-0", both, "rockspec, builtin (Lua only), http tarball",
			"luarocks.org: a tarball with an explicit source.dir"},
		{"penlight", "1.14.0-3", both, "rockspec, builtin (Lua only), git source",
			"luarocks.org: a rockspec computing its fields in Lua; depends on luafilesystem (C), resolved across two servers"},
		{"luasocket", "3.1.0-1", both, "rockspec, builtin with C, git source",
			"luarocks.org: build.platforms overrides made by a rockspec function, several C modules, copy_directories"},
		{"lpeg", "1.1.0-2", both, "rockspec, builtin with C, http tarball with md5",
			"luarocks.org: a C module from a checksummed tarball"},
		{"luaposix", "36.3-1", both, "rockspec, command, zip from GitHub",
			"luarocks.org: build and install commands running $(LUA) (tarantool) on the rock's own build script; a dependency (bit32) added by rockspec code"},
	}
}

// tmpLeak returns a glob of the files the rock's own build is known to leave
// in /tmp, or "" for none. The corpus notes and removes them, so that a
// third-party build script does not fail every run through the sandbox's
// leftover check; anything else a rock leaves there fails it.
func (r corpusRock) tmpLeak() string {
	if r.name == "luaposix" {
		// build-aux/luke names its configure probes with os.tmpname, which
		// LuaJIT creates under /tmp whatever TMPDIR says, and never removes.
		return "/tmp/lua_*"
	}

	return ""
}

// corpusResult is what the corpus found for one rock.
type corpusResult struct {
	rock    corpusRock
	status  string   // pass, divergence, rot or failure
	details []string // the divergences, install errors and notes
	deps    []string // the "name version" pairs the lua tree holds
	smoke   string   // smoke-test summary
	elapsed time.Duration
}

// TestCorpus_Parity installs every rock of corpusRocks through both backends
// and compares the results; see the file comment.
//
//nolint:paralleltest // testenv.Hermetic gives each rock its own home via t.Setenv.
func TestCorpus_Parity(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	ttBin := filepath.Join(prefix, "bin", "tarantool")

	for _, url := range []string{corpusTarantoolServer, corpusLuarocksServer, "https://github.com/"} {
		if err := corpusProbe(t.Context(), url); err != nil {
			testenv.NoNetworkf(t, "%s not reachable; the corpus installs real rocks: %v", url, err)
		}
	}

	results := make([]*corpusResult, 0, len(corpusRocks()))
	start := time.Now()

	for _, rock := range corpusRocks() {
		res := &corpusResult{rock: rock}
		watch := newCorpusWatch(t, rock)

		ran := t.Run(rock.name+"@"+rock.version, func(t *testing.T) {
			corpusRun(t, res, watch, prefix, incDir, ttBin)
		})

		// What is new now that the rock's temp dirs are gone was written
		// outside them after the last step corpusRun checked.
		if stray := watch.step(t, res, "the test"); len(stray) > 0 {
			msg := fmt.Sprintf("the test left %d entries outside its trees and temp dirs: %s", len(stray), corpusSample(stray))
			t.Errorf("%s %s: %s", rock.name, rock.version, msg)
			res.details = append(res.details, msg)
			ran = false
		}

		switch {
		case res.status == "":
			continue // filtered out by -run
		case !ran && res.status == "pass":
			res.status = "failure"
		}

		results = append(results, res)
	}

	_, _ = fmt.Fprint(os.Stdout, corpusReport(results, time.Since(start)))
}

// corpusRun installs res.rock through both backends and compares the trees.
// It fills res as it goes, so the report says how far the rock got even when
// a check ends the test.
func corpusRun(t *testing.T, res *corpusResult, watch *corpusWatch, prefix, incDir, ttBin string) {
	t.Helper()

	rock := res.rock
	home := testenv.Hermetic(t)
	start := time.Now()
	rec := &corpusRecorder{TB: t}
	// The temp dir of this test exists now; it is not a stray write.
	watch.step(t, res, "")

	// checkStray fails the test for what the step called what wrote outside
	// the trees and the temp dirs.
	checkStray := func(what string) {
		if stray := watch.step(t, res, what); len(stray) > 0 {
			rec.Errorf("%s left %d entries outside its trees and temp dirs: %s", what, len(stray), corpusSample(stray))
		}
	}

	defer func() {
		res.elapsed = time.Since(start)

		switch {
		case t.Skipped():
			res.status = "rot"
		case t.Failed():
			res.status = "failure"
			if len(rec.messages) > 0 {
				res.status = "divergence"
			}
		default:
			res.status = "pass"
		}

		// The divergences first, then the notes.
		res.details = append(slices.Clone(rec.messages), res.details...)
	}()

	trees := map[client.Backend]string{}
	errs := map[client.Backend]error{}

	for _, backend := range []client.Backend{client.BackendNative, client.BackendLua} {
		trees[backend] = t.TempDir()
		r, err := client.New(rocks.Config{
			Tree:       trees[backend],
			WorkingDir: t.TempDir(),
			Servers:    rock.servers,
			Tarantool: rocks.TarantoolConfig{
				Executable: ttBin,
				Prefix:     prefix,
				IncludeDir: incDir,
				Version:    parityTarantoolVersion,
			},
			// What each backend says it fetched and built goes to the test
			// log: the upstream output of the lua backend, the selected
			// artifact of the native one.
			Logger: slog.New(slog.NewTextHandler(corpusLog{t, backend}, nil)),
		}, client.WithBackend(backend))
		require.NoError(t, err, "New(%v)", backend)

		ctx, cancel := context.WithTimeout(t.Context(), corpusInstallTimeout)
		began := time.Now()
		errs[backend] = r.Install(ctx, rock.name, client.InstallOpts{Version: rock.version})

		cancel()
		t.Logf("%v install took %v, error: %v", backend, time.Since(began).Round(time.Second), errs[backend])
		checkStray(fmt.Sprintf("the %v install", backend))
	}

	treeNative, treeLua := trees[client.BackendNative], trees[client.BackendLua]
	errN, errL := errs[client.BackendNative], errs[client.BackendLua]

	switch {
	case errN != nil && errL != nil:
		res.details = append(res.details, "native: "+corpusCut(errN.Error()), "lua: "+corpusCut(errL.Error()))
		t.Skipf("corpus rot: %s %s installs through neither backend\nnative: %v\nlua: %v",
			rock.name, rock.version, errN, errL)
	case errN != nil:
		rec.Errorf("installs through the lua backend only; native: %v", errN)
		t.FailNow()
	case errL != nil:
		rec.Errorf("installs through the native backend only; lua: %v", errL)
		t.FailNow()
	}

	installed := installedVersions(t, treeLua)
	for _, nv := range installed {
		res.deps = append(res.deps, nv[0]+" "+nv[1])
	}

	label := rock.name + "@" + rock.version
	compareDeployed(rec, label, treeNative, treeLua)
	// The resolved dependencies differ by design (divResolvedDeps).
	compareResolvedDeps(rec, label, treeNative, treeLua)

	for _, nv := range installed {
		if slices.Contains(installedVersions(t, treeNative), nv) {
			compareRockManifests(rec, label, treeNative, treeLua, nv[0], nv[1])
		}
	}

	if !slices.ContainsFunc(installed, func(nv [2]string) bool { return nv == [2]string{rock.name, rock.version} }) {
		rec.Errorf("the lua tree does not hold %s %s: %v", rock.name, rock.version, installed)
	}

	smoke, notes := corpusSmokeCompare(t, rec, ttBin, treeNative, treeLua)
	res.smoke = smoke
	res.details = append(res.details, notes...)

	checkStray("the smoke test")

	for _, p := range corpusHomeWrites(t, home) {
		res.details = append(res.details, "note: wrote into the hermetic home: "+p)
	}
}

// corpusLog is an io.Writer that logs each write to the test, marked with
// the backend that made it.
type corpusLog struct {
	tb      testing.TB
	backend client.Backend
}

// Write logs p.
func (l corpusLog) Write(p []byte) (int, error) {
	l.tb.Logf("%v: %s", l.backend, strings.TrimRight(string(p), "\n"))

	return len(p), nil
}

// corpusRecorder is a testing.TB that records the errors the parity helpers
// report, for the corpus report, and passes everything on to the test.
type corpusRecorder struct {
	testing.TB

	messages []string
}

// Errorf records the message and reports it to the test.
func (r *corpusRecorder) Errorf(format string, args ...any) {
	r.Helper()

	msg := fmt.Sprintf(format, args...)
	r.messages = append(r.messages, corpusSummarize(msg))
	r.TB.Errorf("%s", msg)
}

// Error records the message and reports it to the test.
func (r *corpusRecorder) Error(args ...any) {
	r.Helper()
	r.Errorf("%s", fmt.Sprint(args...))
}

// corpusSummarize shortens a failure message for the report: of a testify
// failure it keeps the message and the first changed lines of the diff.
func corpusSummarize(msg string) string {
	if !strings.Contains(msg, "Error Trace:") {
		return corpusCut(msg)
	}

	var head string

	var diff []string

	inDiff := false

	for line := range strings.SplitSeq(msg, "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "Messages:"):
			head = strings.TrimSpace(strings.TrimPrefix(trimmed, "Messages:"))
		case strings.HasPrefix(trimmed, "Diff:"):
			inDiff = true
		case inDiff && (strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "+")) &&
			!strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "+++"):
			if len(diff) < corpusDiffLines {
				diff = append(diff, trimmed)
			}
		}
	}

	if head == "" {
		head = "assertion failed"
	}

	if len(diff) == 0 {
		return corpusCut(head)
	}

	return corpusCut(head) + "\n" + strings.Join(diff, "\n")
}

// corpusCut cuts s to about corpusMessageLimit bytes, flattening it to one
// line. It keeps both ends: an error says what failed first and why last.
func corpusCut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > corpusMessageLimit {
		s = strings.ToValidUTF8(s[:corpusMessageLimit/2]+" … "+s[len(s)-corpusMessageLimit/2:], "")
	}

	return s
}

// corpusSmokeCompare smoke-tests both trees: it requires every module of
// each tree's manifest and runs every command with --help, and requires the
// outcomes to agree. A module or command that fails in both trees is not a
// divergence — a module may need box.cfg or a rock nobody installed — and is
// only noted, with the lua tree's error. It returns a summary for the report
// and the notes.
func corpusSmokeCompare(t *testing.T, rec *corpusRecorder, ttBin, treeNative, treeLua string) (string, []string) {
	t.Helper()

	outN := corpusSmoke(t, ttBin, treeNative)
	outL := corpusSmoke(t, ttBin, treeLua)

	var (
		pass, differ int
		bothNames    []string
		notes        []string
	)

	names := map[string]struct{}{}
	for name := range outN {
		names[name] = struct{}{}
	}

	for name := range outL {
		names[name] = struct{}{}
	}

	for _, name := range sortedKeys(names) {
		errN, inN := outN[name]
		errL, inL := outL[name]

		switch {
		case !inN || !inL:
			// The manifests differ; compareResolvedDeps reported it.
			differ++
		case errN == "" && errL == "":
			pass++
		case errN == "":
			differ++

			rec.Errorf("smoke %s: passes in the native tree, fails in the lua tree: %s", name, errL)
		case errL == "":
			differ++

			rec.Errorf("smoke %s: passes in the lua tree, fails in the native tree: %s", name, errN)
		default:
			bothNames = append(bothNames, name)
			notes = append(notes, "smoke: "+name+" fails in both trees: "+errL)

			t.Logf("smoke %s fails in both trees: native: %s; lua: %s", name, errN, errL)
		}
	}

	summary := fmt.Sprintf("%d/%d pass", pass, len(names))
	if differ > 0 {
		summary += fmt.Sprintf("; %d differ", differ)
	}

	if len(bothNames) > 0 {
		summary += "; fail in both trees: " + strings.Join(bothNames, ", ")
	}

	return summary, notes
}

// corpusSmoke requires each module the tree manifest of root lists with the
// tree on the module paths, as `luarocks path` would set them for that one
// tree, and runs each command with --help. It returns "require <module>" and
// "run <command>" mapped to "" for success or the first line of the error.
func corpusSmoke(t *testing.T, ttBin, root string) map[string]string {
	t.Helper()

	m := readTreeManifestOf(t, root)
	paths := tree.Paths{Tree: root}
	env := append(os.Environ(),
		"LUA_PATH="+filepath.Join(paths.DeployLuaDir(), "?.lua")+";"+filepath.Join(paths.DeployLuaDir(), "?", "init.lua")+";;",
		"LUA_CPATH="+filepath.Join(paths.DeployLibDir(), "?.so")+";;",
	)
	// An empty working directory, so tarantool finds no .rocks of its own.
	dir := t.TempDir()
	out := map[string]string{}

	for module := range m.Modules {
		out["require "+module] = corpusExec(t, dir, env, ttBin, "-e", fmt.Sprintf("require(%q) os.exit(0)", module))
	}

	for command := range m.Commands {
		out["run "+command] = corpusExec(t, dir, env, filepath.Join(paths.BinDir(), command), "--help")
	}

	return out
}

// corpusExec runs name with args in dir with env and stdin empty, and
// returns "" when it exits 0, or its first line of output and the error.
func corpusExec(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), corpusSmokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env

	b, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}

	first, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")

	return corpusCut(err.Error() + ": " + first)
}

// corpusHomeWrites lists what the installs wrote into the hermetic home
// other than what upstream LuaRocks keeps there by design: its download
// cache, and the HSTS cache of the wget it downloads with.
func corpusHomeWrites(t *testing.T, home testenv.Dirs) []string {
	t.Helper()

	cache := filepath.Join(home.CacheHome, "luarocks")
	hsts := filepath.Join(home.Home, ".wget-hsts")

	var out []string

	err := filepath.WalkDir(home.Home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		switch {
		case p == cache:
			return filepath.SkipDir
		case d.IsDir(), p == hsts:
			return nil
		}

		rel, err := filepath.Rel(home.Home, p)
		if err != nil {
			return err
		}

		out = append(out, "~/"+filepath.ToSlash(rel))

		return nil
	})
	require.NoError(t, err, "walk %s", home.Home)

	return out
}

// corpusWatch attributes what appears in the writable scratch space outside
// the test's temp dirs to the step of a rock that wrote it. The sandbox's
// leftover check would fail the run for it at the end, without saying which
// rock it was. Outside the sandbox other processes write to /tmp too, so the
// watch is off there.
type corpusWatch struct {
	on   bool
	seen map[string]struct{}
	// leak is a glob of the entries the rock's own build is known to leave
	// behind (corpusRock.tmpLeak), which the watch notes and removes.
	leak string
}

// corpusSandboxEnv is set to "1" by scripts/test-in-docker.sh.
const corpusSandboxEnv = "GO_LUAROCKS_SANDBOX"

// corpusSampleSize is how many entries corpusSample names.
const corpusSampleSize = 5

func newCorpusWatch(t *testing.T, rock corpusRock) *corpusWatch {
	t.Helper()

	w := &corpusWatch{on: os.Getenv(corpusSandboxEnv) == "1", leak: rock.tmpLeak()}
	if w.on {
		w.seen = corpusScratch(t)
	}

	return w
}

// step returns the entries that are new since the last step, other than
// the known leak of the rock, which it removes and notes in res as left by
// what. A step with an empty what only takes the snapshot.
func (w *corpusWatch) step(t *testing.T, res *corpusResult, what string) []string {
	t.Helper()

	if !w.on {
		return nil
	}

	now := corpusScratch(t)
	fresh := corpusNew(w.seen, now)
	w.seen = now

	if what == "" {
		return nil
	}

	var stray, leaked []string

	for _, p := range fresh {
		if ok, _ := filepath.Match(w.leak, p); ok && w.leak != "" {
			leaked = append(leaked, p)

			require.NoError(t, os.RemoveAll(p), "remove the known leak %s", p)
			delete(w.seen, p)

			continue
		}

		stray = append(stray, p)
	}

	if len(leaked) > 0 {
		res.details = append(res.details, fmt.Sprintf("note: %s left %d entries matching %s, a known leak of the rock's build (removed): %s",
			what, len(leaked), w.leak, corpusSample(leaked)))
	}

	return stray
}

// corpusSample names the first few of paths and how many there are.
func corpusSample(paths []string) string {
	if len(paths) <= corpusSampleSize {
		return strings.Join(paths, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(paths[:corpusSampleSize], ", "), len(paths)-corpusSampleSize)
}

// corpusScratch lists the top-level entries of the writable scratch space
// the sandbox's leftover check covers — the temp dir, /tmp, /dev/shm — and of
// the Go build cache, which the sandbox leaves writable and does not check.
func corpusScratch(t *testing.T) map[string]struct{} {
	t.Helper()

	dirs := []string{os.TempDir(), "/tmp", "/dev/shm"}
	if cache := os.Getenv("GOCACHE"); cache != "" {
		dirs = append(dirs, cache)
	}

	out := map[string]struct{}{}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		require.NoError(t, err, "read %s", dir)

		for _, e := range entries {
			out[filepath.Join(dir, e.Name())] = struct{}{}
		}
	}

	return out
}

// corpusNew returns the entries of after that before lacks, sorted.
func corpusNew(before, after map[string]struct{}) []string {
	var out []string

	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, p)
		}
	}

	slices.Sort(out)

	return out
}

// corpusProbe reports whether the server at url answers an HTTP request.
func corpusProbe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, corpusProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	return resp.Body.Close()
}

// corpusReport renders the results as Markdown: a table, then the details
// of every rock that has any.
func corpusReport(results []*corpusResult, elapsed time.Duration) string {
	var b strings.Builder

	counts := map[string]int{}
	for _, res := range results {
		counts[res.status]++
	}

	b.WriteString("\n" + corpusReportBegin + "\n")
	fmt.Fprintf(&b, "## Rock corpus: backend parity\n\n%d rocks in %v: %d pass, %d divergence, %d rot, %d failure.\n\n",
		len(results), elapsed.Round(time.Second), counts["pass"], counts["divergence"], counts["rot"], counts["failure"])
	b.WriteString("The tree column lists the rocks the lua tree holds. In a manifest diff below, `-` lines are the lua tree " +
		"(the reference) and `+` lines the native tree.\n\n")
	b.WriteString("| rock | version | build | result | smoke | tree | time |\n|---|---|---|---|---|---|---|\n")

	for _, res := range results {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %v |\n", res.rock.name, res.rock.version, res.rock.build,
			res.status, corpusCell(res.smoke), corpusCell(strings.Join(res.deps, ", ")), res.elapsed.Round(time.Second))
	}

	for _, res := range results {
		if len(res.details) == 0 {
			continue
		}

		fmt.Fprintf(&b, "\n### %s %s: %s\n\n%s\n\n", res.rock.name, res.rock.version, res.status, res.rock.why)

		for _, d := range res.details {
			b.WriteString("```\n" + d + "\n```\n")
		}
	}

	b.WriteString(corpusReportEnd + "\n")

	return b.String()
}

// corpusCell makes s safe for a Markdown table cell.
func corpusCell(s string) string {
	if s == "" {
		return "-"
	}

	return strings.ReplaceAll(s, "|", `\|`)
}
