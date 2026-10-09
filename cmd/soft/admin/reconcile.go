package admin

import (
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/reconcile"
	"github.com/spf13/cobra"
)

var (
	storageReconcileCmd = &cobra.Command{
		Use:   "storage-reconcile",
		Short: "Align cataloged repositories with disk according to a migration manifest",
		Long: `Reconciles repository catalog rows with the bare repositories on disk using an explicit migration manifest.

The manifest YAML has two sections:
  quarantine: registered repositories whose on-disk directories are confirmed lost
  adopt:      bare repository directories on the target disk to (re)register

Directories and catalog rows not declared in the manifest are never modified;
they are reported instead. The command is safe to rerun and converges.`,
		Args:               cobra.NoArgs,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE:               runStorageReconcile,
	}

	reconcileStatusCmd = &cobra.Command{
		Use:                "reconcile-status",
		Short:              "Show the latest persisted storage reconciliation report",
		Args:               cobra.NoArgs,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			be := backend.FromContext(ctx)

			report, err := be.GetLastReconcileReport(ctx)
			if err != nil {
				return fmt.Errorf("read reconciliation status: %w", err)
			}

			if report == nil {
				fmt.Fprintln(c.OutOrStdout(), "No storage reconciliation has been run yet.")
				return nil
			}

			printReport(c.OutOrStdout(), report)
			return nil
		},
	}

	unquarantineCmd = &cobra.Command{
		Use:                "unquarantine [repo]",
		Short:              "Restore a quarantined repository whose bare directory is back on disk",
		Args:               cobra.ExactArgs(1),
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			be := backend.FromContext(ctx)

			if err := be.UnquarantineRepository(ctx, args[0]); err != nil {
				return fmt.Errorf("unquarantine: %w", err)
			}

			fmt.Fprintf(c.OutOrStdout(), "Repository %q restored from quarantine.\n", args[0])
			return nil
		},
	}
)

func init() {
	storageReconcileCmd.Flags().String("manifest", "", "path to the migration manifest YAML file")
	storageReconcileCmd.Flags().Bool("dry-run", false, "print the reconciliation plan without changing the catalog or disk")
	_ = storageReconcileCmd.MarkFlagRequired("manifest")

	Command.AddCommand(
		storageReconcileCmd,
		reconcileStatusCmd,
		unquarantineCmd,
	)
}

func runStorageReconcile(c *cobra.Command, _ []string) error {
	ctx := c.Context()
	be := backend.FromContext(ctx)

	manifestPath, err := c.Flags().GetString("manifest")
	if err != nil {
		return err
	}
	dryRun, err := c.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	report, err := be.ReconcileStorage(ctx, manifestPath, dryRun)
	if err != nil {
		return fmt.Errorf("storage reconciliation: %w", err)
	}

	printReport(c.OutOrStdout(), report)

	if report.HasFailures() {
		return fmt.Errorf("storage reconciliation finished with status %s: %d item(s) require attention",
			report.Status, failureCount(report))
	}

	return nil
}

func failureCount(r *reconcile.Report) int {
	var n int
	for _, item := range r.Items {
		if item.Result == reconcile.ResultConflict || item.Result == reconcile.ResultFailed {
			n++
		}
	}
	return n
}

// printReport renders the structured report as stable, human-readable text.
func printReport(w io.Writer, r *reconcile.Report) {
	fmt.Fprintln(w, "Storage Reconciliation Report")
	fmt.Fprintf(w, "  Manifest: %s\n", r.ManifestPath)
	fmt.Fprintf(w, "  Started:  %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  Finished: %s\n", r.FinishedAt.Format(time.RFC3339))
	status := string(r.Status)
	if r.DryRun {
		status += " (DRY RUN - no changes were made)"
	}
	fmt.Fprintf(w, "  Status:   %s\n", status)
	fmt.Fprintln(w)

	if len(r.Items) > 0 {
		fmt.Fprintln(w, "Actions:")
		for _, item := range r.Items {
			fmt.Fprintf(w, "  [%-10s] %-30s %s\n", item.Type, item.Name, item.Result)
			if item.Reason != "" {
				fmt.Fprintf(w, "               reason: %s\n", item.Reason)
			}
		}
		fmt.Fprintln(w)
	}

	if len(r.UndeclaredOrphans) > 0 {
		fmt.Fprintln(w, "Undeclared orphan directories (left untouched):")
		for _, name := range r.UndeclaredOrphans {
			fmt.Fprintf(w, "  - %s\n", name)
		}
		fmt.Fprintln(w)
	}

	if len(r.UnconfirmedMissing) > 0 {
		fmt.Fprintln(w, "Cataloged repositories missing on disk (not declared for quarantine):")
		for _, name := range r.UnconfirmedMissing {
			fmt.Fprintf(w, "  - %s\n", name)
		}
		fmt.Fprintln(w)
	}

	var quarantined, adopted, already, failed int
	for _, item := range r.Items {
		switch item.Result {
		case reconcile.ResultQuarantine:
			quarantined++
		case reconcile.ResultAdopt:
			adopted++
		case reconcile.ResultAlreadyQuarantined, reconcile.ResultAlreadyRegistered:
			already++
		case reconcile.ResultConflict, reconcile.ResultFailed:
			failed++
		}
	}

	fmt.Fprintf(w,
		"Summary: quarantined=%d adopted=%d already-in-place=%d failed=%d undeclared-orphans=%d unconfirmed-missing=%d aligned=%d\n",
		quarantined, adopted, already, failed,
		len(r.UndeclaredOrphans), len(r.UnconfirmedMissing), r.Aligned)
}
