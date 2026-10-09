package reconcile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/matryer/is"
)

func TestBuildPlanQuarantine(t *testing.T) {
	is := is.New(t)
	root := t.TempDir()

	// "gone" is an active row whose directory is missing.
	// "back" is an active row whose directory is still on disk.
	_, err := git.Init(filepath.Join(root, "back"), true)
	is.NoErr(err)

	m := &Manifest{
		Version: CurrentManifestVersion,
		Quarantine: []QuarantineItem{
			{Name: "gone"},
			{Name: "back"},
			{Name: "was-gone"},
			{Name: "ghost"},
		},
	}
	snap := &Snapshot{ReposRoot: root, Repos: map[string]string{
		"back": filepath.Join(root, "back.git"),
	}}
	rows := []CatalogRepo{
		{Name: "gone"},
		{Name: "back"},
		{Name: "was-gone", Quarantined: true},
	}

	plan := BuildPlan(m, snap, rows)
	results := map[string]Result{}
	for _, a := range plan.Actions {
		results[a.Name] = a.Result
	}
	is.Equal(results["gone"], ResultQuarantine)
	is.Equal(results["back"], ResultConflict)
	is.Equal(results["was-gone"], ResultAlreadyQuarantined)
	is.Equal(results["ghost"], ResultConflict)
}

func TestBuildPlanAdopt(t *testing.T) {
	is := is.New(t)
	root := t.TempDir()

	// "fresh" exists as a bare repo with no row.
	fresh, err := git.Init(filepath.Join(root, "fresh"), true)
	is.NoErr(err)
	// "broken" is a plain directory.
	broken := filepath.Join(root, "broken.git")
	is.NoErr(os.MkdirAll(broken, 0o755))
	// "registered" is a bare repo already cataloged.
	registered, err := git.Init(filepath.Join(root, "registered"), true)
	is.NoErr(err)
	// "quarantined" has a bare dir but its row is quarantined.
	quarantined, err := git.Init(filepath.Join(root, "quarantined"), true)
	is.NoErr(err)

	m := &Manifest{
		Version: CurrentManifestVersion,
		Adopt: []AdoptItem{
			{Name: "fresh", Owner: "admin"},
			{Name: "missing"},
			{Name: "broken"},
			{Name: "registered"},
			{Name: "quarantined"},
		},
	}
	snap := &Snapshot{ReposRoot: root, Repos: map[string]string{
		"fresh":       fresh.Path,
		"broken":      broken,
		"registered":  registered.Path,
		"quarantined": quarantined.Path,
	}}
	rows := []CatalogRepo{
		{Name: "registered"},
		{Name: "quarantined", Quarantined: true},
	}

	plan := BuildPlan(m, snap, rows)
	byName := map[string]PlannedAction{}
	for _, a := range plan.Actions {
		byName[a.Name] = a
	}

	is.Equal(byName["fresh"].Result, ResultAdopt)
	is.Equal(byName["fresh"].Dir, fresh.Path)
	is.Equal(byName["missing"].Result, ResultConflict)
	is.Equal(byName["broken"].Result, ResultConflict)
	is.Equal(byName["registered"].Result, ResultAlreadyRegistered)
	is.Equal(byName["quarantined"].Result, ResultConflict)

	is.True(byName["fresh"].Actionable())
	is.True(!byName["missing"].Actionable())
}

func TestBuildPlanDiscrepancies(t *testing.T) {
	is := is.New(t)
	root := t.TempDir()

	orphan, err := git.Init(filepath.Join(root, "orphan"), true)
	is.NoErr(err)
	healthy, err := git.Init(filepath.Join(root, "healthy"), true)
	is.NoErr(err)

	m := &Manifest{
		Version:    CurrentManifestVersion,
		Quarantine: []QuarantineItem{{Name: "gone-declared"}},
	}
	snap := &Snapshot{ReposRoot: root, Repos: map[string]string{
		"orphan":  orphan.Path,
		"healthy": healthy.Path,
	}}
	rows := []CatalogRepo{
		{Name: "healthy"},
		{Name: "gone-declared"},
		{Name: "gone-silent"},
	}

	plan := BuildPlan(m, snap, rows)

	is.Equal(plan.Aligned, 1)
	is.Equal(len(plan.UndeclaredOrphans), 1)
	is.Equal(plan.UndeclaredOrphans[0], "orphan")
	is.Equal(len(plan.UnconfirmedMissing), 1)
	is.Equal(plan.UnconfirmedMissing[0], "gone-silent")
}

func TestFinalizeStatus(t *testing.T) {
	is := is.New(t)

	is.Equal(FinalizeStatus([]ItemResult{
		{Result: ResultQuarantine},
		{Result: ResultAlreadyQuarantined},
	}), StatusSuccess)

	is.Equal(FinalizeStatus([]ItemResult{
		{Result: ResultAdopt},
		{Result: ResultConflict},
	}), StatusPartial)

	is.Equal(FinalizeStatus([]ItemResult{
		{Result: ResultConflict},
		{Result: ResultFailed},
	}), StatusFailed)
}
