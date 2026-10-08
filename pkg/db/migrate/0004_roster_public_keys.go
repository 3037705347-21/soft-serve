package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	rosterPublicKeysName    = "roster_public_keys"
	rosterPublicKeysVersion = 4
)

var rosterPublicKeys = Migration{
	Name:    rosterPublicKeysName,
	Version: rosterPublicKeysVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, rosterPublicKeysVersion, rosterPublicKeysName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, rosterPublicKeysVersion, rosterPublicKeysName)
	},
}
