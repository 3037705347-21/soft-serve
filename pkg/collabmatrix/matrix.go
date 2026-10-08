// Package collabmatrix reconciles repository collaborators in Soft Serve
// against a single administrator-managed access matrix file.
//
// The matrix is the source of truth for the collaborators it lists: listed
// users get the declared access level, levels of already managed
// collaborators are changed, and managed collaborators that disappeared are
// removed. Only collaborators previously applied from a matrix are revoked;
// collaborators added manually, repository owners and repositories absent
// from the matrix are left untouched. All validation happens before any
// write and the resulting changes are applied in a single database
// transaction, so an invalid matrix or an aborted run never leaves partial
// state, and re-running the same matrix is a no-op.
package collabmatrix

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// ErrInvalidMatrix is returned when a matrix file cannot be parsed or fails
// matrix-level validation (bad line, invalid username, unknown access
// level, same user listed twice for the same repository).
var ErrInvalidMatrix = errors.New("invalid collaborator access matrix")

// Entry is a single "<repository> <username> <access-level>" matrix line.
type Entry struct {
	// Line is the 1-based line number in the source matrix.
	Line int

	// Repo is the sanitized target repository name.
	Repo string

	// Username is the lowercased target user.
	Username string

	// AccessLevel is the declared access level.
	AccessLevel access.AccessLevel
}

// Matrix is a parsed, validated collaborator access matrix.
type Matrix struct {
	entries []Entry

	// declarations maps "repo\x00username" to the first entry declaring it.
	declarations map[string]Entry
}

// Entries returns the matrix entries in file order.
func (m *Matrix) Entries() []Entry {
	return m.entries
}

func declarationKey(repo, username string) string {
	return repo + "\x00" + username
}

// Parse reads and validates an access matrix.
//
// Each non-empty, non-comment line must have the form:
//
//	<repository> <username> <access-level>
//
// where access-level is one of: no-access, read-only, read-write,
// admin-access. Blank lines and lines whose first non-space character is '#'
// are ignored.
//
// Parsing fails (and therefore the whole reconciliation is aborted before
// touching the database) on a malformed line, an invalid repository or
// username, an unknown access level, or when the same user is declared more
// than once for the same repository.
func Parse(r io.Reader) (*Matrix, error) {
	matrix := &Matrix{
		declarations: make(map[string]Entry),
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: line %d: expected \"<repository> <username> <access-level>\"", ErrInvalidMatrix, lineNo)
		}

		repo := utils.SanitizeRepo(fields[0])
		if repo == "" {
			return nil, fmt.Errorf("%w: line %d: repository name cannot be empty", ErrInvalidMatrix, lineNo)
		}

		username := strings.ToLower(fields[1])
		if err := utils.ValidateUsername(username); err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrInvalidMatrix, lineNo, err)
		}

		level := access.ParseAccessLevel(fields[2])
		if level < 0 {
			return nil, fmt.Errorf("%w: line %d: unknown access level %q (valid values: no-access, read-only, read-write, admin-access)",
				ErrInvalidMatrix, lineNo, fields[2])
		}

		entry := Entry{
			Line:        lineNo,
			Repo:        repo,
			Username:    username,
			AccessLevel: level,
		}

		key := declarationKey(repo, username)
		if first, ok := matrix.declarations[key]; ok {
			return nil, fmt.Errorf("%w: user %q is declared for repository %q on both lines %d and %d",
				ErrInvalidMatrix, username, repo, first.Line, lineNo)
		}

		matrix.declarations[key] = entry
		matrix.entries = append(matrix.entries, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMatrix, err)
	}

	return matrix, nil
}
