package pubkeyroster_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/pubkeyroster"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
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

// addManualKey attaches a key through the regular backend path, i.e. it is
// not recorded in the roster ledger.
func (f *fixture) addManualKey(t *testing.T, username string, pk ssh.PublicKey) {
	t.Helper()
	is := is.New(t)
	is.NoErr(f.be.AddPublicKey(f.ctx, username, pk))
}

func (f *fixture) reconcile(t *testing.T, content string, dryRun bool) *pubkeyroster.Report {
	t.Helper()
	is := is.New(t)
	roster, err := pubkeyroster.Parse(strings.NewReader(content))
	is.NoErr(err)
	report, err := pubkeyroster.Reconcile(f.ctx, f.dbx, roster, pubkeyroster.Options{DryRun: dryRun})
	is.NoErr(err)
	return report
}

func (f *fixture) reconcileErr(t *testing.T, content string) error {
	t.Helper()
	roster, err := pubkeyroster.Parse(strings.NewReader(content))
	is.New(t).NoErr(err)
	_, err = pubkeyroster.Reconcile(f.ctx, f.dbx, roster, pubkeyroster.Options{})
	return err
}

// userKeys returns the canonical authorized-key form of a user's keys.
func (f *fixture) userKeys(t *testing.T, username string) map[string]bool {
	t.Helper()
	is := is.New(t)
	pks, err := f.be.ListPublicKeys(f.ctx, username)
	is.NoErr(err)
	out := make(map[string]bool, len(pks))
	for _, pk := range pks {
		out[sshutils.MarshalAuthorizedKey(pk)] = true
	}
	return out
}

type ledgerEntry struct {
	Username  string `db:"username"`
	PublicKey string `db:"public_key"`
}

// ledger returns canonical key -> username for every roster-managed key.
func (f *fixture) ledger(t *testing.T) map[string]string {
	t.Helper()
	is := is.New(t)
	var rows []ledgerEntry
	is.NoErr(f.dbx.SelectContext(f.ctx, &rows, `
		SELECT users.username AS username, roster_public_keys.public_key AS public_key
		FROM roster_public_keys
		INNER JOIN users ON users.id = roster_public_keys.user_id;`))
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.PublicKey] = row.Username
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

func hasKey(pks map[string]bool, pk ssh.PublicKey) bool {
	return pks[sshutils.MarshalAuthorizedKey(pk)]
}

func TestReconcileAddsKeysAndIsIdempotent(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")
	fx.createUser(t, "bob")
	fx.createUser(t, "carol")

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	roster := strings.Join([]string{
		"# roster v1",
		rosterLine("alice", k1, "laptop"),
		rosterLine("bob", k2, ""),
	}, "\n")

	report := fx.reconcile(t, roster, false)
	is.Equal(report.Count(pubkeyroster.ActionAdd), 2)

	is.True(hasKey(fx.userKeys(t, "alice"), k1))
	is.True(hasKey(fx.userKeys(t, "bob"), k2))
	is.Equal(len(fx.userKeys(t, "carol")), 0)
	is.Equal(fx.countRows(t, "public_keys"), 2)
	is.Equal(fx.countRows(t, "roster_public_keys"), 2)
	is.Equal(fx.ledger(t)[sshutils.MarshalAuthorizedKey(k1)], "alice")

	// Re-running the same roster (even with a different key comment) must
	// be a no-op and must not create duplicate rows.
	rerun := strings.Join([]string{
		rosterLine("alice", k1, "rotated-comment"),
		rosterLine("bob", k2, ""),
	}, "\n")
	second := fx.reconcile(t, rerun, false)
	is.Equal(len(second.Changes), 0)
	is.Equal(fx.countRows(t, "public_keys"), 2)
	is.Equal(fx.countRows(t, "roster_public_keys"), 2)
}

func TestReconcileRotationRevokesManagedKeepsManual(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	manual := genPublicKey(t)

	fx.reconcile(t, rosterLine("alice", k1, "old"), false)
	fx.addManualKey(t, "alice", manual)

	report := fx.reconcile(t, rosterLine("alice", k2, "new"), false)
	is.Equal(report.Count(pubkeyroster.ActionRevoke), 1)
	is.Equal(report.Count(pubkeyroster.ActionAdd), 1)

	keys := fx.userKeys(t, "alice")
	is.True(hasKey(keys, k2))
	is.True(hasKey(keys, manual))
	is.True(!hasKey(keys, k1))

	ledger := fx.ledger(t)
	is.Equal(len(ledger), 1)
	is.True(ledger[sshutils.MarshalAuthorizedKey(k2)] == "alice")
}

func TestReconcileManualConflictAbortsEverything(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")
	fx.createUser(t, "bob")

	shared := genPublicKey(t)
	other := genPublicKey(t)
	fx.addManualKey(t, "bob", shared)

	// The roster also contains a perfectly valid key for alice; it must not
	// be applied either: the run is all-or-nothing.
	roster := strings.Join([]string{
		rosterLine("alice", shared, ""),
		rosterLine("alice", other, ""),
	}, "\n")
	err := fx.reconcileErr(t, roster)
	is.True(errors.Is(err, pubkeyroster.ErrManualKeyConflict))

	is.Equal(len(fx.userKeys(t, "alice")), 0)
	is.True(hasKey(fx.userKeys(t, "bob"), shared))
	is.Equal(fx.countRows(t, "public_keys"), 1)
	is.Equal(fx.countRows(t, "roster_public_keys"), 0)
}

func TestReconcileUnknownUserAbortsEverything(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	roster := strings.Join([]string{
		rosterLine("alice", k1, ""),
		rosterLine("ghost", k2, ""),
	}, "\n")
	err := fx.reconcileErr(t, roster)
	is.True(errors.Is(err, pubkeyroster.ErrUserNotFound))
	is.True(strings.Contains(err.Error(), "ghost"))

	is.Equal(len(fx.userKeys(t, "alice")), 0)
	is.Equal(fx.countRows(t, "public_keys"), 0)
	is.Equal(fx.countRows(t, "roster_public_keys"), 0)
}

func TestReconcileMovesManagedKeyBetweenUsers(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")
	fx.createUser(t, "bob")

	k1 := genPublicKey(t)
	aliceManual := genPublicKey(t)

	fx.reconcile(t, rosterLine("alice", k1, ""), false)
	fx.addManualKey(t, "alice", aliceManual)

	report := fx.reconcile(t, rosterLine("bob", k1, ""), false)
	is.Equal(report.Count(pubkeyroster.ActionMove), 1)
	move := report.Changes[0]
	is.Equal(move.Username, "bob")
	is.Equal(move.FromUsername, "alice")

	is.True(hasKey(fx.userKeys(t, "bob"), k1))
	is.Equal(len(fx.userKeys(t, "alice")), 1)
	is.True(hasKey(fx.userKeys(t, "alice"), aliceManual))

	ledger := fx.ledger(t)
	is.Equal(len(ledger), 1)
	is.True(ledger[sshutils.MarshalAuthorizedKey(k1)] == "bob")

	// Another rerun after the move converges to a no-op.
	again := fx.reconcile(t, rosterLine("bob", k1, ""), false)
	is.Equal(len(again.Changes), 0)
}

func TestReconcileAdoptsManualKeyThenCanRevokeIt(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")
	fx.createUser(t, "bob")

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	fx.addManualKey(t, "alice", k1)

	report := fx.reconcile(t, rosterLine("alice", k1, ""), false)
	is.Equal(report.Count(pubkeyroster.ActionAdopt), 1)
	is.Equal(fx.countRows(t, "public_keys"), 1)
	is.Equal(fx.countRows(t, "roster_public_keys"), 1)

	// Once adopted, dropping the key from the roster revokes it even though
	// it originally existed as a manual key.
	next := fx.reconcile(t, rosterLine("bob", k2, ""), false)
	is.Equal(next.Count(pubkeyroster.ActionRevoke), 1)
	is.Equal(len(fx.userKeys(t, "alice")), 0)
	is.True(hasKey(fx.userKeys(t, "bob"), k2))
}

func TestReconcileEmptyRosterRevokesOnlyManagedKeys(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")
	fx.createUser(t, "bob")

	managed := genPublicKey(t)
	manual := genPublicKey(t)
	bobManual := genPublicKey(t)

	fx.reconcile(t, rosterLine("alice", managed, ""), false)
	fx.addManualKey(t, "alice", manual)
	fx.addManualKey(t, "bob", bobManual)

	report := fx.reconcile(t, "# roster emptied\n", false)
	is.Equal(report.Count(pubkeyroster.ActionRevoke), 1)

	is.True(hasKey(fx.userKeys(t, "alice"), manual))
	is.True(!hasKey(fx.userKeys(t, "alice"), managed))
	is.True(hasKey(fx.userKeys(t, "bob"), bobManual))
	is.Equal(fx.countRows(t, "roster_public_keys"), 0)
	is.Equal(fx.countRows(t, "public_keys"), 2)
}

func TestReconcileDryRunWritesNothing(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")

	k1 := genPublicKey(t)
	report := fx.reconcile(t, rosterLine("alice", k1, ""), true)
	is.Equal(report.Count(pubkeyroster.ActionAdd), 1)
	is.Equal(fx.countRows(t, "public_keys"), 0)
	is.Equal(fx.countRows(t, "roster_public_keys"), 0)
	is.Equal(len(fx.userKeys(t, "alice")), 0)

	// Applying afterwards matches the dry-run plan exactly.
	applied := fx.reconcile(t, rosterLine("alice", k1, ""), false)
	is.Equal(applied.Count(pubkeyroster.ActionAdd), 1)
	is.Equal(fx.countRows(t, "public_keys"), 1)
}

func TestReconcileDryRunReportsConflictsWithoutWrites(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")
	fx.createUser(t, "bob")

	shared := genPublicKey(t)
	fx.addManualKey(t, "bob", shared)

	err := fx.reconcileErr(t, rosterLine("alice", shared, ""))
	is.True(errors.Is(err, pubkeyroster.ErrManualKeyConflict))
	is.True(hasKey(fx.userKeys(t, "bob"), shared))
	is.Equal(fx.countRows(t, "roster_public_keys"), 0)
}

func TestReconcileMultipleKeysPerUser(t *testing.T) {
	is := is.New(t)
	fx := newFixture(t)
	fx.createUser(t, "alice")

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	k3 := genPublicKey(t)
	roster := strings.Join([]string{
		rosterLine("alice", k1, "one"),
		rosterLine("alice", k2, "two"),
		rosterLine("alice", k3, "three"),
	}, "\n")

	fx.reconcile(t, roster, false)
	keys := fx.userKeys(t, "alice")
	is.Equal(len(keys), 3)
	is.True(hasKey(keys, k1))
	is.True(hasKey(keys, k2))
	is.True(hasKey(keys, k3))

	// Removing two lines in the next roster revokes exactly those.
	next := rosterLine("alice", k2, "two")
	report := fx.reconcile(t, next, false)
	is.Equal(report.Count(pubkeyroster.ActionRevoke), 2)
	keys = fx.userKeys(t, "alice")
	is.Equal(len(keys), 1)
	is.True(hasKey(keys, k2))
}
