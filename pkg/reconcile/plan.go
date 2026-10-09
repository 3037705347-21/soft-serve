package reconcile

import (
	"os"
	"sort"
	"time"
)

// ActionType identifies which kind of manifest action a plan item carries.
type ActionType string

const (
	// ActionQuarantine flags a catalog row as quarantined.
	ActionQuarantine ActionType = "quarantine"
	// ActionAdopt registers an on-disk bare repository in the catalog.
	ActionAdopt ActionType = "adopt"
)

// Result is the per-item outcome. Values with the "already" prefix are
// convergent no-ops; conflict marks a precondition violation detected
// before any mutation; failed marks an attempted mutation that errored.
type Result string

const (
	ResultQuarantine         Result = "quarantined"
	ResultAdopt              Result = "adopted"
	ResultAlreadyQuarantined Result = "already_quarantined"
	ResultAlreadyRegistered  Result = "already_registered"
	ResultConflict           Result = "conflict"
	ResultFailed             Result = "failed"
)

// RunStatus is the overall result of a reconciliation run.
type RunStatus string

const (
	StatusSuccess RunStatus = "success"
	StatusPartial RunStatus = "partial"
	StatusFailed  RunStatus = "failed"
)

// CatalogRepo is the minimal catalog row shape the planner needs. Backends
// map their database models onto it.
type CatalogRepo struct {
	Name        string
	Quarantined bool
}

// PlannedAction is one manifest item with its decision precomputed from the
// disk snapshot and catalog rows. The executor performs only items whose
// Result is ResultQuarantine or ResultAdopt; everything else is reported
// untouched.
type PlannedAction struct {
	Type       ActionType
	Name       string
	Result     Result
	Reason     string
	Quarantine *QuarantineItem
	Adopt      *AdoptItem
	// Dir is the resolved on-disk target directory for adopt items.
	Dir string
}

// Actionable reports whether the plan asks the executor to mutate state.
func (a PlannedAction) Actionable() bool {
	return a.Result == ResultQuarantine || a.Result == ResultAdopt
}

// Plan is the read-only reconciliation plan derived from a manifest, a disk
// snapshot, and the current catalog.
type Plan struct {
	// Actions preserves manifest section order (quarantine then adopt).
	Actions []PlannedAction
	// UndeclaredOrphans are bare directories with no catalog row that the
	// manifest does not claim: they must never be touched.
	UndeclaredOrphans []string
	// UnconfirmedMissing are active catalog rows whose directories are
	// absent without a manifest quarantine declaration.
	UnconfirmedMissing []string
	// Aligned counts active catalog rows whose directories are present and
	// are not the subject of an adopt declaration.
	Aligned int
}

// ItemResult is the executed (or dry-run) outcome of one action.
type ItemResult struct {
	Type   ActionType `json:"type"`
	Name   string     `json:"name"`
	Result Result     `json:"result"`
	Reason string     `json:"reason,omitempty"`
}

// Report is the persisted, JSON-serializable result of one run.
type Report struct {
	ManifestPath       string       `json:"manifest_path"`
	DryRun             bool         `json:"dry_run"`
	Status             RunStatus    `json:"status"`
	StartedAt          time.Time    `json:"started_at"`
	FinishedAt         time.Time    `json:"finished_at"`
	Items              []ItemResult `json:"items"`
	UndeclaredOrphans  []string     `json:"undeclared_orphans"`
	UnconfirmedMissing []string     `json:"unconfirmed_missing"`
	Aligned            int          `json:"aligned"`
}

// HasFailures reports whether any item ended in conflict or failed.
func (r *Report) HasFailures() bool {
	for _, item := range r.Items {
		if item.Result == ResultConflict || item.Result == ResultFailed {
			return true
		}
	}
	return false
}

// BuildPlan computes the reconciliation plan without performing any
// mutation. The same inputs always produce the same plan, which is what
// makes restart-and-rerun convergent.
func BuildPlan(m *Manifest, snap *Snapshot, rows []CatalogRepo) *Plan {
	byName := make(map[string]CatalogRepo, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	declaredQ, declaredA := m.ManifestNames()

	plan := &Plan{
		Actions:            make([]PlannedAction, 0, len(m.Quarantine)+len(m.Adopt)),
		UndeclaredOrphans:  []string{},
		UnconfirmedMissing: []string{},
	}

	// Disk-driven discrepancies and aligned count.
	for name := range snap.Repos {
		row, registered := byName[name]
		switch {
		case !registered:
			if _, declared := declaredA[name]; !declared {
				plan.UndeclaredOrphans = append(plan.UndeclaredOrphans, name)
			}
		case row.Quarantined:
			// A quarantined row whose directory is back is neither aligned
			// nor missing; it waits for an explicit admin unquarantine.
		default:
			if _, declared := declaredA[name]; !declared {
				plan.Aligned++
			}
		}
	}

	// Catalog-driven discrepancies: active rows missing on disk without a
	// quarantine declaration.
	for _, row := range rows {
		if row.Quarantined {
			continue
		}
		if _, onDisk := snap.Repos[row.Name]; onDisk {
			continue
		}
		if _, declared := declaredQ[row.Name]; declared {
			continue
		}
		plan.UnconfirmedMissing = append(plan.UnconfirmedMissing, row.Name)
	}

	sort.Strings(plan.UndeclaredOrphans)
	sort.Strings(plan.UnconfirmedMissing)

	// Quarantine decisions.
	for i := range m.Quarantine {
		item := &m.Quarantine[i]
		action := PlannedAction{
			Type:       ActionQuarantine,
			Name:       item.Name,
			Quarantine: item,
		}

		row, ok := byName[item.Name]
		switch {
		case !ok:
			action.Result = ResultConflict
			action.Reason = "repository is not registered in the catalog"
		case row.Quarantined:
			action.Result = ResultAlreadyQuarantined
		default:
			if _, onDisk := snap.Repos[item.Name]; onDisk {
				action.Result = ResultConflict
				action.Reason = "directory still exists on disk; refusing to quarantine a repository that is not lost"
			} else {
				action.Result = ResultQuarantine
			}
		}

		plan.Actions = append(plan.Actions, action)
	}

	// Adopt decisions.
	for i := range m.Adopt {
		item := &m.Adopt[i]
		dir := DefaultRepoDir(snap.ReposRoot, item.Name)
		action := PlannedAction{
			Type:  ActionAdopt,
			Name:  item.Name,
			Adopt: item,
			Dir:   dir,
		}

		row, registered := byName[item.Name]
		switch {
		case registered && row.Quarantined:
			action.Result = ResultConflict
			action.Reason = "a quarantined catalog row already uses this name; unquarantine or delete it first"
		case registered && !IsBareRepo(dir):
			action.Result = ResultConflict
			action.Reason = "repository is already registered but its directory is not a valid bare repository"
		case registered:
			action.Result = ResultAlreadyRegistered
		case !pathExists(dir):
			action.Result = ResultConflict
			action.Reason = "target directory does not exist on disk"
		case !IsBareRepo(dir):
			action.Result = ResultConflict
			action.Reason = "target directory is not a bare git repository"
		default:
			action.Result = ResultAdopt
		}

		plan.Actions = append(plan.Actions, action)
	}

	return plan
}

// FinalizeStatus derives the run status from executed item results. A run
// with no failures is a success; with both successes and failures it is
// partial; with failures and no successful mutation it is failed.
func FinalizeStatus(items []ItemResult) RunStatus {
	var successes, failures int
	for _, item := range items {
		switch item.Result {
		case ResultQuarantine, ResultAdopt:
			successes++
		case ResultConflict, ResultFailed:
			failures++
		}
	}

	if failures == 0 {
		return StatusSuccess
	}
	if successes > 0 {
		return StatusPartial
	}
	return StatusFailed
}

// pathExists reports whether anything exists at the given path.
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
