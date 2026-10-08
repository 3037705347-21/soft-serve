package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	webhookPendingDeliveriesName    = "webhook_pending_deliveries"
	webhookPendingDeliveriesVersion = 4
)

var webhookPendingDeliveries = Migration{
	Name:    webhookPendingDeliveriesName,
	Version: webhookPendingDeliveriesVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, webhookPendingDeliveriesVersion, webhookPendingDeliveriesName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, webhookPendingDeliveriesVersion, webhookPendingDeliveriesName)
	},
}
