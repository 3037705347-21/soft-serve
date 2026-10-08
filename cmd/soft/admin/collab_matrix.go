package admin

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/collabmatrix"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/spf13/cobra"
)

var reconcileCollabsCmd = &cobra.Command{
	Use:   "reconcile-collabs MATRIX_FILE",
	Short: "Reconcile repository collaborators against an access matrix file",
	Long: `Reconcile repository collaborators against an access matrix file.

Each non-empty, non-comment matrix line has the form:

    <repository> <username> <access-level>

where access-level is one of: no-access, read-only, read-write, admin-access.

Users listed in the matrix are granted the declared level, levels of already
matrix-managed collaborators are changed, and matrix-managed collaborators
missing from the matrix are removed. Collaborators added manually, repository
owners and repositories absent from the matrix are never modified.

The matrix is fully validated before any change is applied and all changes
are committed in a single transaction, so a malformed line, a duplicate
declaration, an unknown user or repository, a listed repository owner, or a
level clash with a manual grant aborts the run without touching anything.
Re-running the same matrix is a no-op.`,
	Args: cobra.ExactArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		ctx := c.Context()

		f, err := os.Open(args[0])
		if err != nil {
			return fmt.Errorf("open access matrix: %w", err)
		}
		defer f.Close() //nolint:errcheck

		matrix, err := collabmatrix.Parse(f)
		if err != nil {
			return err
		}

		dryRun, err := c.Flags().GetBool("dry-run")
		if err != nil {
			return err
		}

		report, err := collabmatrix.Reconcile(ctx, db.FromContext(ctx), matrix, collabmatrix.Options{DryRun: dryRun})
		if err != nil {
			if errors.Is(err, collabmatrix.ErrUserNotFound) ||
				errors.Is(err, collabmatrix.ErrRepoNotFound) ||
				errors.Is(err, collabmatrix.ErrRepoOwnerConflict) ||
				errors.Is(err, collabmatrix.ErrManualAccessConflict) {
				return fmt.Errorf("collaborator reconciliation aborted, no changes made: %w", err)
			}
			return err
		}

		printMatrixReport(c.OutOrStdout(), report, dryRun)
		return nil
	},
	PersistentPreRunE:  cmd.InitBackendContext,
	PersistentPostRunE: cmd.CloseDBContext,
}

func init() {
	reconcileCollabsCmd.Flags().Bool("dry-run", false, "show the planned changes without applying them")
	Command.AddCommand(reconcileCollabsCmd)
}

func printMatrixReport(w io.Writer, report *collabmatrix.Report, dryRun bool) {
	if len(report.Changes) == 0 {
		if dryRun {
			fmt.Fprintln(w, "dry-run: access matrix already in sync, no changes needed")
		} else {
			fmt.Fprintln(w, "access matrix already in sync, no changes made")
		}
		return
	}

	for _, change := range report.Changes {
		switch change.Action {
		case collabmatrix.ActionUpdate:
			fmt.Fprintf(w, "%-7s %s  %s  %s -> %s\n",
				change.Action, change.Repo, change.Username, change.FromAccessLevel, change.AccessLevel)
		default:
			fmt.Fprintf(w, "%-7s %s  %s  %s\n",
				change.Action, change.Repo, change.Username, change.AccessLevel)
		}
	}

	fmt.Fprintf(w, "summary: granted %d, adopted %d, updated %d, revoked %d\n",
		report.Count(collabmatrix.ActionGrant),
		report.Count(collabmatrix.ActionAdopt),
		report.Count(collabmatrix.ActionUpdate),
		report.Count(collabmatrix.ActionRevoke))
	if dryRun {
		fmt.Fprintln(w, "dry-run: no changes were written")
	}
}
