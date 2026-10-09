package migrate

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/matryer/is"
)

// TestRepoQuarantineMigration verifies the 0004 migration adds the
// quarantined column defaulting to false, the column is writable, and a
// rollback/re-migrate round trip preserves the rest of the repos row.
func TestRepoQuarantineMigration(t *testing.T) {
	is := is.New(t)
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.OpenSqlite(ctx, t)
	is.NoErr(err)

	is.NoErr(Migrate(ctx, dbx))

	// Existing and freshly inserted rows default to not quarantined.
	_, err = dbx.ExecContext(ctx, `INSERT INTO repos
		(name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES ('repo-a', 'Repo A', '', false, false, false, 1, CURRENT_TIMESTAMP)`)
	is.NoErr(err)

	var quarantined bool
	is.NoErr(dbx.GetContext(ctx, &quarantined,
		`SELECT quarantined FROM repos WHERE name = 'repo-a'`))
	is.Equal(quarantined, false)

	// The column is writable.
	_, err = dbx.ExecContext(ctx, `UPDATE repos SET quarantined = TRUE WHERE name = 'repo-a'`)
	is.NoErr(err)
	is.NoErr(dbx.GetContext(ctx, &quarantined,
		`SELECT quarantined FROM repos WHERE name = 'repo-a'`))
	is.Equal(quarantined, true)

	// Rollback drops the column without damaging the rest of the row.
	is.NoErr(Rollback(ctx, dbx))

	var name string
	is.NoErr(dbx.GetContext(ctx, &name,
		`SELECT name FROM repos WHERE name = 'repo-a'`))
	is.Equal(name, "repo-a")

	var colCount int
	is.NoErr(dbx.GetContext(ctx, &colCount,
		`SELECT COUNT(*) FROM pragma_table_info('repos') WHERE name = 'quarantined'`))
	is.Equal(colCount, 0)

	// Re-migrating restores the column with the default.
	is.NoErr(Migrate(ctx, dbx))
	is.NoErr(dbx.GetContext(ctx, &quarantined,
		`SELECT quarantined FROM repos WHERE name = 'repo-a'`))
	is.Equal(quarantined, false)
}
