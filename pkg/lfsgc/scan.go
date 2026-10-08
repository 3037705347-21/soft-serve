package lfsgc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// maxPointerSize is the maximum blob size considered when looking for LFS
// pointer files. Pointer files are a few hundred bytes long; any blob larger
// than this cannot be a pointer.
const maxPointerSize = 1024

// scanReachablePointers returns the set of LFS object oids referenced by
// pointer blobs reachable from the given branch and tag reference names.
//
// It streams `git rev-list --objects <refs>` into `git cat-file --batch`,
// keeping only blobs small enough to be pointer files. Any git command failure
// is returned so the caller can abort before deleting anything.
func scanReachablePointers(ctx context.Context, repoPath string, refs []string) (map[string]struct{}, error) {
	oids := make(map[string]struct{})
	if len(refs) == 0 {
		return oids, nil
	}

	revListOut := makePipe()
	catIn := makePipe()
	catOut := makePipe()

	var revListErr, catErr error
	var revStderr, catStderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(3)

	// git rev-list --objects <refs...>
	go func() {
		defer wg.Done()
		defer revListOut.closeWrite()
		args := append([]string{"rev-list", "--objects"}, refs...)
		err := gitm.NewCommandWithContext(ctx, args...).
			WithTimeout(-1).
			RunInDirWithOptions(repoPath, gitm.RunInDirOptions{
				Stdout: revListOut.w,
				Stderr: &revStderr,
			})
		if err != nil {
			revListErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(revStderr.String()))
			_ = revListOut.closeWriteWith(err)
		}
	}()

	// Turn "<sha> <path>" lines into cat-file batch queries.
	go func() {
		defer wg.Done()
		defer revListOut.closeRead()
		defer catIn.closeWrite()
		sc := bufio.NewScanner(revListOut.r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 0 {
				continue
			}
			if _, err := catIn.w.Write([]byte(fields[0] + "\n")); err != nil {
				_ = catIn.closeWriteWith(err)
				return
			}
		}
		if err := sc.Err(); err != nil {
			_ = catIn.closeWriteWith(err)
		}
	}()

	// git cat-file --batch
	go func() {
		defer wg.Done()
		defer catIn.closeRead()
		defer catOut.closeWrite()
		err := gitm.NewCommandWithContext(ctx, "cat-file", "--batch").
			WithTimeout(-1).
			RunInDirWithOptions(repoPath, gitm.RunInDirOptions{
				Stdin:  catIn.r,
				Stdout: catOut.w,
				Stderr: &catStderr,
			})
		if err != nil {
			catErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(catStderr.String()))
			_ = catOut.closeWriteWith(err)
		}
	}()

	parseErr := parseBatch(ctx, catOut.r, oids)
	wg.Wait()

	if revListErr != nil {
		return nil, fmt.Errorf("git rev-list: %w", revListErr)
	}
	if catErr != nil {
		return nil, fmt.Errorf("git cat-file: %w", catErr)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("read git cat-file output: %w", parseErr)
	}
	return oids, nil
}

// scanLockedPointers resolves locked file paths to the LFS object oids they
// currently point at. A lock carries an optional refname; in addition every
// lock is resolved against HEAD and every branch/tag tip, so a locked path is
// protected regardless of which ref the locker works from.
func scanLockedPointers(ctx context.Context, repoPath string, locks []models.LFSLock, refs []string) (map[string]struct{}, error) {
	oids := make(map[string]struct{})
	if len(locks) == 0 {
		return oids, nil
	}

	seen := make(map[string]struct{})
	var queries []string
	add := func(query string) {
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		queries = append(queries, query)
	}
	for _, lk := range locks {
		p := sanitizeLockPath(lk.Path)
		if p == "" {
			continue
		}
		if lk.Refname != "" {
			add(lk.Refname + ":" + p)
		}
		add("HEAD:" + p)
		for _, ref := range refs {
			add(ref + ":" + p)
		}
	}
	if len(queries) == 0 {
		return oids, nil
	}

	catIn := makePipe()
	catOut := makePipe()

	var catErr error
	var catStderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer catIn.closeWrite()
		for _, q := range queries {
			if _, err := catIn.w.Write([]byte(q + "\x00")); err != nil {
				_ = catIn.closeWriteWith(err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer catIn.closeRead()
		defer catOut.closeWrite()
		err := gitm.NewCommandWithContext(ctx, "cat-file", "--batch", "-z").
			WithTimeout(-1).
			RunInDirWithOptions(repoPath, gitm.RunInDirOptions{
				Stdin:  catIn.r,
				Stdout: catOut.w,
				Stderr: &catStderr,
			})
		if err != nil {
			catErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(catStderr.String()))
			_ = catOut.closeWriteWith(err)
		}
	}()

	parseErr := parseBatchNullDelimited(ctx, catOut.r, oids)
	wg.Wait()

	if catErr != nil {
		return nil, fmt.Errorf("git cat-file: %w", catErr)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("read git cat-file output: %w", parseErr)
	}
	return oids, nil
}

// parseBatch parses newline-delimited `git cat-file --batch` output and adds
// every blob that is a valid LFS pointer to oids. Non-pointer and non-blob
// objects are drained and skipped.
func parseBatch(ctx context.Context, r io.Reader, oids map[string]struct{}) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := br.ReadString('\n')
		if err == io.EOF && line == "" {
			return nil
		}
		line = strings.TrimRight(line, "\n")
		size, isBlob, ok := parseBatchHeader(line)
		if ok && isBlob && size <= maxPointerSize {
			buf := make([]byte, size)
			if _, err := io.ReadFull(br, buf); err != nil {
				return err
			}
			if p, perr := lfs.ReadPointerFromBuffer(buf); perr == nil && p.IsValid() {
				oids[p.Oid] = struct{}{}
			}
		} else if ok {
			// Skip the content of non-pointer blobs and other object types.
			if _, err := io.CopyN(io.Discard, br, size); err != nil {
				return err
			}
		}
		// Existing objects are followed by a single trailing newline;
		// missing/error entries are not.
		if ok {
			if _, err := br.ReadByte(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// parseBatchNullDelimited parses `git cat-file --batch -z` output. The input
// queries are NUL-terminated (making locked paths with newlines safe); on
// output the header line stays newline-terminated, the raw object content is
// unframed, and the usual trailing newline is replaced by a NUL. Missing
// entries are just "<query> missing\n" with no following content.
func parseBatchNullDelimited(ctx context.Context, r io.Reader, oids map[string]struct{}) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := br.ReadString('\n')
		if err == io.EOF && line == "" {
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		header := strings.TrimRight(line, "\n")
		size, isBlob, ok := parseBatchHeader(header)
		if !ok {
			// Missing or unresolvable entry: nothing follows the header.
			if err == io.EOF {
				return nil
			}
			continue
		}
		if isBlob && size <= maxPointerSize {
			buf := make([]byte, size)
			if _, err := io.ReadFull(br, buf); err != nil {
				return err
			}
			if p, perr := lfs.ReadPointerFromBuffer(buf); perr == nil && p.IsValid() {
				oids[p.Oid] = struct{}{}
			}
		} else {
			if _, err := io.CopyN(io.Discard, br, size); err != nil {
				return err
			}
		}
		// Consume the NUL delimiter that follows the content.
		if _, err := br.ReadByte(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err == io.EOF {
			return nil
		}
	}
}

// listBranchAndTagRefs returns the branch and tag reference names in the
// repository. A freshly initialized repository without any refs is reported
// as an empty list rather than an error. for-each-ref exits successfully with
// empty output in that case, and unlike a HEAD-based check it does not assume
// the server's HEAD points at a branch that exists.
func listBranchAndTagRefs(repoPath string) ([]string, error) {
	out := new(bytes.Buffer)
	if err := gitm.NewCommand(
		"for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags",
	).RunInDirWithOptions(repoPath, gitm.RunInDirOptions{Stdout: out}); err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			refs = append(refs, name)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// parseBatchHeader parses a "<name> <type> <size>" cat-file header. The third
// return value is false for missing/ambiguous/error entries, which carry no
// content.
func parseBatchHeader(header string) (size int64, isBlob bool, ok bool) {
	fields := strings.Fields(header)
	if len(fields) != 3 {
		return 0, false, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, false, false
	}
	return size, fields[1] == "blob", true
}

// sanitizeLockPath mirrors the normalization applied when locks are stored.
func sanitizeLockPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	return p
}

// pipe wraps an io.Pipe pair.
type pipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func makePipe() pipe {
	r, w := io.Pipe()
	return pipe{r: r, w: w}
}

func (p pipe) closeRead()  { _ = p.r.Close() }
func (p pipe) closeWrite() { _ = p.w.Close() }
func (p pipe) closeWriteWith(err error) error {
	return p.w.CloseWithError(err)
}
