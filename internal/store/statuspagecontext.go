package store

import (
	"context"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §15.0 — the rest of a status page, batched ([318] P1-1).
//
// The component projections were made page-scoped first, but the render also loops the page's
// projects for incidents and maintenance and then loops the incidents for their timelines and
// postmortems. On an org-level page that is O(projects) + O(incidents) statements on an
// UNAUTHENTICATED surface, which is the same amplification the component work removed — a bound
// that holds for half a request is not a bound.
//
// Four set-wise reads replace all of it, whatever the page spans.

// PageIncidents is a page's incident context: what is open now, and what resolved recently.
type PageIncidents struct {
	Active []domain.Incident
	Recent []domain.Incident
}

// IncidentsForPage reads the open and recently-resolved incidents of MANY projects in TWO
// statements. `since` bounds the resolved set — the render shows 90 days — so the query returns
// what will be displayed rather than everything ever resolved.
func (s *Store) IncidentsForPage(ctx context.Context, projectIDs []string, since time.Time) (PageIncidents, error) {
	out := PageIncidents{Active: []domain.Incident{}, Recent: []domain.Incident{}}
	ids := dedupe(projectIDs)
	if len(ids) == 0 {
		return out, nil
	}
	active, err := s.incidentsByPredicate(ctx, `
		WHERE i.project_id = ANY($1) AND i.status <> 'resolved'
		 ORDER BY i.started_at DESC`, ids)
	if err != nil {
		return out, err
	}
	// Newest first, so the page's "recent" list needs no second sort.
	recent, err := s.incidentsByPredicate(ctx, `
		WHERE i.project_id = ANY($1) AND i.status = 'resolved'
		   AND i.resolved_at IS NOT NULL AND i.resolved_at > $2
		 ORDER BY i.resolved_at DESC`, ids, since)
	if err != nil {
		return out, err
	}
	out.Active, out.Recent = active, recent
	return out, nil
}

// incidentsByPredicate runs ONE incident query with the caller's WHERE/ORDER, reusing the shared
// column list and scanner so a page cannot decode an incident differently from anywhere else.
func (s *Store) incidentsByPredicate(ctx context.Context, predicate string, args ...any) ([]domain.Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+incidentColumns+` FROM incidents i `+predicate, args...)
	if err != nil {
		return nil, fmt.Errorf("store: page incidents: %w", err)
	}
	defer rows.Close()
	out := []domain.Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan page incident: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// MaintenanceForPage reads the ACTIVE and UPCOMING maintenance windows of many projects in ONE
// statement. Archived windows are excluded here exactly as the per-project render excluded them:
// a cancelled window announced as upcoming is downtime that will not happen.
func (s *Store) MaintenanceForPage(ctx context.Context, projectIDs []string, now time.Time) ([]domain.MaintenanceWindow, error) {
	out := []domain.MaintenanceWindow{}
	ids := dedupe(projectIDs)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+maintenanceColumns+`
		  FROM maintenance_windows mw
		 WHERE mw.project_id = ANY($1) AND mw.archived_at IS NULL AND mw.ends_at > $2
		 ORDER BY mw.starts_at`, ids, now)
	if err != nil {
		return nil, fmt.Errorf("store: page maintenance: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		mw, err := scanMaintenance(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan page maintenance: %w", err)
		}
		out = append(out, mw)
	}
	return out, rows.Err()
}

// IncidentTimelines reads the updates of MANY incidents in one statement, grouped by incident and
// ordered exactly as the per-incident read orders them.
func (s *Store) IncidentTimelines(ctx context.Context, incidentIDs []string) (map[string][]domain.IncidentUpdate, error) {
	out := map[string][]domain.IncidentUpdate{}
	ids := dedupe(incidentIDs)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+incidentUpdateColumns+`
		  FROM incident_updates iu WHERE iu.incident_id = ANY($1)
		 ORDER BY iu.incident_id, iu.created_at`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: page incident updates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanIncidentUpdate(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan page incident update: %w", err)
		}
		out[u.IncidentID] = append(out[u.IncidentID], u)
	}
	return out, rows.Err()
}

// PostmortemsForIncidents reads published postmortems for many incidents in one statement. An
// incident without one is simply absent from the map — the same shape the per-incident read's
// ErrNotFound produced, without a statement per incident to discover it.
func (s *Store) PostmortemsForIncidents(ctx context.Context, incidentIDs []string) (map[string]domain.Postmortem, error) {
	out := map[string]domain.Postmortem{}
	ids := dedupe(incidentIDs)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+postmortemColumns+`
		  FROM postmortems p WHERE p.incident_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: page postmortems: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		pm, err := scanPostmortem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan page postmortem: %w", err)
		}
		out[pm.IncidentID] = pm
	}
	return out, rows.Err()
}
