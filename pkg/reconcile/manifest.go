// Package reconcile implements the controlled storage reconciliation used
// after data-disk migrations: it parses an operator-supplied manifest,
// scans the on-disk repositories root, and computes an alignment plan
// between catalog rows and bare repository directories.
//
// The package is deliberately storage-agnostic: it knows nothing about the
// database or the backend. Callers feed it catalog data as plain values and
// execute the plan themselves, which keeps the rules unit-testable and free
// of side effects.
package reconcile

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/utils"
	"gopkg.in/yaml.v3"
)

// CurrentManifestVersion is the only manifest schema version accepted.
const CurrentManifestVersion = 1

// ErrInvalidManifest is returned when a manifest fails validation. Callers
// can use errors.Is to distinguish validation problems from read/parse I/O
// errors.
var ErrInvalidManifest = errors.New("invalid reconciliation manifest")

// Manifest is the operator-supplied migration manifest. Only repository
// names explicitly listed in Quarantine may be flagged, and only on-disk
// directories explicitly listed in Adopt may be registered.
type Manifest struct {
	// Version is the manifest schema version. It must equal
	// CurrentManifestVersion.
	Version int `yaml:"version"`
	// Quarantine lists catalog names whose on-disk directories are
	// confirmed lost.
	Quarantine []QuarantineItem `yaml:"quarantine"`
	// Adopt lists bare repository directories on the target disk that
	// must be (re)registered in the catalog.
	Adopt []AdoptItem `yaml:"adopt"`
}

// QuarantineItem is a single entry of the manifest quarantine section.
type QuarantineItem struct {
	// Name is the catalog repository name.
	Name string `yaml:"name"`
	// Reason is optional audit text persisted only in the report.
	Reason string `yaml:"reason"`
}

// AdoptItem is a single entry of the manifest adopt section.
type AdoptItem struct {
	// Name is the catalog name the directory is registered under.
	Name string `yaml:"name"`
	// Path optionally pins the directory relative to the repositories
	// root. When empty it defaults to "<name>.git". When provided it must
	// resolve to exactly the conventional "<name>.git" location, because
	// the serving layer derives the directory from the catalog name; this
	// makes the field an explicit, path-traversal-safe assertion.
	Path string `yaml:"path"`
	// Owner optionally names the owning user. It defaults to the
	// lowest-ID administrator when empty.
	Owner string `yaml:"owner"`
	// ProjectName is the optional display project name.
	ProjectName string `yaml:"project_name"`
	// Description is the optional repository description.
	Description string `yaml:"description"`
	// Private marks the registered repository private.
	Private bool `yaml:"private"`
	// Hidden marks the registered repository hidden.
	Hidden bool `yaml:"hidden"`
	// Mirror records the mirror flag. No remote URL is configured here.
	Mirror bool `yaml:"mirror"`
}

// LoadManifest reads, parses and validates a manifest from disk. On success
// item names and paths are normalized in place.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: parse yaml: %w", ErrInvalidManifest, err)
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}

	return &m, nil
}

// Validate applies all manifest-wide rules and normalizes names/paths. It
// aggregates every problem rather than failing on the first.
func (m *Manifest) Validate() error {
	var errs []error

	if m.Version != CurrentManifestVersion {
		errs = append(errs, fmt.Errorf("%w: unsupported version %d, expected %d",
			ErrInvalidManifest, m.Version, CurrentManifestVersion))
	}

	seen := make(map[string]struct{})
	remember := func(section, name string) {
		key := section + "\x00" + name
		if _, ok := seen[key]; ok {
			errs = append(errs, fmt.Errorf("%w: duplicate %s entry %q", ErrInvalidManifest, section, name))
			return
		}
		seen[key] = struct{}{}
	}
	cross := make(map[string]string)

	for i := range m.Quarantine {
		item := &m.Quarantine[i]
		name, err := normalizeName(item.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: quarantine[%d]: %w", ErrInvalidManifest, i, err))
			continue
		}
		item.Name = name
		remember("quarantine", name)
		if other, ok := cross[name]; ok && other != "quarantine" {
			errs = append(errs, fmt.Errorf("%w: %q appears in both quarantine and adopt sections", ErrInvalidManifest, name))
		}
		cross[name] = "quarantine"
	}

	for i := range m.Adopt {
		item := &m.Adopt[i]
		name, err := normalizeName(item.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: adopt[%d]: %w", ErrInvalidManifest, i, err))
		} else {
			item.Name = name
			remember("adopt", name)
			if other, ok := cross[name]; ok && other != "adopt" {
				errs = append(errs, fmt.Errorf("%w: %q appears in both quarantine and adopt sections", ErrInvalidManifest, name))
			}
			cross[name] = "adopt"
		}

		if item.Path != "" {
			p, err := normalizeRelativePath(item.Path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%w: adopt[%d] (%s): %w", ErrInvalidManifest, i, item.Name, err))
				continue
			}
			item.Path = p
			if name != "" {
				if want := name + ".git"; p != want {
					errs = append(errs, fmt.Errorf("%w: adopt[%d] (%s): path %q must resolve to the conventional %q location",
						ErrInvalidManifest, i, name, p, want))
				}
			}
		}
	}

	return errors.Join(errs...)
}

// ManifestNames returns the names declared in each section. It is used by the
// executor to exclude explicitly declared entries from discrepancy lists.
func (m *Manifest) ManifestNames() (quarantine map[string]struct{}, adopt map[string]struct{}) {
	quarantine = make(map[string]struct{}, len(m.Quarantine))
	for _, item := range m.Quarantine {
		quarantine[item.Name] = struct{}{}
	}
	adopt = make(map[string]struct{}, len(m.Adopt))
	for _, item := range m.Adopt {
		adopt[item.Name] = struct{}{}
	}
	return quarantine, adopt
}

// normalizeName sanitizes and validates a manifest repository name.
func normalizeName(raw string) (string, error) {
	name := utils.SanitizeRepo(raw)
	if err := utils.ValidateRepo(name); err != nil {
		return "", err
	}
	return name, nil
}

// normalizeRelativePath validates a repositories-root-relative path: it must
// be non-absolute, contain no ".." components after cleaning, and use the
// same character set as repository names plus separators and the .git
// suffix. The cleaned slash form is returned.
func normalizeRelativePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("path cannot be empty")
	}
	if filepath.IsAbs(p) || path.IsAbs(p) {
		return "", errors.New("path must be relative to the repositories root")
	}

	// Normalize OS separators (Windows) to slash form before cleaning.
	p = filepath.ToSlash(p)
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned == "." {
		return "", fmt.Errorf("path %q escapes the repositories root", raw)
	}

	rel := strings.TrimSuffix(cleaned, ".git")
	if err := utils.ValidateRepo(rel); err != nil {
		return "", fmt.Errorf("path %q: %w", raw, err)
	}

	return cleaned, nil
}
