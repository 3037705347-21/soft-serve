package reconcile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
)

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadManifestValid(t *testing.T) {
	is := is.New(t)

	p := writeManifest(t, `
version: 1
quarantine:
  - name: team/lost
    reason: old disk failed
adopt:
  - name: team/found
    path: team/found.git
    owner: admin
    private: true
    hidden: false
`)
	m, err := LoadManifest(p)
	is.NoErr(err)
	is.Equal(m.Version, 1)
	is.Equal(len(m.Quarantine), 1)
	is.Equal(m.Quarantine[0].Name, "team/lost")
	is.Equal(m.Quarantine[0].Reason, "old disk failed")
	is.Equal(len(m.Adopt), 1)
	is.Equal(m.Adopt[0].Name, "team/found")
	is.Equal(m.Adopt[0].Path, "team/found.git")
	is.Equal(m.Adopt[0].Owner, "admin")
	is.Equal(m.Adopt[0].Private, true)
}

func TestLoadManifestValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{
			"yaml syntax error",
			"version: 1\nquarantine: [",
		},
		{
			"missing version",
			"quarantine:\n  - name: a\n",
		},
		{
			"unsupported version",
			"version: 2\n",
		},
		{
			"illegal quarantine name",
			"version: 1\nquarantine:\n  - name: 'has space'\n",
		},
		{
			"duplicate quarantine entry",
			"version: 1\nquarantine:\n  - name: a\n  - name: a\n",
		},
		{
			"duplicate adopt entry",
			"version: 1\nadopt:\n  - name: a\n  - name: a\n",
		},
		{
			"cross section name",
			"version: 1\nquarantine:\n  - name: a\nadopt:\n  - name: a\n",
		},
		{
			"relative path traversal",
			"version: 1\nadopt:\n  - name: a\n    path: ../../etc/a.git\n",
		},
		{
			"absolute path",
			"version: 1\nadopt:\n  - name: a\n    path: /etc/a.git\n",
		},
		{
			"path does not match conventional name",
			"version: 1\nadopt:\n  - name: a\n    path: other.git\n",
		},
		{
			"unknown field",
			"version: 1\nbogus: true\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := is.New(t)
			_, err := LoadManifest(writeManifest(t, tt.contents))
			is.True(err != nil)
			is.True(errors.Is(err, ErrInvalidManifest))
		})
	}
}

func TestLoadManifestMissingFile(t *testing.T) {
	is := is.New(t)
	_, err := LoadManifest(filepath.Join(t.TempDir(), "nope.yaml"))
	is.True(err != nil)
	is.True(!errors.Is(err, ErrInvalidManifest))
}

func TestManifestNames(t *testing.T) {
	is := is.New(t)
	m := &Manifest{
		Version: 1,
		Quarantine: []QuarantineItem{
			{Name: "a"},
		},
		Adopt: []AdoptItem{
			{Name: "b"},
		},
	}
	q, a := m.ManifestNames()
	_, ok := q["a"]
	is.True(ok)
	_, ok = a["b"]
	is.True(ok)
	_, ok = q["b"]
	is.True(!ok)
}
