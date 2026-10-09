package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	repoQuarantineName    = "repo_quarantine"
	repoQuarantineVersion = 4
)

// repoQuarantine adds the quarantined flag to repos so that a controlled
// storage reconciliation can mark catalog rows whose on-disk repositories are
// confirmed lost. Quarantined repositories are hidden from every normal
// serving path until an administrator explicitly restores them.
var repoQuarantine = Migration{
	Name:    repoQuarantineName,
	Version: repoQuarantineVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, repoQuarantineVersion, repoQuarantineName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, repoQuarantineVersion, repoQuarantineName)
	},
}
