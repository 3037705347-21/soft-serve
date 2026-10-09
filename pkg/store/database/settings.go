package database

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

type settingsStore struct{}

var _ store.SettingStore = (*settingsStore)(nil)

// GetAllowKeylessAccess implements store.SettingStore.
func (*settingsStore) GetAllowKeylessAccess(ctx context.Context, tx db.Handler) (bool, error) {
	var allow bool
	query := tx.Rebind(`SELECT value FROM settings WHERE "key" = 'allow_keyless'`)
	if err := tx.GetContext(ctx, &allow, query); err != nil {
		return false, db.WrapError(err)
	}
	return allow, nil
}

// GetAnonAccess implements store.SettingStore.
func (*settingsStore) GetAnonAccess(ctx context.Context, tx db.Handler) (access.AccessLevel, error) {
	var level string
	query := tx.Rebind(`SELECT value FROM settings WHERE "key" = 'anon_access'`)
	if err := tx.GetContext(ctx, &level, query); err != nil {
		return access.NoAccess, db.WrapError(err)
	}
	return access.ParseAccessLevel(level), nil
}

// SetAllowKeylessAccess implements store.SettingStore.
func (*settingsStore) SetAllowKeylessAccess(ctx context.Context, tx db.Handler, allow bool) error {
	query := tx.Rebind(`UPDATE settings SET value = ?, updated_at = CURRENT_TIMESTAMP WHERE "key" = 'allow_keyless'`)
	_, err := tx.ExecContext(ctx, query, allow)
	return db.WrapError(err)
}

// SetAnonAccess implements store.SettingStore.
func (*settingsStore) SetAnonAccess(ctx context.Context, tx db.Handler, level access.AccessLevel) error {
	query := tx.Rebind(`UPDATE settings SET value = ?, updated_at = CURRENT_TIMESTAMP WHERE "key" = ?`)
	_, err := tx.ExecContext(ctx, query, level.String(), "anon_access")
	return db.WrapError(err)
}

// GetReconcileReport implements store.SettingStore. It wraps
// db.ErrRecordNotFound when no report has been persisted yet.
func (*settingsStore) GetReconcileReport(ctx context.Context, tx db.Handler) (string, error) {
	var report string
	query := tx.Rebind(`SELECT value FROM settings WHERE "key" = ?`)
	if err := tx.GetContext(ctx, &report, query, store.ReconcileReportSettingKey); err != nil {
		return "", db.WrapError(err)
	}
	return report, nil
}

// SetReconcileReport implements store.SettingStore. It upserts portably: the
// row is updated in place and inserted only when the key does not exist yet.
func (*settingsStore) SetReconcileReport(ctx context.Context, tx db.Handler, report string) error {
	query := tx.Rebind(`UPDATE settings SET value = ?, updated_at = CURRENT_TIMESTAMP WHERE "key" = ?`)
	res, err := tx.ExecContext(ctx, query, report, store.ReconcileReportSettingKey)
	if err != nil {
		return db.WrapError(err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return db.WrapError(err)
	}

	if n > 0 {
		return nil
	}

	insert := tx.Rebind(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`)
	_, err = tx.ExecContext(ctx, insert, store.ReconcileReportSettingKey, report)
	return db.WrapError(err)
}
