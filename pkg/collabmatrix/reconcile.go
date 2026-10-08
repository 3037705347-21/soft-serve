package collabmatrix

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// Action describes what the reconciliation does with a single collaborator.
type Action string

const (
	// ActionGrant inserts a collaborator the repository did not have yet.
	ActionGrant Action = "grant"
	// ActionAdopt marks an existing manual collaborator as matrix-managed.
	ActionAdopt Action = "adopt"
	// ActionUpdate changes the access level of a matrix-managed collaborator.
	ActionUpdate Action = "update"
	// ActionRevoke removes a matrix-managed collaborator absent from the new matrix.
	ActionRevoke Action = "revoke"
)

// ErrUserNotFound is returned when the matrix names a user that does not
// exist. No collaborators are changed in that case.
var ErrUserNotFound = errors.New("matrix references unknown user")

// ErrRepoNotFound is returned when the matrix names a repository that does
// not exist. No collaborators are changed in that case.
var ErrRepoNotFound = errors.New("matrix references unknown repository")

// ErrRepoOwnerConflict is returned when the matrix declares the repository
// owner as a collaborator. Owners already hold full access through ownership
// and the reconciliation never manages ownership, so the run aborts rather
// than creating a redundant row it could later revoke.
var ErrRepoOwnerConflict = errors.New("matrix lists the repository owner as a collaborator")

// ErrManualAccessConflict is returned when the matrix declares a user at a
// different access level than the level that user already holds through a
// manual grant (a collabs row absent from the matrix ledger). The run aborts
// rather than overwriting the manual grant; operators must either align the
// matrix or remove the manual grant outside of this process.
var ErrManualAccessConflict = errors.New("matrix conflicts with a manual grant")

// Change is a single planned or applied collaborator change.
type Change struct {
	// Action is the kind of change.
	Action Action

	// Repo is the affected repository.
	Repo string

	// Username is the affected user.
	Username string

	// AccessLevel is the resulting access level.
	AccessLevel access.AccessLevel

	// FromAccessLevel is the previous level for ActionUpdate; NoAccess
	// (its zero value) otherwise.
	FromAccessLevel access.AccessLevel
}

// Report is the outcome of a reconciliation.
type Report struct {
	// Changes are the collaborator changes in a stable order: revocations
	// first (sorted by repository then user), then matrix entries in their
	// file order.
	Changes []Change
}

// Count returns the number of changes of the given action.
func (r *Report) Count(a Action) int {
	n := 0
	for _, c := range r.Changes {
		if c.Action == a {
			n++
		}
	}
	return n
}

// Options controls a reconciliation run.
type Options struct {
	// DryRun computes and returns the report without writing anything.
	DryRun bool
}

type userRow struct {
	ID       int64  `db:"id"`
	Username string `db:"username"`
}

type repoRow struct {
	ID     int64  `db:"id"`
	Name   string `db:"name"`
	UserID int64  `db:"user_id"`
}

type collabRow struct {
	ID          int64              `db:"id"`
	RepoID      int64              `db:"repo_id"`
	UserID      int64              `db:"user_id"`
	AccessLevel access.AccessLevel `db:"access_level"`
}

type matrixKey struct {
	repoID int64
	userID int64
}

// Reconcile brings the matrix-managed collaborators in the database in line
// with the given access matrix.
//
// All database state checks and writes happen inside a single transaction:
// if the matrix references an unknown user or repository, lists a
// repository owner, or clashes with a manual grant at another level, the
// transaction is rolled back and nothing is modified.
//
// Re-running Reconcile with the same matrix is a no-op: collaborators are
// matched by (repository, user) with unique constraints guarding the
// tables, so a rerun after a crash cannot create duplicates or leave
// half-applied state.
func Reconcile(ctx context.Context, dbx *db.DB, matrix *Matrix, opts Options) (*Report, error) {
	var report Report

	err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		var users []userRow
		if err := tx.SelectContext(ctx, &users, tx.Rebind(`SELECT id, username FROM users;`)); err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		idForUser := make(map[string]int64, len(users))
		nameForID := make(map[int64]string, len(users))
		for _, u := range users {
			idForUser[u.Username] = u.ID
			nameForID[u.ID] = u.Username
		}

		var repos []repoRow
		if err := tx.SelectContext(ctx, &repos, tx.Rebind(`SELECT id, name, user_id FROM repos;`)); err != nil {
			return fmt.Errorf("list repositories: %w", err)
		}
		idForRepo := make(map[string]int64, len(repos))
		nameForIDRepo := make(map[int64]string, len(repos))
		ownerForRepo := make(map[int64]int64, len(repos))
		for _, r := range repos {
			idForRepo[r.Name] = r.ID
			nameForIDRepo[r.ID] = r.Name
			ownerForRepo[r.ID] = r.UserID
		}

		var existing []collabRow
		if err := tx.SelectContext(ctx, &existing, tx.Rebind(`SELECT id, repo_id, user_id, access_level FROM collabs;`)); err != nil {
			return fmt.Errorf("list collaborators: %w", err)
		}
		collabs := make(map[matrixKey]collabRow, len(existing))
		for _, row := range existing {
			collabs[matrixKey{row.RepoID, row.UserID}] = row
		}

		var managed []collabRow
		if err := tx.SelectContext(ctx, &managed, tx.Rebind(`SELECT 0 AS id, repo_id, user_id, access_level FROM roster_collabs;`)); err != nil {
			return fmt.Errorf("list matrix-managed collaborators: %w", err)
		}
		ledger := make(map[matrixKey]collabRow, len(managed))
		for _, row := range managed {
			ledger[matrixKey{row.RepoID, row.UserID}] = row
		}

		// Resolve and validate every matrix entry against the current
		// database state before planning anything: unknown users/repos,
		// repository owners and manual-grant clashes abort the whole run.
		type desired struct {
			entry Entry
			key   matrixKey
		}
		desiredByKey := make(map[matrixKey]desired, len(matrix.entries))
		for _, entry := range matrix.entries {
			uid, ok := idForUser[entry.Username]
			if !ok {
				return fmt.Errorf("%w: %q (matrix line %d)", ErrUserNotFound, entry.Username, entry.Line)
			}

			rid, ok := idForRepo[entry.Repo]
			if !ok {
				return fmt.Errorf("%w: %q (matrix line %d)", ErrRepoNotFound, entry.Repo, entry.Line)
			}

			if ownerForRepo[rid] == uid {
				return fmt.Errorf("%w: repository %q is owned by %q (matrix line %d)",
					ErrRepoOwnerConflict, entry.Repo, entry.Username, entry.Line)
			}

			key := matrixKey{rid, uid}
			if c, exists := collabs[key]; exists {
				if _, isManaged := ledger[key]; !isManaged && c.AccessLevel != entry.AccessLevel {
					return fmt.Errorf("%w: user %q already has %s on repository %q but matrix line %d declares %s",
						ErrManualAccessConflict, entry.Username, c.AccessLevel, entry.Repo, entry.Line, entry.AccessLevel)
				}
			}

			desiredByKey[key] = desired{entry, key}
		}

		// Plan revocations: ledger rows the new matrix no longer claims.
		// Manual grants are never in the ledger, so they are inherently
		// left alone.
		revoked := make([]matrixKey, 0)
		for key := range ledger {
			if _, ok := desiredByKey[key]; !ok {
				revoked = append(revoked, key)
			}
		}
		sort.Slice(revoked, func(i, j int) bool {
			ni, nj := nameForIDRepo[revoked[i].repoID], nameForIDRepo[revoked[j].repoID]
			if ni != nj {
				return ni < nj
			}
			return nameForID[revoked[i].userID] < nameForID[revoked[j].userID]
		})
		for _, key := range revoked {
			report.Changes = append(report.Changes, Change{
				Action:      ActionRevoke,
				Repo:        nameForIDRepo[key.repoID],
				Username:    nameForID[key.userID],
				AccessLevel: ledger[key].AccessLevel,
			})
		}

		// Plan grants/adopts/updates following the matrix file order.
		type planned struct {
			change Change
			action Action
			// repairLedger marks a grant whose ledger row survived but
			// whose collabs row was removed outside this process.
			repairLedger bool
		}
		plans := make([]planned, 0, len(matrix.entries))
		for _, entry := range matrix.entries {
			key := matrixKey{idForRepo[entry.Repo], idForUser[entry.Username]}
			c, hasCollab := collabs[key]
			_, hasLedger := ledger[key]

			switch {
			case !hasCollab:
				// No effective grant. Whether or not a ledger row is left
				// behind, re-insert the collabs row (and repair the ledger
				// level if it already existed).
				plans = append(plans, planned{
					change: Change{
						Action:      ActionGrant,
						Repo:        entry.Repo,
						Username:    entry.Username,
						AccessLevel: entry.AccessLevel,
					},
					action:       ActionGrant,
					repairLedger: hasLedger,
				})
			case !hasLedger:
				// Validation above guaranteed the manual grant's level
				// matches the declared one, so adopting it just records the
				// ledger row.
				plans = append(plans, planned{
					change: Change{
						Action:      ActionAdopt,
						Repo:        entry.Repo,
						Username:    entry.Username,
						AccessLevel: entry.AccessLevel,
					},
					action: ActionAdopt,
				})
			case c.AccessLevel == entry.AccessLevel:
				// Already managed at the right level: no-op.
				continue
			default:
				plans = append(plans, planned{
					change: Change{
						Action:          ActionUpdate,
						Repo:            entry.Repo,
						Username:        entry.Username,
						AccessLevel:     entry.AccessLevel,
						FromAccessLevel: c.AccessLevel,
					},
					action: ActionUpdate,
				})
			}
		}

		for _, p := range plans {
			report.Changes = append(report.Changes, p.change)
		}

		if opts.DryRun {
			return nil
		}

		// Apply. Everything below runs in the same transaction; any error
		// rolls back every preceding statement as well.
		for _, key := range revoked {
			if _, err := tx.ExecContext(ctx,
				tx.Rebind(`DELETE FROM roster_collabs WHERE repo_id = ? AND user_id = ?;`), key.repoID, key.userID); err != nil {
				return fmt.Errorf("revoke managed collaborator: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				tx.Rebind(`DELETE FROM collabs WHERE repo_id = ? AND user_id = ?;`), key.repoID, key.userID); err != nil {
				return fmt.Errorf("revoke collaborator: %w", err)
			}
		}

		for _, p := range plans {
			key := matrixKey{idForRepo[p.change.Repo], idForUser[p.change.Username]}

			switch p.action {
			case ActionGrant:
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`INSERT INTO collabs (repo_id, user_id, access_level, updated_at)
					 VALUES (?, ?, ?, CURRENT_TIMESTAMP);`),
					key.repoID, key.userID, p.change.AccessLevel); err != nil {
					return fmt.Errorf("grant collaborator: %w", err)
				}
				if p.repairLedger {
					if _, err := tx.ExecContext(ctx, tx.Rebind(
						`UPDATE roster_collabs SET access_level = ?, updated_at = CURRENT_TIMESTAMP
						 WHERE repo_id = ? AND user_id = ?;`),
						p.change.AccessLevel, key.repoID, key.userID); err != nil {
						return fmt.Errorf("repair managed collaborator: %w", err)
					}
				} else {
					if _, err := tx.ExecContext(ctx, tx.Rebind(
						`INSERT INTO roster_collabs (repo_id, user_id, access_level, updated_at)
						 VALUES (?, ?, ?, CURRENT_TIMESTAMP);`),
						key.repoID, key.userID, p.change.AccessLevel); err != nil {
						return fmt.Errorf("record managed collaborator: %w", err)
					}
				}
			case ActionAdopt:
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`INSERT INTO roster_collabs (repo_id, user_id, access_level, updated_at)
					 VALUES (?, ?, ?, CURRENT_TIMESTAMP);`),
					key.repoID, key.userID, p.change.AccessLevel); err != nil {
					return fmt.Errorf("record adopted collaborator: %w", err)
				}
			case ActionUpdate:
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`UPDATE collabs SET access_level = ?, updated_at = CURRENT_TIMESTAMP
					 WHERE repo_id = ? AND user_id = ?;`),
					p.change.AccessLevel, key.repoID, key.userID); err != nil {
					return fmt.Errorf("update collaborator: %w", err)
				}
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`UPDATE roster_collabs SET access_level = ?, updated_at = CURRENT_TIMESTAMP
					 WHERE repo_id = ? AND user_id = ?;`),
					p.change.AccessLevel, key.repoID, key.userID); err != nil {
					return fmt.Errorf("update managed collaborator: %w", err)
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &report, nil
}
