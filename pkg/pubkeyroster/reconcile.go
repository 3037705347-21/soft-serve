package pubkeyroster

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"golang.org/x/crypto/ssh"
)

// Action describes what the reconciliation does with a single key.
type Action string

const (
	// ActionAdd inserts a roster key the user did not have yet.
	ActionAdd Action = "add"
	// ActionAdopt marks an existing key of the user as roster-managed.
	ActionAdopt Action = "adopt"
	// ActionMove reassigns a roster-managed key from one user to another.
	ActionMove Action = "move"
	// ActionRevoke removes a roster-managed key absent from the new roster.
	ActionRevoke Action = "revoke"
)

// ErrUserNotFound is returned when the roster names a user that does not
// exist. No keys are changed in that case.
var ErrUserNotFound = errors.New("roster references unknown user")

// ErrManualKeyConflict is returned when the roster assigns a public key to a
// user but that very key is already attached to a different user outside of
// roster management (a manual key). The run aborts rather than hijacking or
// deleting the manual key.
var ErrManualKeyConflict = errors.New("roster key conflicts with a manual key of another user")

// Change is a single planned or applied key change.
type Change struct {
	// Action is the kind of change.
	Action Action

	// Username is the user the key belongs to after the change.
	Username string

	// FromUsername is the previous owner for ActionMove; empty otherwise.
	FromUsername string

	// Key is the affected public key.
	Key ssh.PublicKey
}

// Report is the outcome of a reconciliation.
type Report struct {
	// Changes are the key changes in a stable order: revocations first (by
	// key), then roster entries in their file order.
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

type publicKeyRow struct {
	ID        int64  `db:"id"`
	UserID    int64  `db:"user_id"`
	PublicKey string `db:"public_key"`
}

type ledgerRow struct {
	UserID    int64  `db:"user_id"`
	PublicKey string `db:"public_key"`
}

// Reconcile brings the roster-managed public keys in the database in line
// with the given roster.
//
// All database state checks and writes happen inside a single transaction:
// if the roster references an unknown user or claims a key that another user
// holds manually, the transaction is rolled back and nothing is modified.
//
// Re-running Reconcile with the same roster is a no-op: keys are matched by
// their canonical authorized-key form and unique constraints guard the
// tables, so a rerun after a crash cannot create duplicates or half-applied
// state.
func Reconcile(ctx context.Context, dbx *db.DB, roster *Roster, opts Options) (*Report, error) {
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

		var existing []publicKeyRow
		if err := tx.SelectContext(ctx, &existing, tx.Rebind(`SELECT id, user_id, public_key FROM public_keys;`)); err != nil {
			return fmt.Errorf("list public keys: %w", err)
		}
		existingByKey := make(map[string]publicKeyRow, len(existing))
		for _, row := range existing {
			existingByKey[row.PublicKey] = row
		}

		var managed []ledgerRow
		if err := tx.SelectContext(ctx, &managed, tx.Rebind(`SELECT user_id, public_key FROM roster_public_keys;`)); err != nil {
			return fmt.Errorf("list roster-managed public keys: %w", err)
		}
		managedByKey := make(map[string]int64, len(managed))
		for _, row := range managed {
			managedByKey[row.PublicKey] = row.UserID
		}

		// Validate the roster against the current database state before
		// planning anything: unknown users and manual-key conflicts abort
		// the whole run.
		desired := make(map[string]Entry, len(roster.entries))
		for canonical, entry := range roster.keyOwner {
			uid, ok := idForUser[entry.Username]
			if !ok {
				return fmt.Errorf("%w: %q (roster line %d)", ErrUserNotFound, entry.Username, entry.Line)
			}

			if row, exists := existingByKey[canonical]; exists && row.UserID != uid {
				if _, isManaged := managedByKey[canonical]; !isManaged {
					return fmt.Errorf("%w: key %s is attached to user %q but roster line %d assigns it to %q",
						ErrManualKeyConflict, ssh.FingerprintSHA256(entry.Key),
						nameForID[row.UserID], entry.Line, entry.Username)
				}
			}

			desired[canonical] = entry
		}

		// Plan revocations: roster-managed keys that the new roster no
		// longer claims. Manual keys are never in the ledger, so they are
		// inherently left alone.
		revokedKeys := make([]string, 0)
		for canonical := range managedByKey {
			if _, ok := desired[canonical]; !ok {
				revokedKeys = append(revokedKeys, canonical)
			}
		}
		sort.Strings(revokedKeys)
		for _, canonical := range revokedKeys {
			pk, _, err := sshutils.ParseAuthorizedKey(canonical)
			if err != nil {
				return fmt.Errorf("parse roster-managed key: %w", err)
			}
			report.Changes = append(report.Changes, Change{
				Action:   ActionRevoke,
				Username: nameForID[managedByKey[canonical]],
				Key:      pk,
			})
		}

		// Plan adds/adopts/moves following the roster file order.
		type planned struct {
			entry  Entry
			action Action
			from   string
		}
		plans := make([]planned, 0, len(roster.entries))
		for _, entry := range roster.entries {
			canonical := sshutils.MarshalAuthorizedKey(entry.Key)
			uid := idForUser[entry.Username]
			row, exists := existingByKey[canonical]

			switch {
			case !exists:
				plans = append(plans, planned{entry, ActionAdd, ""})
			case row.UserID == uid:
				if owner, isManaged := managedByKey[canonical]; isManaged && owner == uid {
					// Already roster-managed for the right user: no-op.
					continue
				}
				// Either a manual key of the same user or a ledger row
				// pointing at a stale owner: adopt it for this user.
				plans = append(plans, planned{entry, ActionAdopt, ""})
			default:
				// Present on another user; the validation pass above proved
				// it is roster-managed, so it is safe to reassign.
				plans = append(plans, planned{entry, ActionMove, nameForID[row.UserID]})
			}
		}

		for _, p := range plans {
			report.Changes = append(report.Changes, Change{
				Action:       p.action,
				Username:     p.entry.Username,
				FromUsername: p.from,
				Key:          p.entry.Key,
			})
		}

		if opts.DryRun {
			return nil
		}

		// Apply. Everything below runs in the same transaction; any error
		// rolls back every preceding statement as well.
		for _, canonical := range revokedKeys {
			if _, err := tx.ExecContext(ctx,
				tx.Rebind(`DELETE FROM roster_public_keys WHERE public_key = ?;`), canonical); err != nil {
				return fmt.Errorf("revoke roster key: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				tx.Rebind(`DELETE FROM public_keys WHERE public_key = ?;`), canonical); err != nil {
				return fmt.Errorf("revoke public key: %w", err)
			}
		}

		for _, p := range plans {
			canonical := sshutils.MarshalAuthorizedKey(p.entry.Key)
			uid := idForUser[p.entry.Username]

			switch p.action {
			case ActionMove:
				if _, err := tx.ExecContext(ctx,
					tx.Rebind(`DELETE FROM roster_public_keys WHERE public_key = ?;`), canonical); err != nil {
					return fmt.Errorf("move roster key: %w", err)
				}
				if _, err := tx.ExecContext(ctx,
					tx.Rebind(`DELETE FROM public_keys WHERE public_key = ?;`), canonical); err != nil {
					return fmt.Errorf("move public key: %w", err)
				}
				fallthrough
			case ActionAdd:
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`INSERT INTO public_keys (user_id, public_key, updated_at)
					 VALUES (?, ?, CURRENT_TIMESTAMP);`), uid, canonical); err != nil {
					return fmt.Errorf("add public key: %w", err)
				}
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`INSERT INTO roster_public_keys (user_id, public_key, updated_at)
					 VALUES (?, ?, CURRENT_TIMESTAMP);`), uid, canonical); err != nil {
					return fmt.Errorf("record roster key: %w", err)
				}
			case ActionAdopt:
				if _, err := tx.ExecContext(ctx,
					tx.Rebind(`DELETE FROM roster_public_keys WHERE public_key = ?;`), canonical); err != nil {
					return fmt.Errorf("adopt roster key: %w", err)
				}
				if _, err := tx.ExecContext(ctx, tx.Rebind(
					`INSERT INTO roster_public_keys (user_id, public_key, updated_at)
					 VALUES (?, ?, CURRENT_TIMESTAMP);`), uid, canonical); err != nil {
					return fmt.Errorf("record roster key: %w", err)
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
