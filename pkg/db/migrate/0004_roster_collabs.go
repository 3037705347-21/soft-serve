package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	rosterCollabsName    = "roster_collabs"
	rosterCollabsVersion = 4
)

var rosterCollabs = Migration{
	Name:    rosterCollabsName,
	Version: rosterCollabsVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, rosterCollabsVersion, rosterCollabsName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, rosterCollabsVersion, rosterCollabsName)
	},
}
