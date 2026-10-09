package store

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// ReconcileReportSettingKey is the settings key under which the latest
// storage reconciliation report is persisted as a JSON string.
const ReconcileReportSettingKey = "storage_reconcile_report"

// SettingStore is an interface for managing settings.
type SettingStore interface {
	GetAnonAccess(ctx context.Context, h db.Handler) (access.AccessLevel, error)
	SetAnonAccess(ctx context.Context, h db.Handler, level access.AccessLevel) error
	GetAllowKeylessAccess(ctx context.Context, h db.Handler) (bool, error)
	SetAllowKeylessAccess(ctx context.Context, h db.Handler, allow bool) error
	GetReconcileReport(ctx context.Context, h db.Handler) (string, error)
	SetReconcileReport(ctx context.Context, h db.Handler, report string) error
}
