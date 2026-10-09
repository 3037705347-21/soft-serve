package reconcile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/matryer/is"
)

func TestScan(t *testing.T) {
	is := is.New(t)
	root := t.TempDir()

	// A bare repo at the top level: git.Init(bare) appends .git.
	_, err := git.Init(filepath.Join(root, "a"), true)
	is.NoErr(err)

	// A nested bare repo.
	_, err = git.Init(filepath.Join(root, "team", "b"), true)
	is.NoErr(err)

	// A plain directory that is not a git repository.
	is.NoErr(os.MkdirAll(filepath.Join(root, "notbare"), 0o755))
	is.NoErr(os.WriteFile(filepath.Join(root, "notbare", "notes.txt"), []byte("hi"), 0o600))

	// A loose file at the root.
	is.NoErr(os.WriteFile(filepath.Join(root, "README"), []byte("x"), 0o600))

	snap, err := Scan(root)
	is.NoErr(err)
	is.Equal(len(snap.Repos), 2)

	dir, ok := snap.Repos["a"]
	is.True(ok)
	is.Equal(dir, filepath.Join(root, "a.git"))

	dir, ok = snap.Repos["team/b"]
	is.True(ok)
	is.Equal(dir, filepath.Join(root, "team", "b.git"))

	_, ok = snap.Repos["notbare"]
	is.True(!ok)
}

func TestScanMissingRoot(t *testing.T) {
	is := is.New(t)
	_, err := Scan(filepath.Join(t.TempDir(), "gone"))
	is.True(err != nil)
}

func TestIsBareRepo(t *testing.T) {
	is := is.New(t)

	bare, err := git.Init(filepath.Join(t.TempDir(), "r"), true)
	is.NoErr(err)
	is.True(IsBareRepo(bare.Path))

	plain := filepath.Join(t.TempDir(), "plain")
	is.NoErr(os.MkdirAll(plain, 0o755))
	is.True(!IsBareRepo(plain))
	is.True(!IsBareRepo(filepath.Join(t.TempDir(), "missing")))
}

func TestDefaultRepoDir(t *testing.T) {
	is := is.New(t)
	got := DefaultRepoDir("/data/repos", "team/b")
	is.Equal(got, filepath.Join("/data/repos", "team", "b.git"))
}
