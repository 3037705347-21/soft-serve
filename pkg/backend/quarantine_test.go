package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// quarantineFlag flips the quarantined column directly; the reconciliation
// executor gets its own end-to-end coverage in reconcile_test.go.
func quarantineFlag(t *testing.T, be *Backend, name string, quarantined bool) {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.SetRepoQuarantinedByName(ctx, tx, name, quarantined)
	}))
}

func TestQuarantinedRepositoryExcludedAndRefused(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "q", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "ok", nil, proto.RepositoryOptions{})
	is.NoErr(err)

	// Lose the directory then quarantine the row.
	is.NoErr(os.RemoveAll(be.repoPath("q")))
	quarantineFlag(t, be, "q", true)

	// Listings exclude it while healthy repositories remain visible.
	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	for _, r := range repos {
		is.True(r.Name() != "q")
	}
	var foundOK bool
	for _, r := range repos {
		if r.Name() == "ok" {
			foundOK = true
		}
	}
	is.True(foundOK)

	// Direct resolution is refused with the sentinel.
	_, err = be.Repository(ctx, "q")
	is.True(errors.Is(err, proto.ErrRepoQuarantined))

	// Even when the directory reappears, the quarantine holds.
	_, err = git.Init(filepath.Join(cfg.DataPath, "repos", "q"), true)
	is.NoErr(err)
	_, err = be.Repository(ctx, "q")
	is.True(errors.Is(err, proto.ErrRepoQuarantined))

	// The quarantined row is still visible through the admin listing.
	quarantined, err := be.QuarantinedRepos(ctx)
	is.NoErr(err)
	is.Equal(len(quarantined), 1)
	is.Equal(quarantined[0].Name, "q")
}

func TestUnquarantineRepository(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "q", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(be.repoPath("q")))
	quarantineFlag(t, be, "q", true)

	// Missing directory: unquarantine refused, flag unchanged.
	err = be.UnquarantineRepository(ctx, "q")
	is.True(err != nil)
	_, err = be.Repository(ctx, "q")
	is.True(errors.Is(err, proto.ErrRepoQuarantined))

	// Unquarantining a non-quarantined repository is also an error.
	_, err = be.CreateRepository(ctx, "plain", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.True(be.UnquarantineRepository(ctx, "plain") != nil)

	// Directory restored: unquarantine succeeds and service resumes.
	_, err = git.Init(be.repoPath("q"), true)
	is.NoErr(err)
	is.NoErr(be.UnquarantineRepository(ctx, "q"))
	r, err := be.Repository(ctx, "q")
	is.NoErr(err)
	is.Equal(r.Name(), "q")
}

func TestDeleteQuarantinedRepository(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "q", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(be.repoPath("q")))
	quarantineFlag(t, be, "q", true)

	is.NoErr(be.DeleteRepository(ctx, "q"))

	err = be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		_, err := be.store.GetRepoByName(ctx, tx, "q")
		return err
	})
	is.True(errors.Is(db.WrapError(err), db.ErrRecordNotFound))
}
