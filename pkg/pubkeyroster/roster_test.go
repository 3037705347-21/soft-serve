package pubkeyroster_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/pubkeyroster"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

func genPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func rosterLine(username string, pk ssh.PublicKey, comment string) string {
	line := username + " " + sshutils.MarshalAuthorizedKey(pk)
	if comment != "" {
		line += " " + comment
	}
	return line
}

func TestParseValidRoster(t *testing.T) {
	is := is.New(t)

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	k3 := genPublicKey(t)
	content := strings.Join([]string{
		"# team key roster",
		"",
		rosterLine("Alice", k1, "laptop"),
		"   " + rosterLine("bob", k2, ""),
		rosterLine("alice", k3, "second key"),
	}, "\n")

	roster, err := pubkeyroster.Parse(strings.NewReader(content))
	is.NoErr(err)
	is.Equal(len(roster.Entries()), 3)

	// Usernames are normalized to lower case and inline comments are
	// accepted as part of the authorized key line.
	is.Equal(roster.Entries()[0].Username, "alice")
	is.Equal(roster.Entries()[0].Line, 3)
	is.Equal(roster.Entries()[1].Username, "bob")
	is.Equal(roster.Entries()[1].Line, 4)
	is.True(sshutils.KeysEqual(roster.Entries()[0].Key, k1))
}

func TestParseEmptyRoster(t *testing.T) {
	is := is.New(t)

	roster, err := pubkeyroster.Parse(strings.NewReader("# only comments\n\n   \n"))
	is.NoErr(err)
	is.Equal(len(roster.Entries()), 0)
}

func TestParseMalformedLines(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"too few fields", "alice ssh-ed25519"},
		{"malformed key blob", "alice ssh-ed25519 not-a-key!!"},
		{"invalid username", "alice! ssh-ed25519 AAAAC3Nza"},
		{"username starting with digit", "1alice ssh-ed25519 AAAAC3Nza"},
		{"unsupported key type", "alice ssh-weird AAAAC3Nza"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := is.New(t)
			_, err := pubkeyroster.Parse(strings.NewReader("# header\n\n" + tt.line + "\n"))
			is.True(err != nil)
			if !strings.Contains(err.Error(), "line 3") {
				t.Fatalf("error %q should report line 3", err.Error())
			}
		})
	}
}

func TestParseDuplicateKeyTwoUsers(t *testing.T) {
	is := is.New(t)

	k1 := genPublicKey(t)
	k2 := genPublicKey(t)
	content := strings.Join([]string{
		rosterLine("alice", k1, ""),
		rosterLine("bob", k2, ""),
		rosterLine("carol", k1, "same key as alice"),
	}, "\n")

	_, err := pubkeyroster.Parse(strings.NewReader(content))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "assigned to two users"))
	is.True(strings.Contains(err.Error(), "alice"))
	is.True(strings.Contains(err.Error(), "carol"))
}

func TestParseDuplicateKeySameUser(t *testing.T) {
	is := is.New(t)

	k1 := genPublicKey(t)
	content := strings.Join([]string{
		rosterLine("alice", k1, "laptop"),
		rosterLine("alice", k1, "different comment"),
	}, "\n")

	_, err := pubkeyroster.Parse(strings.NewReader(content))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "both lines"))
}
