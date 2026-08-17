package domain

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// ImpactMarker prefixes the system-authored impact note on an incident's
// timeline — the 🕸 member of the ⚡/⏸ system-note family. Unlike those two it
// is NOT the idempotency key: link-row insertion is (one note per batch of
// NEWLY inserted links, so a late root legitimately earns a second note while
// a redelivery earns none — FR-021 §14.3).
const ImpactMarker = "🕸 Impact:"

// Impact roles (FR-021 §14.3). probable_root marks EVERY upstream service on a
// path with an open incident — the relation records candidates, it never
// elects a single culprit; ranking is presentation. affected marks downstream
// services with open incidents.
const (
	ImpactProbableRoot = "probable_root"
	ImpactAffected     = "affected"
)

// ServiceGraphDepthCap bounds every service-graph traversal (validation and
// correlation). A cap hit REJECTS a write — it never silently truncates a
// cycle check (§14.1); stored paths are endpoint-inclusive and therefore at
// most ServiceGraphDepthCap+1 slugs long (invariant 55).
const ServiceGraphDepthCap = 10

// MaxServiceDependencies bounds a service's DIRECT upstream edges. Fixed in
// phase 3 (the min_decidable_coverage pattern): not operator-settable.
const MaxServiceDependencies = 20

// MaxCorrelationWitnessesPerService bounds, per endpoint service, how many OPEN
// monitor-anchored incidents a correlation attempt selects as witnesses —
// ascending (started_at, id), oldest first, a deterministic function of the
// committed state at attempt time (review [276] P1-3 / [278] conditions).
// Service-level completeness is unchanged (a service with ANY open incident is
// still marked); the bound truncates witness lists, capping the attempt's lock
// and write set by construction. Overflow is returned, logged and counted —
// never silent. Fixed, not operator-settable.
const MaxCorrelationWitnessesPerService = 5

// impactNoteNameCap bounds how many service names the 🕸 prose lists per role;
// the RELATION stays complete, only the rendering truncates, and the
// truncation names its remainder (§14.7).
const impactNoteNameCap = 8

// ServiceImpactLink is one structured incident↔service link. Path is the
// canonical ROOT-FIRST, endpoint-inclusive slug sequence (shortest, then
// lexicographic tie-break) — the SAME array on a probable_root row and its
// affected counterpart; the role says which endpoint is the row's own service.
type ServiceImpactLink struct {
	ServiceID  string    `json:"service_id"`
	Slug       string    `json:"slug"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Path       []string  `json:"path"`
	ComputedAt time.Time `json:"computed_at"`
}

// RenderImpactNote renders the 🕸 system note for one batch of newly inserted
// links, bounded per role. Ordering inside a role is by path length (nearest
// first), then slug — deterministic, matching the UI's presentation ranking.
// Empty batch → empty string (the caller writes no note).
func RenderImpactNote(links []ServiceImpactLink) string {
	var roots, affected []ServiceImpactLink
	for _, l := range links {
		switch l.Role {
		case ImpactProbableRoot:
			roots = append(roots, l)
		case ImpactAffected:
			affected = append(affected, l)
		}
	}
	if len(roots) == 0 && len(affected) == 0 {
		return ""
	}
	part := func(ls []ServiceImpactLink, via bool) string {
		sort.Slice(ls, func(i, j int) bool {
			if len(ls[i].Path) != len(ls[j].Path) {
				return len(ls[i].Path) < len(ls[j].Path)
			}
			return ls[i].Slug < ls[j].Slug
		})
		names := make([]string, 0, len(ls))
		for i, l := range ls {
			if i == impactNoteNameCap {
				break
			}
			if via && len(l.Path) > 0 {
				names = append(names, l.Slug+" (via "+strings.Join(l.Path, " → ")+")")
			} else {
				names = append(names, l.Slug)
			}
		}
		out := strings.Join(names, ", ")
		if extra := len(ls) - impactNoteNameCap; extra > 0 {
			out += ", +" + strconv.Itoa(extra) + " more"
		}
		return out
	}
	segs := make([]string, 0, 2)
	if len(roots) > 0 {
		segs = append(segs, "probable root — "+part(roots, true))
	}
	if len(affected) > 0 {
		segs = append(segs, "affected — "+part(affected, false))
	}
	return ImpactMarker + " " + strings.Join(segs, "; ") + "."
}
