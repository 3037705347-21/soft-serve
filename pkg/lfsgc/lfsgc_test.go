package lfsgc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	logr "github.com/charmbracelet/soft-serve/pkg/log"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

const testRepoID int64 = 1

// testRepo is a minimal lfsgc.Repo backed by a working tree on disk.
type testRepo struct {
	id   int64
	name string
	path string
}

func (r *testRepo) ID() int64                      { return r.id }
func (r *testRepo) Name() string                   { return r.name }
func (r *testRepo) Open() (*git.Repository, error) { return git.Open(r.path) }

type fixture struct {
	t        *testing.T
	tmp      string
	dataPath string
	work     string
	lfsRoot  string
	ctx      context.Context
	dbx      *db.DB
	st       store.Store
	now      time.Time
	rng      *rand.Rand
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	dataPath := filepath.Join(tmp, "data")
	work := filepath.Join(tmp, "work")

	if err := os.MkdirAll(dataPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "init", "-b", "main", work)

	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	logger, _, err := logr.NewLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := charmlog.WithContext(config.WithContext(context.Background(), cfg), logger)

	dbx, err := db.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	st := database.New(ctx, dbx)

	now := time.Now().UTC().Truncate(time.Second)
	return &fixture{
		t:        t,
		tmp:      tmp,
		dataPath: dataPath,
		work:     work,
		lfsRoot:  filepath.Join(dataPath, "lfs", strconv.FormatInt(testRepoID, 10)),
		ctx:      ctx,
		dbx:      dbx,
		st:       st,
		now:      now,
		rng:      rand.New(rand.NewSource(42)),
	}
}

func (f *fixture) repo() Repo {
	return &testRepo{id: testRepoID, name: "test", path: f.work}
}

// run executes garbage collection with a 48h grace period unless overridden.
func (f *fixture) run(opts Options) *Report {
	f.t.Helper()
	if opts.Grace == 0 {
		opts.Grace = 48 * time.Hour
	}
	opts.now = func() time.Time { return f.now }
	report, err := Run(f.ctx, []Repo{f.repo()}, f.dbx, f.st, f.dataPath, opts)
	if err != nil {
		f.t.Fatalf("Run: %v", err)
	}
	return report
}

// addPointerCommit writes an LFS pointer at name and commits it on the
// current branch.
func (f *fixture) addPointerCommit(name, oid string, size int64, msg string) {
	f.t.Helper()
	content := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
	path := filepath.Join(f.work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, f.work, "add", "--", name)
	runGit(f.t, f.work, "commit", "-m", msg)
}

// randomContent returns deterministic random bytes of the given size.
func (f *fixture) randomContent(size int) []byte {
	b := make([]byte, size)
	if _, err := f.rng.Read(b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

// putObject stores content in the LFS layout and optionally registers it.
// When old is true both the registration and the file are backdated past the
// grace period. It returns the content's oid and pointer metadata.
func (f *fixture) putObject(content []byte, register, old bool) string {
	f.t.Helper()
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	rel := filepath.Join(f.lfsRoot, "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(rel, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if register {
		if err := f.st.CreateLFSObject(f.ctx, f.dbx, testRepoID, oid, int64(len(content))); err != nil {
			f.t.Fatal(err)
		}
	}
	if old {
		aged := f.now.Add(-7 * 24 * time.Hour)
		if _, err := f.dbx.ExecContext(f.ctx,
			"UPDATE lfs_objects SET created_at = ?, updated_at = ? WHERE oid = ?;",
			aged.Format(time.RFC3339), aged.Format(time.RFC3339), oid); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Chtimes(rel, aged, aged); err != nil {
			f.t.Fatal(err)
		}
	}
	return oid
}

// putNamedObject writes arbitrary content at the storage path for oid, which
// need not hash to it. Used to simulate corruption.
func (f *fixture) putNamedObject(oid string, content []byte, register, old bool) {
	f.t.Helper()
	rel := filepath.Join(f.lfsRoot, "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(rel, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if register {
		if err := f.st.CreateLFSObject(f.ctx, f.dbx, testRepoID, oid, int64(len(content))); err != nil {
			f.t.Fatal(err)
		}
	}
	if old {
		aged := f.now.Add(-7 * 24 * time.Hour)
		if _, err := f.dbx.ExecContext(f.ctx,
			"UPDATE lfs_objects SET created_at = ?, updated_at = ? WHERE oid = ?;",
			aged.Format(time.RFC3339), aged.Format(time.RFC3339), oid); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Chtimes(rel, aged, aged); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) removeObjectFile(oid string) {
	f.t.Helper()
	rel := filepath.Join(f.lfsRoot, "objects", oid[:2], oid[2:4], oid)
	if err := os.Remove(rel); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) objectExists(oid string) bool {
	f.t.Helper()
	rel := filepath.Join(f.lfsRoot, "objects", oid[:2], oid[2:4], oid)
	_, err := os.Stat(rel)
	return err == nil
}

func (f *fixture) registered(oid string) bool {
	f.t.Helper()
	_, err := f.st.GetLFSObjectByOid(f.ctx, f.dbx, testRepoID, oid)
	if err == nil {
		return true
	}
	if errors.Is(err, db.ErrRecordNotFound) {
		return false
	}
	f.t.Fatalf("GetLFSObjectByOid: %v", err)
	return false
}

func (f *fixture) addLock(path, refname string) {
	f.t.Helper()
	if err := f.st.CreateLFSLockForUser(f.ctx, f.dbx, testRepoID, 1, path, refname); err != nil {
		f.t.Fatalf("CreateLFSLockForUser: %v", err)
	}
}

func (f *fixture) putExternalFile(relPath string, content []byte) {
	f.t.Helper()
	p := filepath.Join(f.lfsRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// buildTypicalFixture builds the reference scenario:
//   - main: pointer A reachable, pointer L reachable and locked
//   - pointer B used to be on main but was dropped by a history reset
//   - pointer F only reachable through an annotated tag (branch deleted)
//   - pointer C unreferenced but recently uploaded (within grace)
//   - an external object D copied in without a database registration
//   - an unrecognized external file dumped under objects/
func (f *fixture) buildTypicalFixture() (a, b, c, d, l, f64 string) {
	a = f.putObject(f.randomContent(2048), true, true)
	l = f.putObject(f.randomContent(4096), true, true)
	f.addPointerCommit("a.bin", a, 2048, "add a")
	f.addPointerCommit("locked.bin", l, 4096, "add locked")
	f.addLock("locked.bin", "refs/heads/main")

	b = f.putObject(f.randomContent(8192), true, true)
	f.addPointerCommit("b.bin", b, 8192, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")

	f64 = f.putObject(f.randomContent(1024), true, true)
	runGit(f.t, f.work, "checkout", "-b", "keep")
	f.addPointerCommit("f.bin", f64, 1024, "add f")
	runGit(f.t, f.work, "tag", "-a", "v1.0", "-m", "release")
	runGit(f.t, f.work, "checkout", "main")
	runGit(f.t, f.work, "branch", "-D", "keep")

	c = f.putObject(f.randomContent(512), true, false)

	// External content-addressed file without a registration.
	d = f.putObject(f.randomContent(777), false, true)
	// An unrelated file someone dropped into the store.
	f.putExternalFile("objects/notes.txt", []byte("do not touch me"))

	return a, b, c, d, l, f64
}

func reportFor(t *testing.T, report *Report) RepoReport {
	t.Helper()
	if len(report.Repos) != 1 {
		t.Fatalf("expected 1 repo report, got %d", len(report.Repos))
	}
	return report.Repos[0]
}

func findDeleted(report *Report, oid string) (DeletedObject, bool) {
	for _, d := range report.Repos[0].Deleted {
		if d.Oid == oid {
			return d, true
		}
	}
	return DeletedObject{}, false
}

func keptReasons(report *Report, oid string) []string {
	for _, k := range report.Repos[0].Kept {
		if k.Oid == oid {
			return k.Reasons
		}
	}
	return nil
}

func externalPaths(report *Report) map[string]bool {
	out := map[string]bool{}
	for _, f := range report.Repos[0].ExternalFiles {
		out[f.Path] = true
	}
	return out
}

// TestRunPrunesUnreachable covers the main scenario: only old, registered,
// unreferenced, unlocked objects are removed; everything else is retained and
// accounted for.
func TestRunPrunesUnreachable(t *testing.T) {
	f := newFixture(t)
	a, b, c, d, l, tagged := f.buildTypicalFixture()

	report := f.run(Options{})
	rr := reportFor(t, report)

	if report.Status != StatusCompleted {
		t.Fatalf("status = %s, want %s: %v", report.Status, StatusCompleted, rr.Discrepancies)
	}
	if got := rr.Status; got != StatusCompleted {
		t.Fatalf("repo status = %s, want completed (%s)", got, rr.Error)
	}

	// B is the only object eligible for deletion.
	if _, ok := findDeleted(report, b); !ok {
		t.Errorf("object %s not marked deleted", b)
	}
	if !f.objectExists(a) || !f.registered(a) {
		t.Error("reachable object A was removed")
	}
	if f.objectExists(b) || f.registered(b) {
		t.Error("unreachable object B was not removed (file or registration left)")
	}
	if !f.objectExists(c) || !f.registered(c) {
		t.Error("recent object C within the grace period was removed")
	}
	if !f.objectExists(d) {
		t.Error("external object D without registration was removed")
	}
	if !f.objectExists(l) || !f.objectExists(tagged) {
		t.Error("locked/tag-only reachable objects were removed")
	}
	if _, err := os.Stat(filepath.Join(f.lfsRoot, "objects", "notes.txt")); err != nil {
		t.Errorf("unrecognized external file removed: %v", err)
	}

	if rr.ReachableCount != 3 {
		t.Errorf("reachable_oids = %d, want 3 (A, L, tag-only F)", rr.ReachableCount)
	}
	if rr.LockProtectedCount < 1 {
		t.Errorf("lock_protected_oids = %d, want >= 1", rr.LockProtectedCount)
	}
	if got := keptReasons(report, c); !contains(got, reasonRecent) {
		t.Errorf("C reasons = %v, want %q", got, reasonRecent)
	}
	if got := keptReasons(report, l); !contains(got, reasonReachable) || !contains(got, reasonLocked) {
		t.Errorf("locked L reasons = %v, want reachable+locked", got)
	}
	if got := keptReasons(report, tagged); !contains(got, reasonReachable) {
		t.Errorf("tag-only F reasons = %v, want reachable", got)
	}
	paths := externalPaths(report)
	if !paths["objects/"+d[:2]+"/"+d[2:4]+"/"+d] {
		t.Errorf("external object D not reported: %v", paths)
	}
	if !paths["objects/notes.txt"] {
		t.Errorf("unrecognized external file not reported: %v", paths)
	}

	wantRefs := []string{"refs/heads/main", "refs/tags/v1.0"}
	if len(rr.Refs) != 2 || rr.Refs[0] != wantRefs[0] || rr.Refs[1] != wantRefs[1] {
		t.Errorf("refs = %v, want %v", rr.Refs, wantRefs)
	}

	// The audit report must exist on disk and parse.
	if report.ReportPath == "" {
		t.Fatal("no report path")
	}
	data, err := os.ReadFile(report.ReportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if decoded.RunID != report.RunID {
		t.Error("decoded report run id mismatch")
	}

	// Re-running converges: nothing new to delete.
	again := f.run(Options{})
	if again.Status != StatusCompleted {
		t.Fatalf("second run status = %s", again.Status)
	}
	if len(again.Repos[0].Deleted) != 0 {
		t.Errorf("second run deleted %d objects, want 0", len(again.Repos[0].Deleted))
	}
	if !f.objectExists(a) {
		t.Error("A disappeared after convergent re-run")
	}
}

// TestDryRunPlansButDoesNotDelete verifies the dry run leaves state intact.
func TestDryRunPlansButDoesNotDelete(t *testing.T) {
	f := newFixture(t)
	a := f.putObject(f.randomContent(128), true, true)
	f.addPointerCommit("a.bin", a, 128, "add a")
	b := f.putObject(f.randomContent(256), true, true)
	f.addPointerCommit("b.bin", b, 256, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")

	report := f.run(Options{DryRun: true})
	if report.Status != StatusDryRun {
		t.Fatalf("status = %s, want dry_run", report.Status)
	}
	if _, ok := findDeleted(report, b); !ok {
		t.Error("dry-run report does not plan B for deletion")
	}
	if !f.objectExists(b) || !f.registered(b) {
		t.Error("dry run deleted B")
	}
	if report.BytesReclaimed != 256 {
		t.Errorf("bytes_reclaimed = %d, want 256", report.BytesReclaimed)
	}

	// A subsequent real run performs the planned deletion.
	real := f.run(Options{})
	if real.Status != StatusCompleted {
		t.Fatalf("real run status = %s", real.Status)
	}
	if f.objectExists(b) || f.registered(b) {
		t.Error("real run did not delete B")
	}
}

// TestCorruptObjectAbortsRun ensures verification failure aborts before any
// deletion in the run, and that repairing the object lets a re-run converge.
func TestCorruptObjectAbortsRun(t *testing.T) {
	f := newFixture(t)
	a := f.putObject(f.randomContent(64), true, true)
	f.addPointerCommit("a.bin", a, 64, "add a")
	b := f.putObject(f.randomContent(512), true, true)
	f.addPointerCommit("b.bin", b, 512, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")

	// E is registered with the size and oid of good content, but the file
	// holds different content of the same length.
	good := f.randomContent(256)
	sum := sha256.Sum256(good)
	e := hex.EncodeToString(sum[:])
	wrong := f.randomContent(256)
	f.putNamedObject(e, wrong, true, true)

	report := f.run(Options{})
	if report.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", report.Status)
	}
	rr := reportFor(t, report)
	if !strings.Contains(rr.Error, "checksum mismatch") {
		t.Errorf("error = %q, want checksum mismatch", rr.Error)
	}
	if f.objectExists(b) != true || f.registered(b) != true {
		t.Error("B was deleted despite the run aborting")
	}
	if !f.objectExists(e) {
		t.Error("corrupt E disappeared during an aborted run")
	}

	// Repair E and re-run: both B and E are now collected.
	f.putNamedObject(e, good, false, true)
	again := f.run(Options{})
	if again.Status != StatusCompleted {
		t.Fatalf("re-run status = %s, want completed: %s", again.Status, again.Repos[0].Error)
	}
	if f.objectExists(b) || f.registered(b) {
		t.Error("B not collected after repair")
	}
	if f.objectExists(e) || f.registered(e) {
		t.Error("repaired E not collected")
	}
	if !f.objectExists(a) {
		t.Error("reachable A removed")
	}
}

// TestSizeMismatchAbortsRun ensures a database/file size mismatch aborts the
// run without deleting anything.
func TestSizeMismatchAbortsRun(t *testing.T) {
	f := newFixture(t)
	runGit(f.t, f.work, "commit", "--allow-empty", "-m", "init")
	b := f.putObject(f.randomContent(512), true, true)
	f.addPointerCommit("b.bin", b, 512, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")
	// Tamper with the registered size.
	if _, err := f.dbx.ExecContext(f.ctx,
		"UPDATE lfs_objects SET size = 999 WHERE oid = ?;", b); err != nil {
		t.Fatal(err)
	}

	report := f.run(Options{})
	if report.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", report.Status)
	}
	if !strings.Contains(report.Repos[0].Error, "size mismatch") {
		t.Errorf("error = %q, want size mismatch", report.Repos[0].Error)
	}
	if !f.objectExists(b) {
		t.Error("B deleted despite size mismatch")
	}
}

// TestStaleRegistrationAndInFlightUpload covers the two states where the
// registration exists but the file does not: a past interrupted run leaves a
// stale row to clean up; a fresh missing row is an upload in flight and must
// be protected by the grace period.
func TestStaleRegistrationAndInFlightUpload(t *testing.T) {
	f := newFixture(t)
	runGit(f.t, f.work, "commit", "--allow-empty", "-m", "init")
	b := f.putObject(f.randomContent(512), true, true)
	f.addPointerCommit("b.bin", b, 512, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")
	f.removeObjectFile(b) // simulate a previous run that deleted the file

	g := f.putObject(f.randomContent(128), false, false)
	// Registration exists, file never landed yet; leave timestamps fresh.
	if err := f.st.CreateLFSObject(f.ctx, f.dbx, testRepoID, g, 128); err != nil {
		t.Fatal(err)
	}
	f.removeObjectFile(g)

	plan := f.run(Options{DryRun: true})
	if plan.Status != StatusDryRun {
		t.Fatalf("dry run status = %s", plan.Status)
	}
	if !contains(plan.Repos[0].RegistrationsRemoved, b) {
		t.Errorf("dry run does not plan stale registration removal: %v", plan.Repos[0].RegistrationsRemoved)
	}
	if !f.registered(b) {
		t.Error("dry run removed the stale registration")
	}

	report := f.run(Options{})
	if report.Status != StatusCompleted {
		t.Fatalf("status = %s, want completed: %s", report.Status, report.Repos[0].Error)
	}
	rr := reportFor(t, report)
	if !contains(rr.RegistrationsRemoved, b) {
		t.Errorf("stale registration %s not removed: %v", b, rr.RegistrationsRemoved)
	}
	if f.registered(b) {
		t.Error("stale registration for B still present")
	}
	if !f.registered(g) {
		t.Error("in-flight registration G removed")
	}
	if got := keptReasons(report, g); !contains(got, reasonRecent) {
		t.Errorf("G reasons = %v, want %q", got, reasonRecent)
	}
	if len(rr.Deleted) != 0 {
		t.Errorf("deleted %d files, want 0", len(rr.Deleted))
	}
}

// TestReferencedObjectMissingAborts ensures a referenced object missing from
// disk aborts the whole run rather than pruning around the breakage.
func TestReferencedObjectMissingAborts(t *testing.T) {
	f := newFixture(t)
	a := f.putObject(f.randomContent(64), true, true)
	f.addPointerCommit("a.bin", a, 64, "add a")
	b := f.putObject(f.randomContent(128), true, true)
	f.addPointerCommit("b.bin", b, 128, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")
	f.removeObjectFile(a) // referenced content is broken

	report := f.run(Options{})
	if report.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", report.Status)
	}
	if !strings.Contains(report.Repos[0].Error, "missing from the object store") {
		t.Errorf("error = %q", report.Repos[0].Error)
	}
	if !f.registered(b) || !f.objectExists(b) {
		t.Error("B deleted while a referenced object was missing")
	}
}

// TestConcurrentRunIsSkipped verifies the repository lock serializes runs.
func TestConcurrentRunIsSkipped(t *testing.T) {
	f := newFixture(t)
	runGit(f.t, f.work, "commit", "--allow-empty", "-m", "init")

	unlock, err := acquireLock(f.lfsRoot)
	if err != nil {
		t.Fatal(err)
	}
	report := f.run(Options{})
	rr := reportFor(t, report)
	if rr.Status != StatusSkippedLocked {
		t.Fatalf("status = %s, want %s", rr.Status, StatusSkippedLocked)
	}
	if report.Status != StatusCompletedWithSkips {
		t.Errorf("top status = %s, want %s", report.Status, StatusCompletedWithSkips)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	again := f.run(Options{})
	if again.Repos[0].Status != StatusCompleted {
		t.Fatalf("status after release = %s", again.Repos[0].Status)
	}
}

// TestEmptyRepository verifies a repository with no refs scans cleanly.
func TestEmptyRepository(t *testing.T) {
	f := newFixture(t)
	report := f.run(Options{})
	if report.Status != StatusCompleted {
		t.Fatalf("status = %s, want completed: %s", report.Status, report.Repos[0].Error)
	}
	if len(report.Repos[0].Refs) != 0 {
		t.Errorf("refs = %v, want none", report.Repos[0].Refs)
	}
}

// TestPointerIsNeverConfusedWithContent makes sure large blobs and non-pointer
// small blobs are not treated as LFS references.
func TestScanIgnoresNonPointers(t *testing.T) {
	f := newFixture(t)
	// Commit a small regular (non-pointer) file with hex-looking content.
	regular := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(f.work, "regular.txt"), []byte(regular), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(f.t, f.work, "add", "regular.txt")
	runGit(f.t, f.work, "commit", "-m", "regular")

	// An unreferenced object named with the same 64 a's but otherwise valid
	// must still be collectable: the regular blob is NOT an LFS pointer.
	candidate := f.putObject([]byte(strings.Repeat("a", 64)), true, true)
	report := f.run(Options{})
	if report.Status != StatusCompleted {
		t.Fatalf("status = %s: %s", report.Status, report.Repos[0].Error)
	}
	if report.Repos[0].ReachableCount != 0 {
		t.Errorf("reachable_oids = %d, want 0", report.Repos[0].ReachableCount)
	}
	if _, ok := findDeleted(report, candidate); !ok {
		t.Error("unreferenced object not collected because of a false pointer match")
	}
}

// TestInvalidOidRegistrationAborts guards against malformed rows in the
// database: nothing should be deleted while the registry itself is suspect.
func TestInvalidOidRegistrationAborts(t *testing.T) {
	f := newFixture(t)
	runGit(f.t, f.work, "commit", "--allow-empty", "-m", "init")
	if _, err := f.dbx.ExecContext(f.ctx,
		"INSERT INTO lfs_objects (repo_id, oid, size, created_at, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);",
		testRepoID, "../../etc/passwd", 1); err != nil {
		t.Fatal(err)
	}
	report := f.run(Options{})
	if report.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", report.Status)
	}
	if !strings.Contains(report.Repos[0].Error, "invalid oid") {
		t.Errorf("error = %q, want invalid oid", report.Repos[0].Error)
	}
}

// TestNoVerifyStillChecksSize documents that --no-verify keeps the size guard.
func TestNoVerifyStillChecksSize(t *testing.T) {
	f := newFixture(t)
	runGit(f.t, f.work, "commit", "--allow-empty", "-m", "init")
	b := f.putObject(f.randomContent(256), true, true)
	f.addPointerCommit("b.bin", b, 256, "add b")
	runGit(f.t, f.work, "reset", "--hard", "HEAD~1")
	report := f.run(Options{NoVerify: true})
	if report.Status != StatusCompleted {
		t.Fatalf("status = %s: %s", report.Status, report.Repos[0].Error)
	}
	if d, ok := findDeleted(report, b); !ok || d.Verified {
		t.Errorf("B deletion = %+v, want unverified deletion", d)
	}
	if f.objectExists(b) {
		t.Error("B not deleted")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// runGit runs a git command in dir with a hermetic environment.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
