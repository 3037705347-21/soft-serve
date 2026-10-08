package lfsgc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	logr "github.com/charmbracelet/soft-serve/pkg/log"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

// TestRunWithBackendRepository exercises the full production wiring: a
// repository created through the backend (a bare repository on disk, returned
// as proto.Repository) is reconciled exactly like the lightweight test
// repositories.
func TestRunWithBackendRepository(t *testing.T) {
	tmp := t.TempDir()
	dataPath := filepath.Join(tmp, "data")
	if err := os.MkdirAll(dataPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	logger, _, err := logr.NewLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withConfigAndLogger(context.Background(), cfg, logger)

	dbx, err := db.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrateDB(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	st := database.New(ctx, dbx)
	be := backend.New(ctx, cfg, dbx, st)

	// Migrations seed a default admin user.
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	r, err := be.CreateRepository(ctx, "proj", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}

	// Push history into the bare repository: A stays reachable, B is dropped
	// by a forced history rewrite, F is reachable only via an annotated tag.
	work := filepath.Join(tmp, "work")
	runGit(t, tmp, "init", "-b", "main", work)
	runGit(t, work, "remote", "add", "origin", filepath.Join(dataPath, "repos", "proj.git"))

	aContent := deterministicBytes(1, 2048)
	a := shaOf(aContent)
	commitPointer(t, work, "a.bin", a, 2048, "add a")
	runGit(t, work, "push", "-q", "origin", "main")

	bContent := deterministicBytes(2, 4096)
	b := shaOf(bContent)
	commitPointer(t, work, "b.bin", b, 4096, "add b")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, work, "reset", "--hard", "HEAD~1")
	runGit(t, work, "push", "--force", "-q", "origin", "main")

	fContent := deterministicBytes(3, 1024)
	f := shaOf(fContent)
	runGit(t, work, "checkout", "-q", "-b", "keep")
	commitPointer(t, work, "f.bin", f, 1024, "add f")
	runGit(t, work, "tag", "-a", "v1.0", "-m", "release")
	runGit(t, work, "checkout", "-q", "main")
	runGit(t, work, "push", "-q", "origin", "v1.0")

	// Place the LFS objects the way a server would store them, then backdate.
	now := time.Now().UTC().Truncate(time.Second)
	aged := now.Add(-7 * 24 * time.Hour)
	putRegisteredObject(t, ctx, dbx, dataPath, r.ID(), a, aContent, aged)
	putRegisteredObject(t, ctx, dbx, dataPath, r.ID(), b, bContent, aged)
	putRegisteredObject(t, ctx, dbx, dataPath, r.ID(), f, fContent, aged)

	// External file copied in by hand: same layout, no database row.
	external := deterministicBytes(4, 333)
	putBareObject(t, dataPath, r.ID(), shaOf(external), external, now.Add(-7*24*time.Hour))

	repos, err := be.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("Repositories() = %d, want 1", len(repos))
	}

	gcRepos := make([]Repo, 0, len(repos))
	for _, rr := range repos {
		gcRepos = append(gcRepos, rr)
	}
	report, err := Run(ctx, gcRepos, dbx, st, dataPath, Options{
		Grace: 48 * time.Hour,
		now:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusCompleted {
		t.Fatalf("status = %s: %v", report.Status, report.Repos[0].Error)
	}
	rr := report.Repos[0]
	if rr.ReachableCount != 2 {
		t.Errorf("reachable_oids = %d, want 2 (A and tag-only F)", rr.ReachableCount)
	}
	if !bareObjectExists(t, dataPath, r.ID(), a) || !bareObjectExists(t, dataPath, r.ID(), f) {
		t.Error("reachable object removed")
	}
	if bareObjectExists(t, dataPath, r.ID(), b) {
		t.Error("unreachable object B survived garbage collection")
	}
	if _, err := st.GetLFSObjectByOid(ctx, dbx, r.ID(), b); !errorIsNotFound(err) {
		t.Errorf("B registration still present: %v", err)
	}
	if !bareObjectExists(t, dataPath, r.ID(), shaOf(external)) {
		t.Error("external object removed")
	}
}

func commitPointer(t *testing.T, work, name, oid string, size int64, msg string) {
	t.Helper()
	content := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid +
		"\nsize " + strconv.FormatInt(size, 10) + "\n"
	p := filepath.Join(work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "--", name)
	runGit(t, work, "commit", "-m", msg)
}

// --- helpers for the backend-backed integration test ---

func withConfigAndLogger(ctx context.Context, cfg *config.Config, logger *charmlog.Logger) context.Context {
	return charmlog.WithContext(config.WithContext(ctx, cfg), logger)
}

func migrateDB(ctx context.Context, dbx *db.DB) error {
	return migrate.Migrate(ctx, dbx)
}

func deterministicBytes(seed, size int64) []byte {
	b := make([]byte, size)
	rng := rand.New(rand.NewSource(seed))
	if _, err := rng.Read(b); err != nil {
		panic(err)
	}
	return b
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func objectPath(dataPath string, repoID int64, oid string) string {
	return filepath.Join(dataPath, "lfs", strconv.FormatInt(repoID, 10),
		"objects", oid[:2], oid[2:4], oid)
}

func putBareObject(t *testing.T, dataPath string, repoID int64, oid string, content []byte, mtime time.Time) {
	t.Helper()
	p := objectPath(dataPath, repoID, oid)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func putRegisteredObject(t *testing.T, ctx context.Context, dbx *db.DB, dataPath string, repoID int64, oid string, content []byte, aged time.Time) {
	t.Helper()
	putBareObject(t, dataPath, repoID, oid, content, aged)
	st := database.New(ctx, dbx)
	if err := st.CreateLFSObject(ctx, dbx, repoID, oid, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if _, err := dbx.ExecContext(ctx,
		"UPDATE lfs_objects SET created_at = ?, updated_at = ? WHERE repo_id = ? AND oid = ?;",
		aged.Format(time.RFC3339), aged.Format(time.RFC3339), repoID, oid); err != nil {
		t.Fatal(err)
	}
}

func bareObjectExists(t *testing.T, dataPath string, repoID int64, oid string) bool {
	t.Helper()
	_, err := os.Stat(objectPath(dataPath, repoID, oid))
	return err == nil
}

func errorIsNotFound(err error) bool {
	return errors.Is(err, db.ErrRecordNotFound)
}
