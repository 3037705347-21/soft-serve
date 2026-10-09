package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/reconcile"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// ReconcileStorage runs the controlled storage reconciliation driven by the
// manifest at manifestPath. It aligns catalog rows with the bare repositories
// on the target disk:
//
//   - quarantine entries flip the catalog flag for rows whose directories
//     are confirmed lost (database-only mutation, one transaction per item);
//   - adopt entries register explicitly declared on-disk bare repositories,
//     after idempotently preparing hooks/description/export-ok files and
//     committing the catalog row in a single transaction;
//   - everything else (undeclared orphan directories, unconfirmed missing
//     rows, healthy repositories) is never modified and only reported.
//
// Each item is independent: a failed item never rolls back successful ones,
// and the whole procedure is safe to rerun because decisions derive solely
// from the current catalog and disk state. The resulting report is persisted
// (except for dry runs) and readable via GetLastReconcileReport.
func (d *Backend) ReconcileStorage(ctx context.Context, manifestPath string, dryRun bool) (*reconcile.Report, error) {
	started := time.Now()

	// Manifest load + validate happens before any state is touched.
	manifest, err := reconcile.LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	reposRoot := filepath.Join(d.cfg.DataPath, "repos")
	snapshot, err := reconcile.Scan(reposRoot)
	if err != nil {
		return nil, err
	}

	var rows []models.Repo
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		rows, err = d.store.GetAllRepos(ctx, tx)
		return err
	}); err != nil {
		return nil, db.WrapError(err)
	}

	catalog := make([]reconcile.CatalogRepo, 0, len(rows))
	for _, row := range rows {
		catalog = append(catalog, reconcile.CatalogRepo{
			Name:        row.Name,
			Quarantined: row.Quarantined,
		})
	}

	plan := reconcile.BuildPlan(manifest, snapshot, catalog)
	report := &reconcile.Report{
		ManifestPath:       manifestPath,
		DryRun:             dryRun,
		StartedAt:          started,
		Items:              make([]reconcile.ItemResult, 0, len(plan.Actions)),
		UndeclaredOrphans:  plan.UndeclaredOrphans,
		UnconfirmedMissing: plan.UnconfirmedMissing,
		Aligned:            plan.Aligned,
	}

	if dryRun {
		for _, action := range plan.Actions {
			report.Items = append(report.Items, reconcile.ItemResult{
				Type:   action.Type,
				Name:   action.Name,
				Result: action.Result,
				Reason: action.Reason,
			})
		}
		report.Status = reconcile.StatusSuccess
		report.FinishedAt = time.Now()
		return report, nil
	}

	for _, action := range plan.Actions {
		item := reconcile.ItemResult{
			Type:   action.Type,
			Name:   action.Name,
			Result: action.Result,
			Reason: action.Reason,
		}

		if action.Actionable() {
			switch action.Type {
			case reconcile.ActionQuarantine:
				item = d.executeQuarantine(ctx, action)
			case reconcile.ActionAdopt:
				item = d.executeAdopt(ctx, action)
			}
		}

		report.Items = append(report.Items, item)
	}

	report.Status = reconcile.FinalizeStatus(report.Items)
	report.FinishedAt = time.Now()

	data, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.SetReconcileReport(ctx, tx, string(data))
	}); err != nil {
		return report, db.WrapError(err)
	}

	return report, nil
}

// executeQuarantine flips one catalog row inside a single transaction. There
// are no disk side effects: the directory has already been confirmed absent
// by the plan.
func (d *Backend) executeQuarantine(ctx context.Context, action reconcile.PlannedAction) reconcile.ItemResult {
	item := reconcile.ItemResult{
		Type:   reconcile.ActionQuarantine,
		Name:   action.Name,
		Result: reconcile.ResultQuarantine,
	}

	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.SetRepoQuarantinedByName(ctx, tx, action.Name, true)
	})
	if err != nil {
		d.logger.Error("reconcile: failed to quarantine repository", "repo", action.Name, "err", err)
		item.Result = reconcile.ResultFailed
		item.Reason = db.WrapError(err).Error()
	}

	if action.Quarantine != nil && action.Quarantine.Reason != "" {
		d.logger.Info("reconcile: repository quarantined",
			"repo", action.Name, "reason", action.Quarantine.Reason)
	}

	return item
}

// executeAdopt registers one declared orphan bare repository. Disk
// preparation (hooks/description/export-ok) happens first and is idempotent,
// so a failed or interrupted database insert leaves only re-runnable files
// and an unregistered directory rather than a half-applied state.
func (d *Backend) executeAdopt(ctx context.Context, action reconcile.PlannedAction) reconcile.ItemResult {
	item := reconcile.ItemResult{
		Type:   reconcile.ActionAdopt,
		Name:   action.Name,
		Result: reconcile.ResultAdopt,
	}
	adopt := action.Adopt

	ownerID, err := d.resolveAdoptOwner(ctx, adopt)
	if err != nil {
		item.Result = reconcile.ResultFailed
		item.Reason = err.Error()
		return item
	}

	rp := action.Dir
	description := utils.Sanitize(adopt.Description)
	projectName := utils.Sanitize(adopt.ProjectName)

	// Idempotent disk preparation, before any catalog write.
	if err := os.WriteFile(filepath.Join(rp, "description"), []byte(description), fs.ModePerm); err != nil {
		d.logger.Error("reconcile: failed to write description", "repo", action.Name, "err", err)
		item.Result = reconcile.ResultFailed
		item.Reason = err.Error()
		return item
	}

	if !adopt.Private {
		if err := os.WriteFile(filepath.Join(rp, "git-daemon-export-ok"), []byte{}, fs.ModePerm); err != nil {
			d.logger.Error("reconcile: failed to write git-daemon-export-ok", "repo", action.Name, "err", err)
			item.Result = reconcile.ResultFailed
			item.Reason = err.Error()
			return item
		}
	}

	if err := hooks.GenerateHooks(ctx, d.cfg, action.Name); err != nil {
		d.logger.Error("reconcile: failed to generate hooks", "repo", action.Name, "err", err)
		item.Result = reconcile.ResultFailed
		item.Reason = err.Error()
		return item
	}

	// Single catalog transaction commits the adoption.
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.CreateRepo(ctx, tx, action.Name, ownerID,
			projectName, description, adopt.Private, adopt.Hidden, adopt.Mirror)
	}); err != nil {
		wrapped := db.WrapError(err)
		d.logger.Error("reconcile: failed to register repository", "repo", action.Name, "err", wrapped)
		item.Result = reconcile.ResultFailed
		item.Reason = wrapped.Error()
		return item
	}

	d.logger.Info("reconcile: orphan repository registered", "repo", action.Name, "path", rp)
	return item
}

// resolveAdoptOwner maps an optional manifest owner to a user ID, falling
// back to the lowest-ID administrator.
func (d *Backend) resolveAdoptOwner(ctx context.Context, adopt *reconcile.AdoptItem) (int64, error) {
	var ownerID int64
	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if adopt.Owner != "" {
			user, err := d.store.FindUserByUsername(ctx, tx, adopt.Owner)
			if err != nil {
				return err
			}
			ownerID = user.ID
			return nil
		}

		id, err := d.defaultAdminUserID(ctx, tx)
		if err != nil {
			return err
		}
		ownerID = id
		return nil
	})
	if err != nil {
		wrapped := db.WrapError(err)
		if errors.Is(wrapped, db.ErrRecordNotFound) {
			return 0, errors.New("adopt owner \"" + adopt.Owner + "\" does not exist")
		}
		return 0, wrapped
	}
	return ownerID, nil
}

// GetLastReconcileReport reads the most recently persisted reconciliation
// report. It returns (nil, nil) when no report has ever been stored.
func (d *Backend) GetLastReconcileReport(ctx context.Context) (*reconcile.Report, error) {
	var raw string
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		raw, err = d.store.GetReconcileReport(ctx, tx)
		return err
	}); err != nil {
		wrapped := db.WrapError(err)
		if errors.Is(wrapped, db.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, wrapped
	}

	var report reconcile.Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return nil, err
	}
	return &report, nil
}
