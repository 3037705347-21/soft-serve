package admin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
	"github.com/spf13/pflag"
)

// adminTestContext builds a fully wired backend context backed by a temp
// data directory, without invoking the cobra InitBackendContext hooks.
func adminTestContext(t *testing.T) (context.Context, *backend.Backend) {
	t.Helper()

	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = filepath.Join(dp, "test.db")

	ctx := config.WithContext(context.Background(), cfg)
	logger := log.NewWithOptions(&bytes.Buffer{}, log.Options{})
	ctx = log.WithContext(ctx, logger)

	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}

	dbstore := database.New(ctx, dbx)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)
	return ctx, be
}

// execCommand runs a registered child command through the admin root with
// the child's pre/post run hooks temporarily stripped (the context already
// carries a backend), and returns the combined output.
func execCommand(t *testing.T, ctx context.Context, name string, args ...string) (string, error) {
	t.Helper()

	src, _, err := Command.Find([]string{name})
	if err != nil {
		t.Fatal(err)
	}

	savedPre := src.PersistentPreRunE
	savedPost := src.PersistentPostRunE
	src.PersistentPreRunE = nil
	src.PersistentPostRunE = nil
	t.Cleanup(func() {
		src.PersistentPreRunE = savedPre
		src.PersistentPostRunE = savedPost
	})

	// Reset every flag to its default so repeated executions in the same
	// process cannot inherit values parsed by a previous test.
	src.Flags().VisitAll(func(f *pflag.Flag) {
		if err := f.Value.Set(f.DefValue); err != nil {
			t.Fatal(err)
		}
	})

	buf := &bytes.Buffer{}
	Command.SetOut(buf)
	Command.SetErr(buf)
	src.SetOut(buf)
	src.SetErr(buf)
	// cobra only propagates the root context into a child on its first
	// execution, so set the child context explicitly on every run.
	Command.SetContext(ctx)
	src.SetContext(ctx)
	Command.SetArgs(append([]string{name}, args...))

	err = Command.Execute()
	return buf.String(), err
}

func writeTestManifest(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdminCommandsRegistered(t *testing.T) {
	is := is.New(t)

	for _, name := range []string{"storage-reconcile", "reconcile-status", "unquarantine"} {
		cmd, _, err := Command.Find([]string{name})
		is.NoErr(err)
		is.Equal(cmd.Name(), name)
	}
}

func TestStorageReconcileRequiresManifest(t *testing.T) {
	is := is.New(t)
	ctx, _ := adminTestContext(t)

	_, err := execCommand(t, ctx, "storage-reconcile")
	is.True(err != nil)
}

func TestStorageReconcileDryRun(t *testing.T) {
	is := is.New(t)
	ctx, _ := adminTestContext(t)
	is.NoErr(os.MkdirAll(filepath.Join(config.FromContext(ctx).DataPath, "repos"), 0o755))
	manifest := writeTestManifest(t, "version: 1\n")

	out, err := execCommand(t, ctx, "storage-reconcile", "--manifest", manifest, "--dry-run")
	is.NoErr(err)
	is.True(strings.Contains(out, "DRY RUN"))
	is.True(strings.Contains(out, "Summary:"))
}

func TestStorageReconcileSuccessAndFailure(t *testing.T) {
	is := is.New(t)
	ctx, be := adminTestContext(t)
	cfg := config.FromContext(ctx)
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	// A real successful quarantine.
	_, err := be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))

	okManifest := writeTestManifest(t, "version: 1\nquarantine:\n  - name: gone\n")
	out, err := execCommand(t, ctx, "storage-reconcile", "--manifest", okManifest)
	is.NoErr(err)
	is.True(strings.Contains(out, "quarantined=1"))

	// A failing run (unknown catalog name) exits non-zero but still prints.
	badManifest := writeTestManifest(t, "version: 1\nquarantine:\n  - name: ghost\n")
	out, err = execCommand(t, ctx, "storage-reconcile", "--manifest", badManifest)
	is.True(err != nil)
	is.True(strings.Contains(out, "conflict"))
	is.True(strings.Contains(out, "require attention"))
}

func TestReconcileStatusCommands(t *testing.T) {
	is := is.New(t)
	ctx, be := adminTestContext(t)
	cfg := config.FromContext(ctx)

	// Empty state.
	out, err := execCommand(t, ctx, "reconcile-status")
	is.NoErr(err)
	is.True(strings.Contains(out, "No storage reconciliation"))

	// Persist a report through a real run.
	reposRoot := filepath.Join(cfg.DataPath, "repos")
	_, err = be.CreateRepository(ctx, "gone", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "gone.git")))
	manifest := writeTestManifest(t, "version: 1\nquarantine:\n  - name: gone\n")
	_, err = execCommand(t, ctx, "storage-reconcile", "--manifest", manifest)
	is.NoErr(err)

	out, err = execCommand(t, ctx, "reconcile-status")
	is.NoErr(err)
	is.True(strings.Contains(out, manifest))
	is.True(strings.Contains(out, "Status:"))
}

func TestUnquarantineCommand(t *testing.T) {
	is := is.New(t)
	ctx, be := adminTestContext(t)
	cfg := config.FromContext(ctx)
	reposRoot := filepath.Join(cfg.DataPath, "repos")

	_, err := be.CreateRepository(ctx, "q", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(filepath.Join(reposRoot, "q.git")))

	// Quarantine through the reconciliation command itself.
	manifest := writeTestManifest(t, "version: 1\nquarantine:\n  - name: q\n")
	_, err = execCommand(t, ctx, "storage-reconcile", "--manifest", manifest)
	is.NoErr(err)

	// Missing disk: unquarantine is refused.
	_, err = execCommand(t, ctx, "unquarantine", "q")
	is.True(err != nil)

	// Disk restored: succeeds.
	_, err = git.Init(filepath.Join(reposRoot, "q"), true)
	is.NoErr(err)
	out, err := execCommand(t, ctx, "unquarantine", "q")
	is.NoErr(err)
	is.True(strings.Contains(out, "restored from quarantine"))
}
