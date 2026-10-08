// Package pubkeyroster reconciles user SSH public keys in Soft Serve against a
// single administrator-managed roster file.
//
// The roster is the source of truth for the keys it lists: listed keys are
// added to their users, keys that moved users are reassigned, and keys that
// disappeared are revoked. Only keys previously applied from a roster are
// revoked; public keys added manually and users absent from the roster are
// left untouched. All validation happens before any write and the resulting
// changes are applied in a single database transaction, so an invalid roster
// or an aborted run never leaves partial state, and re-running the same
// roster is a no-op.
package pubkeyroster

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"golang.org/x/crypto/ssh"
)

// ErrInvalidRoster is returned when a roster file cannot be parsed or fails
// roster-level validation (bad line, malformed key, same key listed twice).
var ErrInvalidRoster = errors.New("invalid public key roster")

// Entry is a single "<username> <authorized key>" roster line.
type Entry struct {
	// Line is the 1-based line number in the source roster.
	Line int

	// Username is the lowercased target user.
	Username string

	// Key is the parsed public key.
	Key ssh.PublicKey
}

// Roster is a parsed, validated public key roster.
type Roster struct {
	entries []Entry

	// keyOwner maps the canonical authorized-key form to its roster entry.
	keyOwner map[string]Entry
}

// Entries returns the roster entries in file order.
func (r *Roster) Entries() []Entry {
	return r.entries
}

// Parse reads and validates a roster.
//
// Each non-empty, non-comment line must have the form:
//
//	<username> <key-type> <key-base64> [comment]
//
// where the part after the username is a standard authorized_keys entry.
// Blank lines and lines whose first non-space character is '#' are ignored.
//
// Parsing fails (and therefore the whole reconciliation is aborted before
// touching the database) on a malformed line, an invalid username, an
// unparseable key, or when the same public key is listed more than once,
// including when it is assigned to two different users.
func Parse(r io.Reader) (*Roster, error) {
	roster := &Roster{
		keyOwner: make(map[string]Entry),
	}

	scanner := bufio.NewScanner(r)
	// Keys can be long; give the scanner room instead of its 64KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("%w: line %d: expected \"<username> <key-type> <key-base64> [comment]\"", ErrInvalidRoster, lineNo)
		}

		username := strings.ToLower(fields[0])
		if err := utils.ValidateUsername(username); err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrInvalidRoster, lineNo, err)
		}

		pk, _, err := sshutils.ParseAuthorizedKey(strings.Join(fields[1:], " "))
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: malformed authorized key: %w", ErrInvalidRoster, lineNo, err)
		}

		entry := Entry{
			Line:     lineNo,
			Username: username,
			Key:      pk,
		}

		canonical := sshutils.MarshalAuthorizedKey(pk)
		if first, ok := roster.keyOwner[canonical]; ok {
			if first.Username == username {
				return nil, fmt.Errorf("%w: public key %s is listed for user %q on both lines %d and %d",
					ErrInvalidRoster, ssh.FingerprintSHA256(pk), username, first.Line, lineNo)
			}
			return nil, fmt.Errorf("%w: public key %s is assigned to two users: %q (line %d) and %q (line %d)",
				ErrInvalidRoster, ssh.FingerprintSHA256(pk), first.Username, first.Line, username, lineNo)
		}

		roster.keyOwner[canonical] = entry
		roster.entries = append(roster.entries, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRoster, err)
	}

	return roster, nil
}
