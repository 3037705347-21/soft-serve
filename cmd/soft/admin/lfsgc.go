package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfsgc"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// defaultLFSGCGrace matches the default in pkg/lfsgc.
const defaultLFSGCGrace = 7 * 24 * time.Hour

var (
	lfsGCRepos    []string
	lfsGCAll      bool
	lfsGCDryRun   bool
	lfsGCGrace    time.Duration
	lfsGCNoVerify bool
	lfsGCReport   string

	lfsGCCmd = &cobra.Command{
		Use:   "lfs-gc",
		Short: "Reconcile and prune unreachable Git LFS objects",
		Long: `Reconcile LFS object storage against the pointers reachable from every
branch and tag of the selected repositories.

Only objects that are registered in the database, no longer referenced by any
branch or tag, not protected by an LFS lock, and older than the grace period are
deleted. Files copied into the object store without a database registration are
left untouched. Scanning or checksum verification failures abort the affected
repository before anything is deleted. A JSON report is written for every run
under <data-path>/lfs-gc.

Deletions are performed file-first and registration-second, so an interrupted
run converges when the command is re-run.`,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE:               runLFSGC,
	}
)

func init() {
	lfsGCCmd.Flags().StringSliceVar(&lfsGCRepos, "repo", nil, "repository to collect garbage from (can be repeated)")
	lfsGCCmd.Flags().BoolVar(&lfsGCAll, "all", false, "collect garbage from every repository")
	lfsGCCmd.Flags().BoolVarP(&lfsGCDryRun, "dry-run", "n", false, "report what would be deleted without deleting")
	lfsGCCmd.Flags().DurationVar(&lfsGCGrace, "grace", defaultLFSGCGrace, "minimum age of unreferenced objects before deletion (e.g. 168h, 30m); use a negative duration to disable")
	lfsGCCmd.Flags().BoolVar(&lfsGCNoVerify, "no-verify", false, "skip SHA-256 verification of deletion candidates")
	lfsGCCmd.Flags().StringVar(&lfsGCReport, "report-dir", "", "directory for the JSON report (default <data-path>/lfs-gc)")
	Command.AddCommand(lfsGCCmd)
}

func runLFSGC(c *cobra.Command, _ []string) error {
	ctx := c.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	dbx := db.FromContext(ctx)
	lstore := store.FromContext(ctx)

	if !lfsGCAll && len(lfsGCRepos) == 0 {
		return fmt.Errorf("specify at least one --repo name or --all to collect garbage from every repository")
	}

	repos := make([]lfsgc.Repo, 0)
	if lfsGCAll {
		all, err := be.Repositories(ctx)
		if err != nil {
			return fmt.Errorf("list repositories: %w", err)
		}
		for _, r := range all {
			repos = append(repos, r)
		}
	} else {
		for _, name := range lfsGCRepos {
			r, err := be.Repository(ctx, strings.TrimSpace(name))
			if err != nil {
				return fmt.Errorf("repository %q: %w", name, err)
			}
			repos = append(repos, r)
		}
	}

	opts := lfsgc.Options{
		DryRun:    lfsGCDryRun,
		Grace:     lfsGCGrace,
		NoVerify:  lfsGCNoVerify,
		ReportDir: lfsGCReport,
	}
	report, err := lfsgc.Run(ctx, repos, dbx, lstore, cfg.DataPath, opts)
	if err != nil {
		return err
	}

	if lfsGCDryRun {
		fmt.Fprintln(c.OutOrStdout(), "Dry run: no objects were deleted.")
	}
	for _, rr := range report.Repos {
		fmt.Fprintf(c.OutOrStdout(), "%-12s %-32s refs=%d deleted=%d reclaimed=%s registrations_removed=%d\n",
			rr.Status, rr.RepoName, len(rr.Refs), len(rr.Deleted),
			humanize.Bytes(uint64(rr.BytesReclaimed)), rr.RegistrationsRemovedN)
		for _, d := range rr.Discrepancies {
			fmt.Fprintf(c.ErrOrStderr(), "  ! %s\n", d)
		}
	}
	fmt.Fprintf(c.OutOrStdout(), "run %s: %s, %d object(s), %s reclaimed\n",
		report.RunID[:8], report.Status, report.ObjectsDeleted,
		humanize.Bytes(uint64(report.BytesReclaimed)))
	if report.ReportPath != "" {
		fmt.Fprintf(c.OutOrStdout(), "report: %s\n", report.ReportPath)
	}

	if report.Status == lfsgc.StatusAborted {
		return fmt.Errorf("garbage collection aborted for one or more repositories; see the report for details")
	}
	return nil
}
