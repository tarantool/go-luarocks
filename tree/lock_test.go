package tree_test

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/tree"
)

// skipUnlocked skips a test of the tree lock's exclusion where the lock
// guards nothing.
func skipUnlocked(t *testing.T) {
	t.Helper()

	if !slices.Contains([]string{"linux", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd"}, runtime.GOOS) {
		t.Skipf("no flock(2) on %s: the tree lock guards nothing", runtime.GOOS)
	}
}

// recordRock records name/1.0-1 in the tree through Update, as a rock with
// the one module name: Deploy gets a deploy that writes nothing and returns
// a rock_manifest listing name.lua. Between reading the manifest and writing
// it back the Update yields, so that writers that did not exclude each other
// would lose each other's rocks.
func recordRock(ctx context.Context, tr *tree.Tree, name string) error {
	spec := &rocks.Rockspec{Package: name, Version: "1.0-1"}

	return tr.Update(ctx, func(tx *tree.Tx) error {
		return tx.Deploy(spec, func(*tree.Tree) (*rocks.RockManifest, error) {
			time.Sleep(time.Millisecond)

			return &rocks.RockManifest{Lua: map[string]string{name + ".lua": "0"}}, nil
		})
	})
}

// installedNames returns the names of the rocks the tree manifest of tr lists.
func installedNames(t *testing.T, tr *tree.Tree) []string {
	t.Helper()

	m, err := tr.Store.ReadTree(tr.RocksDir())
	require.NoError(t, err, "read the tree manifest")

	out := make([]string, 0, len(m.Repository))
	for name := range m.Repository {
		out = append(out, name)
	}

	slices.Sort(out)

	return out
}

// TestUpdate_ConcurrentGoroutines: Updates of one tree from many goroutines,
// each through a Tree of its own, run one at a time, so every rock each
// records is in the manifest at the end.
func TestUpdate_ConcurrentGoroutines(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	root := t.TempDir()

	const (
		writers = 8
		rounds  = 5
	)

	var wg sync.WaitGroup

	want := make([]string, 0, writers*rounds)

	errs := make(chan error, writers*rounds)

	for w := range writers {
		for r := range rounds {
			want = append(want, fmt.Sprintf("g%d-%d", w, r))
		}

		wg.Go(func() {
			tr, err := tree.Open(rocks.Config{Tree: root})
			if err != nil {
				errs <- err

				return
			}

			for r := range rounds {
				errs <- recordRock(context.Background(), tr, fmt.Sprintf("g%d-%d", w, r))
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "Update")
	}

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	slices.Sort(want)
	assert.Equal(t, want, installedNames(t, tr), "no rock recorded is lost")
}

// childEnv names the tree and the writer id of a child process of
// TestUpdate_ConcurrentProcesses.
const childEnv = "GO_LUAROCKS_TREE_LOCK_CHILD"

// childRounds is how many rocks each child process records.
const childRounds = 10

// TestUpdate_ChildProcess is the body of a child process of
// TestUpdate_ConcurrentProcesses; it is skipped when run on its own.
func TestUpdate_ChildProcess(t *testing.T) {
	t.Parallel()

	spec := os.Getenv(childEnv)
	if spec == "" {
		t.Skip("run by TestUpdate_ConcurrentProcesses only")
	}

	id, root, ok := strings.Cut(spec, ":")
	require.True(t, ok, "%s=%q", childEnv, spec)

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	for r := range childRounds {
		require.NoError(t, recordRock(context.Background(), tr, fmt.Sprintf("p%s-%d", id, r)), "Update")
	}
}

// TestUpdate_ConcurrentProcesses: Updates of one tree from several processes
// at once — this test binary run again — run one at a time, so every rock
// each process records is in the manifest at the end.
func TestUpdate_ConcurrentProcesses(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	if os.Getenv(childEnv) != "" {
		t.Skip("a child process runs TestUpdate_ChildProcess only")
	}

	exe, err := os.Executable()
	require.NoError(t, err)

	root := t.TempDir()

	const children = 4

	want := make([]string, 0, children*childRounds)
	cmds := make([]*exec.Cmd, 0, children)

	for c := range children {
		for r := range childRounds {
			want = append(want, fmt.Sprintf("p%d-%d", c, r))
		}

		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestUpdate_ChildProcess$", "-test.count=1")

		cmd.Env = append(os.Environ(), childEnv+"="+strconv.Itoa(c)+":"+root)
		require.NoError(t, cmd.Start(), "start child %d", c)

		cmds = append(cmds, cmd)
	}

	for c, cmd := range cmds {
		require.NoError(t, cmd.Wait(), "child %d", c)
	}

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	slices.Sort(want)
	assert.Equal(t, want, installedNames(t, tr), "no rock recorded is lost")
}

// TestUpdate_WaitsUntilContextEnds: an Update of a tree another holds waits,
// and fails with ErrLocked and the context's cause when the context ends
// first; once the holder is done it goes through.
func TestUpdate_WaitsUntilContextEnds(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	root := t.TempDir()

	holder, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	other, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- holder.Update(context.Background(), func(*tree.Tx) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	called := false
	err = other.Update(ctx, func(*tree.Tx) error {
		called = true

		return nil
	})
	require.ErrorIs(t, err, tree.ErrLocked, "Update of a held tree")
	require.ErrorIs(t, err, context.DeadlineExceeded, "the cause of the context's end")
	assert.False(t, called, "fn does not run without the lock")

	close(release)
	require.NoError(t, <-done, "the holder's Update")
	require.NoError(t, recordRock(context.Background(), other, "after"), "Update once the holder is done")
}

// holdUpdate runs an Update of the tree at root that holds the tree's lock
// until the returned release is called, which waits for the Update to end.
func holdUpdate(t *testing.T, root string) (release func()) {
	t.Helper()

	holder, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	held := make(chan struct{})
	done := make(chan error, 1)
	stop := make(chan struct{})

	go func() {
		done <- holder.Update(context.Background(), func(*tree.Tx) error {
			close(held)
			<-stop

			return nil
		})
	}()

	<-held

	return func() {
		close(stop)
		require.NoError(t, <-done, "the holder's Update")
	}
}

// TestInspect_WaitsUntilContextEnds: Inspect and Tree.View of a tree an
// Update holds wait for it, and fail with ErrLocked and the context's cause
// when the context ends first; once the Update is done they read the tree.
func TestInspect_WaitsUntilContextEnds(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	root := t.TempDir()
	release := holdUpdate(t, root)

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	for name, read := range map[string]func(ctx context.Context) (*tree.View, error){
		"Inspect":   func(ctx context.Context) (*tree.View, error) { return tree.Inspect(ctx, root) },
		"Tree.View": tr.View,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := read(ctx)

		cancel()
		require.ErrorIs(t, err, tree.ErrLocked, "%s of a held tree", name)
		require.ErrorIs(t, err, context.DeadlineExceeded, "%s: the cause of the context's end", name)
	}

	release()

	v, err := tree.Inspect(context.Background(), root)
	require.NoError(t, err, "Inspect once the Update is done")
	assert.Empty(t, v.Rocks())
}

// TestLockFile_NotTreeContents: the lock file the changes of a tree leave in
// it is the library's, not the tree's: Check finds nothing wrong with it, a
// graft of the tree neither reports nor takes it, the grafted tree having a
// lock of its own, and neither Pack nor PackStaged packs it.
func TestLockFile_NotTreeContents(t *testing.T) {
	t.Parallel()

	src := buildTree(t, alpha1)
	lock := tree.Paths{Tree: src}.LockFile()
	require.FileExists(t, lock, "the lock of the changes that built the tree")
	require.NoError(t, os.WriteFile(lock, []byte("the source's\n"), 0o600))

	assert.Empty(t, check(t, src).Issues, "Check")

	dst := t.TempDir()
	report, err := graft(t, dst, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft")
	assert.Empty(t, report.Untracked, "Untracked")
	assert.Empty(t, readFile(t, tree.Paths{Tree: dst}.LockFile()), "the grafted tree's own lock")

	spec := stagedSpec(t, "staged", "1.0-1")
	staged := filepath.Join(t.TempDir(), "staged-1.0-1.all.rock")
	require.NoError(t, client.PackStaged(spec, stageRockDir(t, spec), staged, client.PackStagedOpts{}), "PackStaged")

	for _, archive := range []string{packRock(t, alpha1), staged} {
		zr, err := zip.OpenReader(archive)
		require.NoError(t, err)

		for _, f := range zr.File {
			assert.NotEqual(t, ".lock", path.Base(f.Name), "%s: %s", archive, f.Name)
		}

		require.NoError(t, zr.Close())
	}
}
