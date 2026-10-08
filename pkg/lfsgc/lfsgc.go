// Package lfsgc implements LFS object reconciliation and garbage collection.
//
// It reconciles the LFS object store against the LFS pointers that are
// reachable from a repository's current branches and tags. Only objects that
// are registered in the database, no longer referenced by any branch or tag,
// not protected by an LFS lock, and older than a grace period are eligible for
// deletion. Files placed on disk outside of the database registration are
// never removed.
//
// Every run produces a machine-readable JSON report. Scanning or verification
// failures abort the affected repository before anything is deleted, and the
// delete order (file first, database row second) makes interrupted runs
// converge when re-run.
package lfsgc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	ssgit "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
)

const (
	// StatusCompleted is the report status for a successful run.
	StatusCompleted = "completed"
	// StatusDryRun is the report status for a run that only planned deletions.
	StatusDryRun = "dry_run"
	// StatusAborted is the report status when scanning or verification failed.
	StatusAborted = "aborted"
	// StatusSkippedLocked is the report status when another run holds the lock.
	StatusSkippedLocked = "skipped_locked"
	// StatusCompletedWithSkips is the top-level status when repositories were
	// skipped because another run was active.
	StatusCompletedWithSkips = "completed_with_skips"

	// defaultGrace is how old an object (both registration and file) must be
	// before it becomes eligible for deletion. It protects objects uploaded in
	// preparation for an in-flight push, which are briefly unreferenced.
	defaultGrace = 7 * 24 * time.Hour

	// reportTimeLayout is the timestamp format used in report file names.
	reportTimeLayout = "20060102T150405Z"

	// lockPageSize is the page size used when listing LFS locks.
	lockPageSize = 100
)

// Reasons recorded for objects that are kept.
const (
	reasonReachable = "reachable"
	reasonLocked    = "locked"
	reasonRecent    = "within_grace_period"
)

var oidPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ErrAlreadyLocked is returned when another garbage collection run holds the
// repository lock.
var ErrAlreadyLocked = errors.New("another lfs garbage collection run is in progress")

// Repo is the repository surface garbage collection depends on.
// proto.Repository satisfies this interface.
type Repo interface {
	ID() int64
	Name() string
	Open() (*ssgit.Repository, error)
}

// Options controls a garbage collection run.
type Options struct {
	// DryRun plans and verifies but never deletes files or registrations.
	DryRun bool
	// Grace is the minimum age for an object, measured by both its database
	// registration time and file modification time, before deletion is
	// allowed. Zero defaults to a week; a negative duration disables the grace
	// period.
	Grace time.Duration
	// NoVerify disables SHA-256 verification of deletion candidates.
	NoVerify bool
	// ReportDir overrides where the JSON report is written. It defaults to
	// <data-path>/lfs-gc.
	ReportDir string

	// now overrides the clock, used in tests.
	now func() time.Time
}

// DiskFile describes a file found in the object store that is not registered
// in the database. It is reported for audit purposes and never deleted.
type DiskFile struct {
	Path    string    `json:"path"`
	Oid     string    `json:"oid,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// KeptObject records a registered object that was retained and why.
type KeptObject struct {
	Oid     string   `json:"oid"`
	Size    int64    `json:"size"`
	Reasons []string `json:"reasons"`
}

// DeletedObject records a removed object and the verification that preceded
// the removal.
type DeletedObject struct {
	Oid      string `json:"oid"`
	Size     int64  `json:"size"`
	Verified bool   `json:"sha256_verified"`
}

// RepoReport is the per-repository section of a garbage collection report.
type RepoReport struct {
	RepoID   int64  `json:"repo_id"`
	RepoName string `json:"repo_name"`
	Status   string `json:"status"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Refs are the branches and tags the reachability scan was based on.
	Refs []string `json:"refs"`

	RegisteredCount    int `json:"registered_count"`
	ReachableCount     int `json:"reachable_oids"`
	LockProtectedCount int `json:"lock_protected_oids"`

	// ExternalFiles are on-disk object files without a database registration.
	// They are never deleted.
	ExternalFiles []DiskFile `json:"external_files,omitempty"`

	Kept                  []KeptObject    `json:"kept,omitempty"`
	Deleted               []DeletedObject `json:"deleted,omitempty"`
	RegistrationsRemoved  []string        `json:"registrations_removed,omitempty"`
	Discrepancies         []string        `json:"discrepancies,omitempty"`
	Warnings              []string        `json:"warnings,omitempty"`
	BytesReclaimed        int64           `json:"bytes_reclaimed"`
	RegistrationsRemovedN int             `json:"registrations_removed_count"`
	Error                 string          `json:"error,omitempty"`
}

// Report is the auditable result of a garbage collection run.
type Report struct {
	RunID      string       `json:"run_id"`
	Status     string       `json:"status"`
	DryRun     bool         `json:"dry_run"`
	Grace      string       `json:"grace"`
	Verify     bool         `json:"sha256_verify"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	DataPath   string       `json:"data_path"`
	ReportPath string       `json:"report_path,omitempty"`
	Repos      []RepoReport `json:"repos"`

	ObjectsDeleted int64 `json:"objects_deleted"`
	BytesReclaimed int64 `json:"bytes_reclaimed"`
}

type candidate struct {
	obj  models.LFSObject
	size int64
}

// Run performs garbage collection across the given repositories and writes a
// JSON report under <data-path>/lfs-gc (or Options.ReportDir). Per-repository
// failures are recorded in the report and do not stop other repositories; the
// returned report is non-nil even when a repository was aborted. The only
// returned error is a failure to persist the audit report.
func Run(ctx context.Context, repos []Repo, dbx *db.DB, lstore store.LFSStore, dataPath string, opts Options) (*Report, error) {
	logger := log.FromContext(ctx).WithPrefix("lfs-gc")

	if opts.Grace == 0 {
		opts.Grace = defaultGrace
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	verify := !opts.NoVerify

	start := opts.now().UTC()
	report := &Report{
		RunID:     uuid.NewString(),
		DryRun:    opts.DryRun,
		Grace:     opts.Grace.String(),
		Verify:    verify,
		StartedAt: start,
		DataPath:  dataPath,
		Repos:     make([]RepoReport, 0, len(repos)),
	}

	anyAborted := false
	anySkipped := false
	for _, r := range repos {
		rr := RepoReport{
			RepoID:   r.ID(),
			RepoName: r.Name(),
			Status:   StatusCompleted,
		}
		if opts.DryRun {
			rr.Status = StatusDryRun
		}
		report.Repos = append(report.Repos, rr)
		// Take the address of the element we just appended so runRepo fills in
		// the report section the caller will read.
		cur := &report.Repos[len(report.Repos)-1]

		switch err := runRepo(ctx, r, dbx, lstore, dataPath, opts, cur); {
		case err == nil:
		case errors.Is(err, ErrAlreadyLocked):
			cur.Status = StatusSkippedLocked
			cur.Error = err.Error()
			anySkipped = true
			logger.Warn("skipping repository: %v", err)
		default:
			cur.Status = StatusAborted
			if cur.Error == "" {
				cur.Error = err.Error()
			}
			cur.Discrepancies = append(cur.Discrepancies, err.Error())
			anyAborted = true
			logger.Errorf("aborted garbage collection for %s: %v", r.Name(), err)
		}
	}

	switch {
	case anyAborted:
		report.Status = StatusAborted
	case anySkipped:
		report.Status = StatusCompletedWithSkips
	case opts.DryRun:
		report.Status = StatusDryRun
	default:
		report.Status = StatusCompleted
	}

	for i := range report.Repos {
		rr := &report.Repos[i]
		report.ObjectsDeleted += int64(len(rr.Deleted))
		report.BytesReclaimed += rr.BytesReclaimed
	}

	report.FinishedAt = opts.now().UTC()
	reportPath, err := writeReport(dataPath, opts.ReportDir, report)
	if err != nil {
		return report, fmt.Errorf("write garbage collection report: %w", err)
	}
	report.ReportPath = reportPath

	return report, nil
}

// runRepo performs the scan, plan, verify and delete phases for a single
// repository. Deletions only start after every candidate has been verified.
func runRepo(ctx context.Context, r Repo, dbx *db.DB, lstore store.LFSStore, dataPath string, opts Options, rep *RepoReport) error {
	now := opts.now()
	rep.StartedAt = now.UTC()
	defer func() {
		rep.FinishedAt = opts.now().UTC()
	}()

	lfsRoot := filepath.Join(dataPath, "lfs", strconv.FormatInt(r.ID(), 10))
	unlock, err := acquireLock(lfsRoot)
	if err != nil {
		return err
	}
	defer unlock() //nolint:errcheck

	gr, err := r.Open()
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	// Phase 1: enumerate the branches and tags that define reachability.
	refNames, err := listBranchAndTagRefs(gr.Path)
	if err != nil {
		return fmt.Errorf("list references: %w", err)
	}
	rep.Refs = refNames

	// Phase 2: collect the LFS pointers reachable from those refs.
	reachable, err := scanReachablePointers(ctx, gr.Path, refNames)
	if err != nil {
		return fmt.Errorf("scan reachable pointers: %w", err)
	}
	rep.ReachableCount = len(reachable)

	// Phase 3: resolve every locked path to the object it currently points at.
	locks, err := listLocks(ctx, lstore, dbx, r.ID())
	if err != nil {
		return fmt.Errorf("list lfs locks: %w", err)
	}
	protected, err := scanLockedPointers(ctx, gr.Path, locks, refNames)
	if err != nil {
		return fmt.Errorf("resolve locked paths: %w", err)
	}
	rep.LockProtectedCount = len(protected)

	// Phase 4: registered objects and on-disk files.
	registered, err := lstore.GetLFSObjects(ctx, dbx, r.ID())
	if err != nil {
		return fmt.Errorf("list registered lfs objects: %w", db.WrapError(err))
	}
	rep.RegisteredCount = len(registered)
	registeredOids := make(map[string]struct{}, len(registered))
	for _, obj := range registered {
		registeredOids[obj.Oid] = struct{}{}
	}

	strg := storage.NewLocalStorage(lfsRoot)
	diskFiles, err := walkObjectFiles(lfsRoot)
	if err != nil {
		return fmt.Errorf("walk object store: %w", err)
	}
	// Files without a database registration are external (for example objects
	// copied in by hand); report them but never touch them.
	for _, f := range diskFiles {
		if f.Oid != "" {
			if _, ok := registeredOids[f.Oid]; ok {
				continue
			}
		}
		rep.ExternalFiles = append(rep.ExternalFiles, f)
	}

	cutoff := now.Add(-opts.Grace)
	var candidates []candidate
	var staleRegistrations []models.LFSObject

	for _, obj := range registered {
		if !oidPattern.MatchString(obj.Oid) {
			return fmt.Errorf("registered object %q has an invalid oid format; refusing to continue", obj.Oid)
		}

		rel := path.Join("objects", objectRelativePath(obj.Oid))
		fi, statErr := strg.Stat(rel)

		_, isReachable := reachable[obj.Oid]
		_, isProtected := protected[obj.Oid]
		isReferenced := isReachable || isProtected

		if errors.Is(statErr, fs.ErrNotExist) {
			// A referenced object missing from disk means repository content
			// is broken: never prune anything in this state.
			if isReferenced {
				return fmt.Errorf("registered object %s is referenced but missing from the object store", obj.Oid)
			}
			// A recent missing registration is likely an upload in progress
			// (the row is written before the file is renamed into place).
			if !olderThan(obj.CreatedAt, cutoff) {
				rep.Kept = append(rep.Kept, KeptObject{
					Oid: obj.Oid, Size: obj.Size, Reasons: []string{reasonRecent},
				})
				continue
			}
			// Object is gone but its registration remains. This is the
			// residue of an interrupted run (files are deleted first);
			// removing the row completes it and converges on re-run.
			staleRegistrations = append(staleRegistrations, obj)
			continue
		}
		if statErr != nil {
			return fmt.Errorf("stat object %s: %w", obj.Oid, statErr)
		}

		var reasons []string
		if isReachable {
			reasons = append(reasons, reasonReachable)
		}
		if isProtected {
			reasons = append(reasons, reasonLocked)
		}
		if !olderThan(obj.CreatedAt, cutoff) || !fi.ModTime().Before(cutoff) {
			reasons = append(reasons, reasonRecent)
		}
		if len(reasons) > 0 {
			rep.Kept = append(rep.Kept, KeptObject{Oid: obj.Oid, Size: obj.Size, Reasons: reasons})
			continue
		}

		candidates = append(candidates, candidate{obj: obj, size: fi.Size()})
	}

	// Sort everything so reports are deterministic regardless of DB ordering.
	sort.Slice(rep.Kept, func(i, j int) bool { return rep.Kept[i].Oid < rep.Kept[j].Oid })
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].obj.Oid < candidates[j].obj.Oid })
	sort.Slice(staleRegistrations, func(i, j int) bool {
		return staleRegistrations[i].Oid < staleRegistrations[j].Oid
	})

	// Phase 5: verify every candidate before touching anything. A single
	// mismatch aborts the whole repository run.
	for _, c := range candidates {
		rel := path.Join("objects", objectRelativePath(c.obj.Oid))
		if c.size != c.obj.Size {
			return fmt.Errorf("object %s size mismatch: database records %d bytes, file is %d bytes",
				c.obj.Oid, c.obj.Size, c.size)
		}
		if opts.NoVerify {
			continue
		}
		if err := verifyObject(strg, rel, c.obj.Oid); err != nil {
			return fmt.Errorf("verify object %s: %w", c.obj.Oid, err)
		}
	}

	if opts.DryRun {
		// Report the planned actions without performing them.
		for _, c := range candidates {
			rep.Deleted = append(rep.Deleted, DeletedObject{
				Oid: c.obj.Oid, Size: c.obj.Size, Verified: !opts.NoVerify,
			})
			rep.BytesReclaimed += c.obj.Size
		}
		for _, obj := range staleRegistrations {
			rep.RegistrationsRemoved = append(rep.RegistrationsRemoved, obj.Oid)
			rep.RegistrationsRemovedN++
		}
		return nil
	}

	// Phase 6a: complete stale registrations first. No files are involved, and
	// a failure here aborts before any object file is removed.
	for _, obj := range staleRegistrations {
		if err := lstore.DeleteLFSObjectByOid(ctx, dbx, r.ID(), obj.Oid); err != nil {
			return fmt.Errorf("remove stale registration %s: %w", obj.Oid, db.WrapError(err))
		}
		rep.RegistrationsRemoved = append(rep.RegistrationsRemoved, obj.Oid)
		rep.RegistrationsRemovedN++
	}

	// Phase 6b: delete the file first, then its registration. Crashing between
	// the two leaves a stale registration that the next run cleans up; the
	// reverse order would orphan the file as an untracked object forever.
	for _, c := range candidates {
		rel := path.Join("objects", objectRelativePath(c.obj.Oid))

		// Re-check state gathered at the start of the run before mutating.
		fi, err := strg.Stat(rel)
		if err != nil {
			return fmt.Errorf("re-stat object %s: %w", c.obj.Oid, err)
		}
		if fi.Size() != c.size {
			return fmt.Errorf("object %s changed during the run; aborting", c.obj.Oid)
		}
		if _, err := lstore.GetLFSObjectByOid(ctx, dbx, r.ID(), c.obj.Oid); err != nil {
			return fmt.Errorf("re-read registration %s: %w", c.obj.Oid, db.WrapError(err))
		}

		if err := strg.Delete(rel); err != nil {
			return fmt.Errorf("delete object %s: %w", c.obj.Oid, err)
		}
		if err := lstore.DeleteLFSObjectByOid(ctx, dbx, r.ID(), c.obj.Oid); err != nil {
			// The file is already gone. Record the warning; the next run
			// removes the leftover registration.
			warn := fmt.Sprintf("object %s deleted but registration removal failed: %v", c.obj.Oid, db.WrapError(err))
			rep.Warnings = append(rep.Warnings, warn)
		}

		rep.Deleted = append(rep.Deleted, DeletedObject{
			Oid: c.obj.Oid, Size: c.obj.Size, Verified: !opts.NoVerify,
		})
		rep.BytesReclaimed += c.obj.Size
	}

	return nil
}

// listLocks paginates through all of a repository's LFS locks.
func listLocks(ctx context.Context, lstore store.LFSStore, h db.Handler, repoID int64) ([]models.LFSLock, error) {
	var locks []models.LFSLock
	for page := 1; ; page++ {
		batch, err := lstore.GetLFSLocks(ctx, h, repoID, page, lockPageSize)
		if err != nil {
			return nil, db.WrapError(err)
		}
		locks = append(locks, batch...)
		if len(batch) < lockPageSize {
			break
		}
	}
	return locks, nil
}

// olderThan reports whether t is strictly before cutoff. A zero time is
// treated as ancient so missing timestamps do not block cleanup.
func olderThan(t time.Time, cutoff time.Time) bool {
	if t.IsZero() {
		return true
	}
	return t.Before(cutoff)
}

// objectRelativePath returns the objects/<aa>/<bb>/<oid> relative path used by
// Git LFS.
func objectRelativePath(oid string) string {
	return path.Join(oid[0:2], oid[2:4], oid)
}

// verifyObject streams the stored object through SHA-256 and compares the
// digest with the content-addressed oid.
func verifyObject(strg *storage.LocalStorage, rel, oid string) error {
	f, err := strg.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", oid, got)
	}
	return nil
}

// walkObjectFiles lists the files under the LFS objects directory. Files with
// valid content-addressed names are returned keyed by oid; everything else is
// returned as unrecognized external files. The objects directory not existing
// is not an error.
func walkObjectFiles(lfsRoot string) ([]DiskFile, error) {
	objectsRoot := filepath.Join(lfsRoot, "objects")
	var files []DiskFile

	err := filepath.WalkDir(objectsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == objectsRoot {
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(lfsRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		df := DiskFile{
			Path:    rel,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC(),
		}
		if oid, ok := namedObjectOID(rel); ok {
			df.Oid = oid
		}
		files = append(files, df)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// namedObjectOID returns the oid when rel has the standard objects/aa/bb/oid
// layout with a valid SHA-256 name.
func namedObjectOID(rel string) (string, bool) {
	dir, oid := path.Split(rel) // dir ends in "/"
	dir = strings.TrimSuffix(dir, "/")
	bb := path.Base(dir)
	aa := path.Base(path.Dir(dir))
	if len(aa) != 2 || len(bb) != 2 || !oidPattern.MatchString(oid) {
		return "", false
	}
	if oid[0:2] != aa || oid[2:4] != bb {
		return "", false
	}
	return oid, true
}

// writeReport atomically writes the run report as JSON and returns its path.
func writeReport(dataPath, reportDir string, report *Report) (string, error) {
	if reportDir == "" {
		reportDir = filepath.Join(dataPath, "lfs-gc")
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return "", err
	}

	name := fmt.Sprintf("lfs-gc-%s-%s.json",
		report.StartedAt.Format(reportTimeLayout), report.RunID[:8])
	final := filepath.Join(reportDir, name)
	tmp := final + ".tmp"

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return final, nil
}
