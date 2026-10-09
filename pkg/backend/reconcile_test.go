package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/reconcile"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
)

// writeReconcileManifest writes a YAML manifest into a temp directory and
// returns its path.
func writeReconcileManifest(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (d *Backend) testRepoRow(t *testing.T, ctx context.Context, name string) models.Repo {
	t.Helper()
	var m models.Repo
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.GetRepoByName(ctx, tx, name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func (d *Backend) testRepoExists(t *testing.T, ctx context.Context, name string) bool {
	t.Helper()
	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		_, err := d.store.GetRepoByName(ctx, tx, name)
		return db.WrapError(err)
	})
	return err == nil
}

// reopenBackend builds a second backend over the same data directory and
// database, simulating a process restart.
func reopenBackend(t *testing.T, cfg *config.Config) *Backend {
	t.Helper()
	ctx := config.WithContext(context.Background(), cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	return New(ctx, cfg, dbx, dbstore)
}

// TestReconcileQuarantine verifies a declared lost repository is flagged and
// nothing else changes.
func TestReconcileQuarantine(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	_, err := be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "keep", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))

	manifest := writeReconcileManifest(t, "version: 1\nquarantine:\n  - name: gone\n    reason: disk failed\n")
	report, err := be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)
	is.Equal(report.Status, reconcile.StatusSuccess)
	is.Equal(len(report.Items), 1)
	is.Equal(report.Items[0].Result, reconcile.ResultQuarantine)

	is.Equal(be.testRepoRow(t, ctx, "gone").Quarantined, true)
	is.Equal(be.testRepoRow(t, ctx, "keep").Quarantined, false)

	// The report was persisted.
	persisted, err := be.GetLastReconcileReport(ctx)
	is.NoErr(err)
	is.True(persisted != nil)
	is.Equal(persisted.ManifestPath, manifest)
	is.Equal(persisted.Items[0].Name, "gone")
}

// TestReconcileAdopt verifies a declared bare repository is registered with
// hooks and metadata and becomes servable.
func TestReconcileAdopt(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	_, err := git.Init(filepath.Join(reposRoot, "team", "b"), true)
	is.NoErr(err)

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	manifest := writeReconcileManifest(t, `
version: 1
adopt:
  - name: team/b
    owner: admin
    project_name: Team B
    description: recovered
`)
	report, err := be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)
	is.Equal(report.Status, reconcile.StatusSuccess)
	is.Equal(report.Items[0].Result, reconcile.ResultAdopt)

	r, err := be.Repository(ctx, "team/b")
	is.NoErr(err)
	is.Equal(r.UserID(), admin.ID())
	is.Equal(r.ProjectName(), "Team B")

	dir := filepath.Join(reposRoot, "team", "b.git")
	desc, err := os.ReadFile(filepath.Join(dir, "description"))
	is.NoErr(err)
	is.Equal(string(desc), "recovered")
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	is.NoErr(err)
	_, err = os.Stat(filepath.Join(dir, "hooks", "update"))
	is.NoErr(err)
}

// TestReconcileUntouched verifies undeclared orphan directories and
// unconfirmed missing rows are reported but never modified.
func TestReconcileUntouched(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	// Undeclared orphan bare repository.
	orphan, err := git.Init(filepath.Join(reposRoot, "x"), true)
	is.NoErr(err)
	headInfo, err := os.Stat(orphan.Path)
	is.NoErr(err)
	headContent, err := os.ReadFile(filepath.Join(orphan.Path, "HEAD"))
	is.NoErr(err)

	// Registered row whose directory silently vanished, not in manifest.
	_, err = be.CreateRepository(ctx, "y", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "y.git")))

	manifest := writeReconcileManifest(t, "version: 1\n")
	report, err := be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)

	is.Equal(len(report.UndeclaredOrphans), 1)
	is.Equal(report.UndeclaredOrphans[0], "x")
	is.Equal(len(report.UnconfirmedMissing), 1)
	is.Equal(report.UnconfirmedMissing[0], "y")

	// The undeclared directory was not written to in any way.
	after, err := os.Stat(orphan.Path)
	is.NoErr(err)
	is.Equal(after.ModTime(), headInfo.ModTime())
	afterContent, err := os.ReadFile(filepath.Join(orphan.Path, "HEAD"))
	is.NoErr(err)
	is.Equal(string(afterContent), string(headContent))

	// The unconfirmed row was not quarantined.
	is.Equal(be.testRepoRow(t, ctx, "y").Quarantined, false)
}

// TestReconcileFailures verifies mixed manifests commit the good item while
// every conflict/failed item stays without partial state.
func TestReconcileFailures(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	// Succeeds: registered row, directory gone.
	_, err := be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))

	// Conflict: quarantine name with directory present.
	_, err = be.CreateRepository(ctx, "present", nil, proto.RepositoryOptions{})
	is.NoErr(err)

	// Conflict: quarantine name not in the catalog.
	// Conflict: adopt path that is not a bare repository.
	is.NoErr(os.MkdirAll(filepath.Join(reposRoot, "broken.git"), 0o755))

	// Failed: valid bare dir, actionable adopt, but owner does not exist.
	_, err = git.Init(filepath.Join(reposRoot, "nobody"), true)
	is.NoErr(err)

	manifest := writeReconcileManifest(t, `
version: 1
quarantine:
  - name: gone
  - name: present
  - name: ghost
adopt:
  - name: broken
  - name: nobody
    owner: ghost-user
`)
	report, err := be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)
	is.Equal(report.Status, reconcile.StatusPartial)
	is.True(report.HasFailures())

	results := map[string]reconcile.Result{}
	for _, item := range report.Items {
		results[item.Name] = item.Result
	}
	is.Equal(results["gone"], reconcile.ResultQuarantine)
	is.Equal(results["present"], reconcile.ResultConflict)
	is.Equal(results["ghost"], reconcile.ResultConflict)
	is.Equal(results["broken"], reconcile.ResultConflict)
	is.Equal(results["nobody"], reconcile.ResultFailed)

	// No half-state: failed items created no rows, conflicts flipped no flags.
	is.Equal(be.testRepoRow(t, ctx, "gone").Quarantined, true)
	is.Equal(be.testRepoRow(t, ctx, "present").Quarantined, false)
	is.True(!be.testRepoExists(t, ctx, "broken"))
	is.True(!be.testRepoExists(t, ctx, "nobody"))

	// The failure report is persisted.
	persisted, err := be.GetLastReconcileReport(ctx)
	is.NoErr(err)
	is.Equal(persisted.Status, reconcile.StatusPartial)
}

// TestReconcileIdempotent verifies repeated runs converge to a single final
// state with only already/no-op results after the first success.
func TestReconcileIdempotent(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	_, err := be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))
	_, err = git.Init(filepath.Join(reposRoot, "team", "b"), true)
	is.NoErr(err)

	manifest := writeReconcileManifest(t, `
version: 1
quarantine:
  - name: gone
adopt:
  - name: team/b
`)

	run1, err := be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)
	is.Equal(run1.Status, reconcile.StatusSuccess)

	snapshotRows := func() map[string]bool {
		flags := map[string]bool{}
		is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
			rows, err := be.store.GetAllRepos(ctx, tx)
			if err != nil {
				return err
			}
			for _, r := range rows {
				flags[r.Name] = r.Quarantined
			}
			return nil
		}))
		return flags
	}
	afterFirst := snapshotRows()

	for i := 0; i < 2; i++ {
		r, err := be.ReconcileStorage(ctx, manifest, false)
		is.NoErr(err)
		is.Equal(r.Status, reconcile.StatusSuccess)
		is.True(!r.HasFailures())
		results := map[string]reconcile.Result{}
		for _, item := range r.Items {
			results[item.Name] = item.Result
		}
		is.Equal(results["gone"], reconcile.ResultAlreadyQuarantined)
		is.Equal(results["team/b"], reconcile.ResultAlreadyRegistered)
	}

	is.Equal(snapshotRows(), afterFirst)
}

// TestReconcileDryRun verifies dry runs change nothing and never overwrite
// the persisted report.
func TestReconcileDryRun(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	_, err := be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))
	realManifest := writeReconcileManifest(t, "version: 1\nquarantine:\n  - name: gone\n")
	_, err = be.ReconcileStorage(ctx, realManifest, false)
	is.NoErr(err)

	// A dry run for a different manifest.
	_, err = git.Init(filepath.Join(reposRoot, "fresh"), true)
	is.NoErr(err)
	dryManifest := writeReconcileManifest(t, "version: 1\nadopt:\n  - name: fresh\n")
	dry, err := be.ReconcileStorage(ctx, dryManifest, true)
	is.NoErr(err)
	is.Equal(dry.DryRun, true)
	is.Equal(dry.Status, reconcile.StatusSuccess)
	is.Equal(dry.Items[0].Result, reconcile.ResultAdopt)

	// No row was created for the dry-run adoption.
	is.True(!be.testRepoExists(t, ctx, "fresh"))

	// The persisted report still reflects the real run.
	persisted, err := be.GetLastReconcileReport(ctx)
	is.NoErr(err)
	is.Equal(persisted.ManifestPath, realManifest)
	is.Equal(persisted.DryRun, false)
}

// TestReconcileReportPersistence verifies the settings upsert (single key,
// overwritten in place), empty state, and readability from a fresh backend
// simulating a restart.
func TestReconcileReportPersistence(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	// Empty state before any run.
	empty, err := be.GetLastReconcileReport(ctx)
	is.NoErr(err)
	is.True(empty == nil)

	_, err = be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))
	manifest := writeReconcileManifest(t, "version: 1\nquarantine:\n  - name: gone\n")
	report, err := be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)
	is.Equal(report.Aligned, 0)

	// A second run upserts in place: still one settings row.
	_, err = be.ReconcileStorage(ctx, manifest, false)
	is.NoErr(err)
	var count int
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return tx.GetContext(ctx, &count,
			`SELECT COUNT(*) FROM settings WHERE key = 'storage_reconcile_report'`)
	}))
	is.Equal(count, 1)

	// Readable after "restart" with a fresh backend over the same data.
	restarted := reopenBackend(t, cfg)
	persisted, err := restarted.GetLastReconcileReport(ctx)
	is.NoErr(err)
	is.True(persisted != nil)
	is.Equal(persisted.ManifestPath, manifest)
	is.Equal(persisted.Status, reconcile.StatusSuccess)
	is.True(!persisted.StartedAt.IsZero())
	is.True(!persisted.FinishedAt.IsZero())
	is.Equal(len(persisted.Items), 1)
	is.Equal(persisted.Items[0].Name, "gone")
}

// TestReconcileInvalidManifestNoSideEffects verifies a rejected manifest
// touches neither catalog nor disk.
func TestReconcileInvalidManifestNoSideEffects(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	manifest := writeReconcileManifest(t, "version: 1\nquarantine:\n  - name: 'bad name'\n")
	_, err := be.ReconcileStorage(ctx, manifest, false)
	is.True(err != nil)

	persisted, err := be.GetLastReconcileReport(ctx)
	is.NoErr(err)
	is.True(persisted == nil)

	// Sanity: the store interface wiring is intact through the run path.
	var _ store.Store = be.store
}
