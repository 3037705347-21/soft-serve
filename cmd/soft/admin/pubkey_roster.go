package admin

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/pubkeyroster"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

var reconcilePubkeysCmd = &cobra.Command{
	Use:   "reconcile-pubkeys ROSTER_FILE",
	Short: "Reconcile user SSH public keys against a roster file",
	Long: `Reconcile user SSH public keys against a roster file.

Each non-empty, non-comment roster line has the form:

    <username> <key-type> <key-base64> [comment]

Keys listed in the roster are added to their users, roster-managed keys that
moved users are reassigned, and roster-managed keys missing from the roster
are revoked. Public keys added manually and users absent from the roster are
never modified.

The roster is fully validated before any change is applied and all changes
are committed in a single transaction, so a malformed roster, a key assigned
to two users or a key already held manually by another user aborts the run
without touching anything. Re-running the same roster is a no-op.`,
	Args: cobra.ExactArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		ctx := c.Context()

		f, err := os.Open(args[0])
		if err != nil {
			return fmt.Errorf("open roster: %w", err)
		}
		defer f.Close() //nolint:errcheck

		roster, err := pubkeyroster.Parse(f)
		if err != nil {
			return err
		}

		dryRun, err := c.Flags().GetBool("dry-run")
		if err != nil {
			return err
		}

		report, err := pubkeyroster.Reconcile(ctx, db.FromContext(ctx), roster, pubkeyroster.Options{DryRun: dryRun})
		if err != nil {
			if errors.Is(err, pubkeyroster.ErrUserNotFound) || errors.Is(err, pubkeyroster.ErrManualKeyConflict) {
				return fmt.Errorf("roster reconciliation aborted, no changes made: %w", err)
			}
			return err
		}

		printRosterReport(c.OutOrStdout(), report, dryRun)
		return nil
	},
	PersistentPreRunE:  cmd.InitBackendContext,
	PersistentPostRunE: cmd.CloseDBContext,
}

func init() {
	reconcilePubkeysCmd.Flags().Bool("dry-run", false, "show the planned changes without applying them")
	Command.AddCommand(reconcilePubkeysCmd)
}

func printRosterReport(w io.Writer, report *pubkeyroster.Report, dryRun bool) {
	if len(report.Changes) == 0 {
		if dryRun {
			fmt.Fprintln(w, "dry-run: roster already in sync, no changes needed")
		} else {
			fmt.Fprintln(w, "roster already in sync, no changes made")
		}
		return
	}

	for _, change := range report.Changes {
		pk := change.Key
		detail := fmt.Sprintf("%s %s", pk.Type(), ssh.FingerprintSHA256(pk))
		switch change.Action {
		case pubkeyroster.ActionMove:
			fmt.Fprintf(w, "%-7s %s  %s (moved from %s)\n", change.Action, change.Username, detail, change.FromUsername)
		default:
			fmt.Fprintf(w, "%-7s %s  %s\n", change.Action, change.Username, detail)
		}
	}

	fmt.Fprintf(w, "summary: added %d, adopted %d, moved %d, revoked %d\n",
		report.Count(pubkeyroster.ActionAdd),
		report.Count(pubkeyroster.ActionAdopt),
		report.Count(pubkeyroster.ActionMove),
		report.Count(pubkeyroster.ActionRevoke))
	if dryRun {
		fmt.Fprintln(w, "dry-run: no changes were written")
	}
}
