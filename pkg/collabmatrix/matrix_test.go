package collabmatrix_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/collabmatrix"
	"github.com/matryer/is"
)

func matrixLine(repo, username, level string) string {
	return repo + " " + username + " " + level
}

func TestParseValidMatrix(t *testing.T) {
	is := is.New(t)

	content := strings.Join([]string{
		"# repository access matrix",
		"",
		matrixLine("team/api", "Alice", "read-write"),
		"   " + matrixLine("team/api", "bob", "read-only"),
		matrixLine("infra/terraform", "alice", "admin-access"),
	}, "\n")

	matrix, err := collabmatrix.Parse(strings.NewReader(content))
	is.NoErr(err)
	is.Equal(len(matrix.Entries()), 3)

	// Usernames are normalized to lower case; repository names are kept
	// as written; line numbers skip blanks/comments correctly.
	is.Equal(matrix.Entries()[0].Repo, "team/api")
	is.Equal(matrix.Entries()[0].Username, "alice")
	is.Equal(matrix.Entries()[0].Line, 3)
	is.Equal(matrix.Entries()[0].AccessLevel, access.ReadWriteAccess)

	is.Equal(matrix.Entries()[1].Username, "bob")
	is.Equal(matrix.Entries()[1].Line, 4)
	is.Equal(matrix.Entries()[2].AccessLevel, access.AdminAccess)
}

func TestParseEmptyMatrix(t *testing.T) {
	is := is.New(t)

	matrix, err := collabmatrix.Parse(strings.NewReader("# only comments\n\n   \n"))
	is.NoErr(err)
	is.Equal(len(matrix.Entries()), 0)
}

func TestParseMalformedLines(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"too few fields", "team/api alice"},
		{"too many fields", "team/api alice read-write extra"},
		{"invalid username", "team/api alice! read-write"},
		{"username starting with digit", "team/api 1alice read-write"},
		{"unknown access level", "team/api alice write"},
		{"empty repository", "/ alice read-write"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := is.New(t)
			_, err := collabmatrix.Parse(strings.NewReader("# header\n\n" + tt.line + "\n"))
			is.True(err != nil)
			if !strings.Contains(err.Error(), "line 3") {
				t.Fatalf("error %q should report line 3", err.Error())
			}
		})
	}
}

func TestParseDuplicateDeclaration(t *testing.T) {
	is := is.New(t)

	content := strings.Join([]string{
		matrixLine("team/api", "alice", "read-only"),
		matrixLine("team/api", "bob", "read-write"),
		matrixLine("team/api", "alice", "admin-access"),
	}, "\n")

	_, err := collabmatrix.Parse(strings.NewReader(content))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "both lines"))
	is.True(strings.Contains(err.Error(), "alice"))
	is.True(strings.Contains(err.Error(), "team/api"))
}

// The same user declared on different repositories is perfectly legal;
// only the same (repository, user) pair is a duplicate.
func TestParseSameUserDifferentRepos(t *testing.T) {
	is := is.New(t)

	content := strings.Join([]string{
		matrixLine("team/a", "alice", "read-only"),
		matrixLine("team/b", "alice", "read-write"),
	}, "\n")

	matrix, err := collabmatrix.Parse(strings.NewReader(content))
	is.NoErr(err)
	is.Equal(len(matrix.Entries()), 2)
}
