package fileprovider

import (
	"reflect"
	"sort"
)

// Action is one monitor's reconcile outcome within a bundle plan (spec §9 step 8).
type Action string

const (
	ActionCreate           Action = "create"
	ActionUpdate           Action = "update"            // semantic change → D-0142 config write (bumps execution_revision)
	ActionDependencyUpdate Action = "dependency_update" // dep graph only → NO revision bump (D-0142)
	ActionNoop             Action = "noop"              // canonical hash + deps unchanged → NO write
	ActionOrphan           Action = "orphan"            // owned UID absent from a valid bundle → mark/disable after grace
	ActionRestore          Action = "restore"           // previously orphaned UID reappears → reuse DB identity
)

// CurrentMonitor is the provider-owned current state for one UID, supplied by the store at
// apply time. Keeping it an input keeps Plan pure and unit-testable without a database.
type CurrentMonitor struct {
	UID       string
	Type      string   // for the immutable-type guard (§6.2)
	Hash      string   // last applied canonical hash
	DependsOn []string // last applied dependency UIDs (sorted+deduped)
	Orphaned  bool     // currently orphaned/disabled by a prior absence
}

// PlanEntry is the resolved action for one UID plus flags the apply path needs to honor the
// D-0142 write contract (semantic change bumps revision; a dependency-only change does not).
type PlanEntry struct {
	UID              string
	Action           Action
	SemanticChange   bool
	DependencyChange bool
}

// ProjectPlan is the deterministic, whole-bundle plan for one resolved tenant. Entries are
// sorted by UID so lock acquisition and application order are stable across replicas.
type ProjectPlan struct {
	Organization string
	Project      string
	Entries      []PlanEntry
}

// Counts returns per-action tallies for metrics/logs (bounded enum, never per-UID labels).
func (p *ProjectPlan) Counts() map[Action]int {
	c := map[Action]int{}
	for _, e := range p.Entries {
		c[e.Action]++
	}
	return c
}

// HasChanges reports whether the plan mutates anything (anything but noop).
func (p *ProjectPlan) HasChanges() bool {
	for _, e := range p.Entries {
		if e.Action != ActionNoop {
			return true
		}
	}
	return false
}

// Plan computes the deterministic reconcile plan comparing the desired bundle ONLY against
// the provider's own current managed set (never all project monitors — spec §8). It is pure:
// no I/O, no clock. A monitor whose type would change for an existing UID rejects the whole
// bundle (§6.2). Absent-from-desired owned UIDs become orphan candidates; the grace timer
// and physical effects are applied later, never here.
func Plan(desired *DesiredProject, current []CurrentMonitor) (*ProjectPlan, error) {
	cur := make(map[string]CurrentMonitor, len(current))
	for _, c := range current {
		cur[c.UID] = c
	}

	plan := &ProjectPlan{Organization: desired.Organization, Project: desired.Project}

	// Desired monitors: create / update / dependency_update / noop / restore.
	for _, uid := range sortedUIDs(desired) {
		dm := desired.Monitors[uid]
		c, owned := cur[uid]
		if !owned {
			plan.Entries = append(plan.Entries, PlanEntry{UID: uid, Action: ActionCreate, SemanticChange: true})
			continue
		}
		if c.Type != "" && c.Type != string(dm.Monitor.Type) {
			return nil, rejectf(ReasonTypeChange, uid, "monitor type is immutable (was %q, bundle declares %q); use a new UID", c.Type, dm.Monitor.Type)
		}
		semantic := c.Hash != dm.Hash
		deps := !sameSet(c.DependsOn, dm.DependsOn)
		switch {
		case c.Orphaned:
			plan.Entries = append(plan.Entries, PlanEntry{UID: uid, Action: ActionRestore, SemanticChange: semantic, DependencyChange: deps})
		case semantic:
			plan.Entries = append(plan.Entries, PlanEntry{UID: uid, Action: ActionUpdate, SemanticChange: true, DependencyChange: deps})
		case deps:
			plan.Entries = append(plan.Entries, PlanEntry{UID: uid, Action: ActionDependencyUpdate, DependencyChange: true})
		default:
			plan.Entries = append(plan.Entries, PlanEntry{UID: uid, Action: ActionNoop})
		}
	}

	// Owned UIDs absent from the desired bundle → orphan candidates (skip the already-orphaned;
	// re-orphaning is a no-op the apply path need not touch).
	var orphans []string
	for uid, c := range cur {
		if _, present := desired.Monitors[uid]; present {
			continue
		}
		if c.Orphaned {
			continue
		}
		orphans = append(orphans, uid)
	}
	sort.Strings(orphans)
	for _, uid := range orphans {
		plan.Entries = append(plan.Entries, PlanEntry{UID: uid, Action: ActionOrphan})
	}

	// Final deterministic order by UID for stable lock/apply order.
	sort.Slice(plan.Entries, func(i, j int) bool { return plan.Entries[i].UID < plan.Entries[j].UID })
	return plan, nil
}

// sameSet compares two already-normalized (sorted+deduped) string sets.
func sameSet(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
