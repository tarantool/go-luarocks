package client_test

// Tests of the deps policies and the rock trees that run on the host: the
// rock trees the lua backend's typed methods run over (rocksTrees), and
// native installs while another rock tree, the Tarantool prefix, holds a
// dependency. TestBackendDivergence_DepsPolicy runs the same operations
// through both backends, which differ there by design.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/tree"
)

// TestRocksTrees pins the rock trees the lua backend's typed methods run
// over: Tree, then the Tarantool prefix when it is set, cleaned and without a
// repeat.
func TestRocksTrees(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  rocks.Config
		want []string
	}{
		{
			name: "tree and prefix",
			cfg:  rocks.Config{Tree: "/w/.rocks", Tarantool: rocks.TarantoolConfig{Prefix: "/opt/tt"}},
			want: []string{"/w/.rocks", "/opt/tt"},
		},
		{
			name: "without a prefix",
			cfg:  rocks.Config{Tree: "/w/.rocks/"},
			want: []string{"/w/.rocks"},
		},
		{
			name: "the prefix as the tree",
			cfg:  rocks.Config{Tree: "/opt/tt", Tarantool: rocks.TarantoolConfig{Prefix: "/opt/tt/"}},
			want: []string{"/opt/tt"},
		},
	} {
		assert.Equal(t, tc.want, client.RocksTrees(tc.cfg), tc.name)
	}
}

func newClient(t *testing.T, cfg rocks.Config, opts ...client.Option) *client.Rocks {
	t.Helper()

	r, err := client.New(cfg, opts...)
	require.NoError(t, err, "New")

	return r
}

// stageHelloSource writes hello's sources and rockspec, as installTestRepo
// serves them, into a fresh directory for Make, and returns it.
func stageHelloSource(t *testing.T, repo string) string {
	t.Helper()

	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "hello.lua"), []byte("return { name = 'hello' }\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "bin", "hello-cli"),
		[]byte("print(require('hello').name)\n"), 0o600))

	spec, err := os.ReadFile(filepath.Join(repo, "hello-1.0-1.rockspec")) //nolint:gosec // test temp path
	require.NoError(t, err, "read hello's rockspec")
	require.NoError(t, os.WriteFile(filepath.Join(src, "hello-1.0-1.rockspec"), spec, 0o600))

	return src
}

// depsOutcome is what an operation of a depsCase leaves in the target tree.
type depsOutcome struct {
	// depInstalled says whether dep is installed into the target tree.
	depInstalled bool
	// listsOther says whether hello's command wrapper lists the other tree
	// after the target one; it lists the target tree alone otherwise.
	listsOther bool
	// resolved is hello's resolved dependencies in the target tree manifest.
	resolved map[string]string
}

// depsCase gets hello, which depends on dep, into a fresh target tree while
// the other tree, which holds dep, is the Tarantool prefix: the rock tree
// after Config.Tree that the lua backend's typed methods run over. The
// backends differ here by design: the native one keeps go-luarocks' own
// semantics — DepsPolicy says whether Install resolves and installs the
// dependencies into Config.Tree, which alone it consults, Build and Make
// install none, and a command wrapper lists Config.Tree alone — while the
// lua one runs upstream, which takes the policy as --deps-mode and selects
// the rock trees by it.
type depsCase struct {
	name string
	// work returns the working directory the operation runs in.
	work func(t *testing.T, repo string) string
	run  func(r *client.Rocks, repo string) error
	// native and lua are what each backend does.
	native, lua depsOutcome
}

func depsCases() []depsCase {
	tmp := func(t *testing.T, _ string) string {
		t.Helper()

		return t.TempDir()
	}
	install := func(p client.DepsPolicy) func(r *client.Rocks, _ string) error {
		return func(r *client.Rocks, _ string) error {
			return r.Install(context.Background(), "hello", client.InstallOpts{Deps: p})
		}
	}

	installed := map[string]string{"dep": "1.0-1"}
	// A dependency satisfied from the other tree is recorded in no tree
	// manifest, as upstream's scan_deps records it: it looks for the
	// dependency's rockspec in the current tree alone.
	none := map[string]string{}

	return []depsCase{
		// --deps-mode all: dep in the other tree satisfies hello, and the
		// wrapper lists both trees.
		{"install/all", tmp, install(client.DepsAll), depsOutcome{true, false, installed}, depsOutcome{false, true, none}},
		// --deps-mode order: the target tree and the ones after it.
		{"install/only-new", tmp, install(client.DepsOnlyNew), depsOutcome{true, false, installed}, depsOutcome{false, true, none}},
		// --deps-mode none: no dependency checked, both trees listed, as
		// upstream's map_trees takes "none" for "order".
		{"install/none", tmp, install(client.DepsNone), depsOutcome{false, false, none}, depsOutcome{false, true, none}},
		// Build and Make pass no --deps-mode: upstream's default, "one",
		// installs dep into the target tree, the only one it consults.
		{"build", tmp, func(r *client.Rocks, repo string) error {
			return r.Build(context.Background(), filepath.Join(repo, "hello-1.0-1.rockspec"), client.BuildOpts{})
		}, depsOutcome{false, false, none}, depsOutcome{true, false, installed}},
		{"make", stageHelloSource, func(r *client.Rocks, _ string) error {
			return r.Make(context.Background(), client.MakeOpts{})
		}, depsOutcome{false, false, none}, depsOutcome{true, false, installed}},
	}
}

// checkDepsOutcome checks the target tree after an operation that should
// have left want.
func checkDepsOutcome(t *testing.T, want depsOutcome, target, other string) {
	t.Helper()

	_, err := os.Stat(filepath.Join(target, "share", "tarantool", "dep.lua"))
	assert.Equal(t, want.depInstalled, err == nil, "dep installed into the target tree")

	trees := []string{target}
	if want.listsOther {
		trees = append(trees, other)
	}

	assert.Equal(t, trees, wrapperTrees(t, filepath.Join(target, "bin", "hello-cli")), "the trees hello-cli lists")

	m, err := manif.FileStore{}.ReadTree(tree.Paths{Tree: target}.RocksDir())
	require.NoError(t, err, "read the target tree manifest")

	resolved := m.Repository["hello"]["1.0-1"].Dependencies
	if resolved == nil {
		resolved = map[string]string{} // the reader does not tell {} from no table
	}

	assert.Equal(t, want.resolved, resolved, "hello's resolved dependencies")
}

// wrapperPathRE captures the package.path a command wrapper sets.
var wrapperPathRE = regexp.MustCompile(`package\.path="([^"]*)"`)

// wrapperTrees returns the roots of the rock trees the command wrapper at
// path puts on package.path, in order. The wrapper passes package.path in a
// single-quoted shell word, which a single quote of a root ends, escaped with
// a backslash, before the word starts again.
func wrapperTrees(t *testing.T, path string) []string {
	t.Helper()

	body, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err, "read the command wrapper")

	match := wrapperPathRE.FindSubmatch(bytes.ReplaceAll(body, []byte(`'\''`), []byte(`'`)))
	require.NotNil(t, match, "no package.path in the wrapper:\n%s", body)

	var roots []string

	for entry := range strings.SplitSeq(string(match[1]), ";") {
		if root, ok := strings.CutSuffix(entry, "/share/tarantool/?.lua"); ok {
			roots = append(roots, root)
		}
	}

	return roots
}

// TestNativeInstall_DepsPolicy runs every depsCase natively and checks the
// native outcome: the dependency the other rock tree holds satisfies nothing,
// and the command wrapper lists Config.Tree alone.
func TestNativeInstall_DepsPolicy(t *testing.T) {
	t.Parallel()

	repo := installTestRepo(t)

	other := t.TempDir()
	require.NoError(t, semanticsClient(t, other, repo).Install(context.Background(), "dep", client.InstallOpts{}),
		"Install dep into the other tree")

	for _, tc := range depsCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := t.TempDir()
			r := newClient(t, rocks.Config{
				Tree:       target,
				WorkingDir: tc.work(t, repo),
				Servers:    []string{repo},
				Tarantool:  rocks.TarantoolConfig{Prefix: other, Version: semanticsTarantool.Version},
			})
			require.NoError(t, tc.run(r, repo), "%s hello", tc.name)

			checkDepsOutcome(t, tc.native, target, other)
		})
	}
}
