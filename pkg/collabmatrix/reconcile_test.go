package collabmatrix_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/collabmatrix"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
)

type fixture struct {
	ctx context.Context
	dbx *db.DB
	be  *backend.Backend
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	is := is.New(t)

	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db"

	ctx := config.WithContext(context.Background(), cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))
	dbstore := database.New(ctx, dbx)
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)

	return &fixture{ctx: ctx, dbx: dbx, be: be}
}

func (f *fixture) createUser(t *testing.T, username string) {
	t.Helper()
	is := is.New(t)
	_, err := f.be.CreateUser(f.ctx, username, proto.UserOptions{})
	is.NoErr(err)
}

func (f *fixture) createRepo(t *testing.T, name string, owner string) {
	t.Helper()
	is := is.New(t)

	var ownerUser proto.User
	if owner != "" {
		u, err := f.be.User(f.ctx, owner)
		is.NoErr(err)
		ownerUser = u
	}
	_, err := f.be.CreateRepository(f.ctx, name, ownerUser, proto.RepositoryOptions{})
	is.NoErr(err)
}

// addManualCollab adds a collaborator through the regular backend path, i.e.
// it is not recorded in the matrix ledger.
func (f *fixture) addManualCollab(t *testing.T, repo, username string, level access.AccessLevel) {
	t.Helper()
	is := is.New(t)

	// webhook payload construction needs a non-nil actor.
	ctx := f.ctx
	if owner, err := f.be.User(f.ctx, "owner"); err == nil {
		ctx = proto.WithUserContext(ctx, owner)
	}
	is.NoErr(f.be.AddCollaborator(ctx, repo, username, level))
}

// level returns the effective collaborator level; NoAccess with false means
// no collaborator row exists.
func (f *fixture) level(t *testing.T, repo, username string) (access.AccessLevel, bool) {
	t.Helper()
	is := is.New(t)
	lvl, ok, err := f.be.IsCollaborator(f.ctx, repo, username)
	if errors.Is(err, db.ErrRecordNotFound) {
		return access.NoAccess, false
	}
	is.NoErr(err)
	return lvl, ok
}

func (f *fixture) reconcile(t *testing.T, content string, dryRun bool) *collabmatrix.Report {
	t.Helper()
	is := is.New(t)
	matrix, err := collabmatrix.Parse(strings.NewReader(content))
	is.NoErr(err)
	report, err := collabmatrix.Reconcile(f.ctx, f.dbx, matrix, collabmatrix.Options{DryRun: dryRun})
	is.NoErr(err)
	return report
}

func (f *fixture) reconcileErr(t *testing.T, content string) error {
	t.Helper()
	matrix, err := collabmatrix.Parse(strings.NewReader(content))
	is.New(t).NoErr(err)
	_, err = collabmatrix.Reconcile(f.ctx, f.dbx, matrix, collabmatrix.Options{})
	return err
}

type ledgerEntry struct {
	Repo        string              `db:"repo"`
	Username    string              `db:"username"`
	AccessLevel access.AccessLevel `db:"access_level"`
}

// ledger returns "repo\x00username" -> level for every managed collaborator.
func (f *fixture) ledger(t *testing.T) map[string]access.AccessLevel {
	t.Helper()
	is := is.New(t)
	var rows []ledgerEntry
	is.NoErr(f.dbx.SelectContext(f.ctx, &rows, `
		SELECT repos.name AS repo, users.username AS username, roster_collabs.access_level AS access_level
		FROM roster_collabs
		INNER JOIN repos ON repos.id = roster_collabs.repo_id
		INNER JOIN users ON users.id = roster_collabs.user_id;`))
	out := make(map[string]access.AccessLevel, len(rows))
	for _, row := range rows {
		out[row.Repo+"\x00"+row.Username] = row.AccessLevel
	}
	return out
}

func (f *fixture) countRows(t *testing.T, table string) int {
	t.Helper()
	is := is.New(t)
	var n int
	is.NoErr(f.dbx.GetContext(f.ctx, &n, "SELECT COUNT(*) FROM "+table+";"))
	return n
}

func (f *fixture) deleteCollabRow(t *testing.T, repo, username string) {
	t.Helper()
	is := is.New(t)
	_, err := f.dbx.ExecContext(f.ctx, `
		DELETE FROM collabs
		WHERE repo_id = (SELECT id FROM repos WHERE name = ?)
		  AND user_id = (SELECT id FROM users WHERE username = ?);`, repo, username)
	is.NoErr(err)
}

func TestReconcileGrantsAndIsIdempotent(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "bob", "carol", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	matrix := strings.Join([]string{
		"# matrix v1",
		matrixLine("team/api", "alice", "read-write"),
		matrixLine("team/api", "bob", "read-only"),
	}, "\n")

	report := fx.reconcile(t, matrix, false)
	is.Equal(report.Count(collabmatrix.ActionGrant), 2)

	lvl, ok := fx.level(t, "team/api", "alice")
	is.True(ok)
	is.Equal(lvl, access.ReadWriteAccess)
	lvl, ok = fx.level(t, "team/api", "bob")
	is.True(ok)
	is.Equal(lvl, access.ReadOnlyAccess)
	_, ok = fx.level(t, "team/api", "carol")
	is.True(!ok)

	is.Equal(fx.countRows(t, "collabs"), 2)
	is.Equal(fx.countRows(t, "roster_collabs"), 2)
	is.Equal(fx.ledger(t)["team/api\x00alice"], access.ReadWriteAccess)

	// Re-running the same matrix is a no-op and cannot create duplicates.
	second := fx.reconcile(t, matrix, false)
	is.Equal(len(second.Changes), 0)
	is.Equal(fx.countRows(t, "collabs"), 2)
	is.Equal(fx.countRows(t, "roster_collabs"), 2)
}

func TestReconcileRotationRevokesManagedKeepsManual(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "bob", "carol", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	fx.reconcile(t, matrixLine("team/api", "alice", "read-write"), false)
	fx.addManualCollab(t, "team/api", "carol", access.AdminAccess)

	next := strings.Join([]string{
		matrixLine("team/api", "bob", "read-only"),
		matrixLine("team/api", "carol", "admin-access"),
	}, "\n")
	report := fx.reconcile(t, next, false)
	is.Equal(report.Count(collabmatrix.ActionRevoke), 1)
	is.Equal(report.Count(collabmatrix.ActionGrant), 1)
	is.Equal(report.Count(collabmatrix.ActionAdopt), 1)

	_, ok := fx.level(t, "team/api", "alice")
	is.True(!ok)
	lvl, ok := fx.level(t, "team/api", "bob")
	is.True(ok)
	is.Equal(lvl, access.ReadOnlyAccess)
	lvl, ok = fx.level(t, "team/api", "carol")
	is.True(ok)
	is.Equal(lvl, access.AdminAccess)

	ledger := fx.ledger(t)
	is.Equal(len(ledger), 2)

	again := fx.reconcile(t, next, false)
	is.Equal(len(again.Changes), 0)
}

func TestReconcileManualConflictAbortsEverything(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "carol", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")
	fx.addManualCollab(t, "team/api", "carol", access.ReadOnlyAccess)

	// A perfectly valid grant for alice sits next to the conflicting line;
	// the all-or-nothing run must not apply it either.
	matrix := strings.Join([]string{
		matrixLine("team/api", "alice", "read-write"),
		matrixLine("team/api", "carol", "read-write"),
	}, "\n")
	err := fx.reconcileErr(t, matrix)
	is.True(errors.Is(err, collabmatrix.ErrManualAccessConflict))

	_, ok := fx.level(t, "team/api", "alice")
	is.True(!ok)
	lvl, ok := fx.level(t, "team/api", "carol")
	is.True(ok)
	is.Equal(lvl, access.ReadOnlyAccess)
	is.Equal(fx.countRows(t, "collabs"), 1)
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
}

func TestReconcileUnknownUserAbortsEverything(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	matrix := strings.Join([]string{
		matrixLine("team/api", "alice", "read-write"),
		matrixLine("team/api", "ghost", "read-only"),
	}, "\n")
	err := fx.reconcileErr(t, matrix)
	is.True(errors.Is(err, collabmatrix.ErrUserNotFound))
	is.True(strings.Contains(err.Error(), "ghost"))

	_, ok := fx.level(t, "team/api", "alice")
	is.True(!ok)
	is.Equal(fx.countRows(t, "collabs"), 0)
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
}

func TestReconcileUnknownRepoAbortsEverything(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")

	err := fx.reconcileErr(t, matrixLine("ghost/repo", "alice", "read-write"))
	is.True(errors.Is(err, collabmatrix.ErrRepoNotFound))
	is.True(strings.Contains(err.Error(), "ghost/repo"))
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
}

func TestReconcileOwnerConflictAborts(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	err := fx.reconcileErr(t, matrixLine("team/api", "owner", "read-write"))
	is.True(errors.Is(err, collabmatrix.ErrRepoOwnerConflict))
	is.Equal(fx.countRows(t, "collabs"), 0)
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
}

func TestReconcileUpdatesManagedLevelAndConverges(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "carol", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")
	fx.addManualCollab(t, "team/api", "carol", access.ReadOnlyAccess)

	before := matrixLine("team/api", "alice", "read-only")
	fx.reconcile(t, before, false)

	after := matrixLine("team/api", "alice", "read-write")
	report := fx.reconcile(t, after, false)
	is.Equal(report.Count(collabmatrix.ActionUpdate), 1)
	change := report.Changes[0]
	is.Equal(change.Username, "alice")
	is.Equal(change.FromAccessLevel, access.ReadOnlyAccess)
	is.Equal(change.AccessLevel, access.ReadWriteAccess)

	lvl, ok := fx.level(t, "team/api", "alice")
	is.True(ok)
	is.Equal(lvl, access.ReadWriteAccess)

	// The manual collaborator of another level was not touched.
	lvl, ok = fx.level(t, "team/api", "carol")
	is.True(ok)
	is.Equal(lvl, access.ReadOnlyAccess)

	again := fx.reconcile(t, after, false)
	is.Equal(len(again.Changes), 0)
}

func TestReconcileAdoptsSameLevelManualThenCanRevokeIt(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"carol", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")
	fx.addManualCollab(t, "team/api", "carol", access.ReadOnlyAccess)

	report := fx.reconcile(t, matrixLine("team/api", "carol", "read-only"), false)
	is.Equal(report.Count(collabmatrix.ActionAdopt), 1)
	is.Equal(fx.countRows(t, "collabs"), 1)
	is.Equal(fx.countRows(t, "roster_collabs"), 1)

	// Once adopted, disappearing from the matrix revokes the grant even
	// though it originally existed as a manual one.
	next := fx.reconcile(t, "# matrix emptied\n", false)
	is.Equal(next.Count(collabmatrix.ActionRevoke), 1)
	_, ok := fx.level(t, "team/api", "carol")
	is.True(!ok)
	is.Equal(fx.countRows(t, "collabs"), 0)
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
}

func TestReconcileEmptyMatrixRevokesOnlyManaged(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "carol", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	fx.reconcile(t, matrixLine("team/api", "alice", "read-write"), false)
	fx.addManualCollab(t, "team/api", "carol", access.AdminAccess)

	report := fx.reconcile(t, "# matrix emptied\n", false)
	is.Equal(report.Count(collabmatrix.ActionRevoke), 1)

	_, ok := fx.level(t, "team/api", "alice")
	is.True(!ok)
	lvl, ok := fx.level(t, "team/api", "carol")
	is.True(ok)
	is.Equal(lvl, access.AdminAccess)
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
	is.Equal(fx.countRows(t, "collabs"), 1)
}

func TestReconcileDryRunWritesNothing(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	line := matrixLine("team/api", "alice", "read-write")
	report := fx.reconcile(t, line, true)
	is.Equal(report.Count(collabmatrix.ActionGrant), 1)
	is.Equal(fx.countRows(t, "collabs"), 0)
	is.Equal(fx.countRows(t, "roster_collabs"), 0)
	_, ok := fx.level(t, "team/api", "alice")
	is.True(!ok)

	// Applying afterwards matches the dry-run plan exactly.
	applied := fx.reconcile(t, line, false)
	is.Equal(applied.Count(collabmatrix.ActionGrant), 1)
	is.Equal(fx.countRows(t, "collabs"), 1)
	is.Equal(fx.countRows(t, "roster_collabs"), 1)
}

func TestReconcileMultipleRepos(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "bob", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/a", "owner")
	fx.createRepo(t, "team/b", "owner")

	matrix := strings.Join([]string{
		matrixLine("team/a", "alice", "read-only"),
		matrixLine("team/a", "bob", "read-write"),
		matrixLine("team/b", "alice", "read-write"),
	}, "\n")
	fx.reconcile(t, matrix, false)
	is.Equal(fx.countRows(t, "collabs"), 3)
	is.Equal(fx.countRows(t, "roster_collabs"), 3)

	again := fx.reconcile(t, matrix, false)
	is.Equal(len(again.Changes), 0)

	// Removing one declaration revokes exactly that one, nothing else.
	next := strings.Join([]string{
		matrixLine("team/a", "alice", "read-only"),
		matrixLine("team/b", "alice", "read-write"),
	}, "\n")
	report := fx.reconcile(t, next, false)
	is.Equal(report.Count(collabmatrix.ActionRevoke), 1)
	is.Equal(fx.countRows(t, "collabs"), 2)

	lvl, ok := fx.level(t, "team/a", "bob")
	is.True(!ok)
	lvl, ok = fx.level(t, "team/b", "alice")
	is.True(ok)
	is.Equal(lvl, access.ReadWriteAccess)
}

// Simulate an interrupted/externally disturbed run: the ledger row survived
// but the effective collabs row was removed. Re-running repairs the grant,
// keeps a single ledger row and then converges to a no-op.
func TestReconcileRepairsMissingCollabRow(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	for _, u := range []string{"alice", "owner"} {
		fx.createUser(t, u)
	}
	fx.createRepo(t, "team/api", "owner")

	line := matrixLine("team/api", "alice", "read-write")
	fx.reconcile(t, line, false)
	fx.deleteCollabRow(t, "team/api", "alice")
	is.Equal(fx.countRows(t, "collabs"), 0)
	is.Equal(fx.countRows(t, "roster_collabs"), 1)

	report := fx.reconcile(t, line, false)
	is.Equal(report.Count(collabmatrix.ActionGrant), 1)

	lvl, ok := fx.level(t, "team/api", "alice")
	is.True(ok)
	is.Equal(lvl, access.ReadWriteAccess)
	is.Equal(fx.countRows(t, "collabs"), 1)
	is.Equal(fx.countRows(t, "roster_collabs"), 1)

	again := fx.reconcile(t, line, false)
	is.Equal(len(again.Changes), 0)
}
