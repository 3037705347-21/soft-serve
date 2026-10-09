package reconcile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/soft-serve/git"
)

// Snapshot is a read-only view of the bare repositories found under the
// repositories root. Repos maps logical repository names (e.g. "team/b")
// to their absolute on-disk bare repository directories.
type Snapshot struct {
	ReposRoot string
	Repos     map[string]string
}

// Scan walks the repositories root and records every bare git repository.
// Directories that are not bare repositories are descended into so nested
// repositories are found; recognized bare repositories are not descended
// into. A missing root is an error rather than an empty snapshot.
func Scan(reposRoot string) (*Snapshot, error) {
	info, err := os.Stat(reposRoot)
	if err != nil {
		return nil, fmt.Errorf("scan repositories root %q: %w", reposRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("scan repositories root %q: not a directory", reposRoot)
	}

	snap := &Snapshot{
		ReposRoot: reposRoot,
		Repos:     make(map[string]string),
	}

	err = filepath.WalkDir(reposRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p == reposRoot {
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		if IsBareRepo(p) {
			rel, err := filepath.Rel(reposRoot, p)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(strings.TrimSuffix(rel, ".git"))
			snap.Repos[name] = p
			// Never descend into a repository directory.
			return fs.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan repositories root %q: %w", reposRoot, err)
	}

	return snap, nil
}

// IsBareRepo reports whether dir exists, is a directory, and can be opened
// as a bare git repository. It is the single validity check shared by the
// planner and the backend's unquarantine path.
func IsBareRepo(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}

	r, err := git.Open(dir)
	if err != nil {
		return false
	}

	return r.IsBare
}

// DefaultRepoDir returns the conventional on-disk directory for a catalog
// name: <reposRoot>/<name with slashes as OS separators>.git.
func DefaultRepoDir(reposRoot, name string) string {
	return filepath.Join(reposRoot, filepath.FromSlash(name)+".git")
}
