package backend

import (
	"context"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/reconcile"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// UnquarantineRepository clears the quarantine flag on a repository. It is
// deliberately conservative: the repository must currently be quarantined
// and its on-disk bare repository must be present and valid, otherwise the
// flag is left untouched and an error is returned. This is the only path
// that restores a quarantined repository to service.
//
// It is invoked by the `soft admin unquarantine` command.
func (d *Backend) UnquarantineRepository(ctx context.Context, name string) error {
	name = utils.SanitizeRepo(name)
	rp := d.repoPath(name)

	return db.WrapError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		m, err := d.store.GetRepoByName(ctx, tx, name)
		if err != nil {
			return err
		}

		if !m.Quarantined {
			return fmt.Errorf("repository %q is not quarantined", name)
		}

		if !reconcile.IsBareRepo(rp) {
			return fmt.Errorf("cannot unquarantine %q: no valid bare repository found at %s", name, rp)
		}

		return d.store.SetRepoQuarantinedByName(ctx, tx, name, false)
	}))
}
