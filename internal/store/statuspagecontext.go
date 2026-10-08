package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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

// PageIncidents is a page's incident context: what is open now, the newest past incidents, and
// whether more past incidents exist than were read (func-status-pages-incidents.md §13.3).
type PageIncidents struct {
	Active []domain.Incident
	Recent []domain.Incident
	// RecentMore is true when the window holds more past incidents than Recent carries. The page
	// links to the incident history only then.
	RecentMore bool
}

// incidentColumnsI is incidentColumns qualified by the alias `i`, for statements that join the
// incident rows to something else (the per-project LATERAL below), where a bare column could be
// ambiguous.
var incidentColumnsI = qualifyColumns("i", incidentColumns)

func qualifyColumns(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for k, c := range parts {
		parts[k] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// pastIncidentsPageSQL reads a page's newest past incidents in history order — resolved_at DESC,
// then id DESC, total so that a cursor is well defined (§13.2). $1 projects, $2 the window's open
// lower bound, $3 its closed upper bound, $4 the row limit.
//
// The read is bounded, not just the result (iter-0203 review R1-5): incidents_resolved_history_idx
// is led by project_id, so across several projects it gives no global order and a plain
// `ANY($1) … ORDER BY … LIMIT` read every matching row to sort them. Here each project yields at
// most $4 rows straight off the index (LATERAL), and only those are merged. `status = 'resolved'`
// is spelled exactly as the index predicate so the planner can use it.
var pastIncidentsPageSQL = `
	SELECT ` + incidentColumnsI + `
	  FROM unnest($1::uuid[]) AS p(pid)
	 CROSS JOIN LATERAL (
	        SELECT * FROM incidents x
	         WHERE x.project_id = p.pid AND x.status = 'resolved'
	           AND x.resolved_at > $2 AND x.resolved_at <= $3
	         ORDER BY x.resolved_at DESC, x.id DESC
	         LIMIT $4) i
	 ORDER BY i.resolved_at DESC, i.id DESC
	 LIMIT $4`

// IncidentsForPage reads the open incidents and the newest `recentLimit` past incidents of MANY
// projects in TWO statements. The window is (since, until]: `since` is now − 90 days and `until`
// the render's now. One row beyond the
// limit is read only to set RecentMore; it is dropped here, so no caller can render or enrich it
// (§13.3, AC-0203-2). The open incidents are not limited: §6 renders every one of them.
func (s *Store) IncidentsForPage(ctx context.Context, projectIDs []string, since, until time.Time, recentLimit int) (PageIncidents, error) {
	out := PageIncidents{Active: []domain.Incident{}, Recent: []domain.Incident{}}
	ids := dedupe(projectIDs)
	if len(ids) == 0 {
		return out, nil
	}
	if recentLimit < 1 {
		return out, fmt.Errorf("store: page incidents: recent limit %d", recentLimit)
	}
	active, err := s.incidentsByPredicate(ctx, s.pool, `
		WHERE i.project_id = ANY($1) AND i.status <> 'resolved'
		 ORDER BY i.started_at DESC`, ids)
	if err != nil {
		return out, err
	}
	recent, err := s.queryIncidents(ctx, s.pool, pastIncidentsPageSQL, ids, since, until, recentLimit+1)
	if err != nil {
		return out, err
	}
	if len(recent) > recentLimit {
		recent, out.RecentMore = recent[:recentLimit], true
	}
	out.Active, out.Recent = active, recent
	return out, nil
}

// HistoryCursor is a position in history order: the (resolved_at, id) of the last incident a page
// returned. The next page starts strictly after it.
type HistoryCursor struct {
	ResolvedAt time.Time
	ID         string
}

// IncidentHistoryPage is one response's worth of a month (§13.4).
type IncidentHistoryPage struct {
	// Counts are the past incidents inside the window per UTC month ("2026-09"). A month with
	// none is absent; the caller lists the offered months and reads a missing one as zero.
	Counts    map[string]int
	Incidents []domain.Incident
	// More is true when the month holds incidents after the last one returned.
	More bool
}

// historyMonthSQL reads the FIRST page of a month in history order, per-project top-K then
// merged, as pastIncidentsPageSQL does and for the same reason. $1 projects, $2 the window's open
// lower bound, $3 its closed upper bound, $4 the month's start (inclusive), $5 its end
// (exclusive), $6 the row limit.
var historyMonthSQL = `
	SELECT ` + incidentColumnsI + `
	  FROM unnest($1::uuid[]) AS p(pid)
	 CROSS JOIN LATERAL (
	        SELECT * FROM incidents x
	         WHERE x.project_id = p.pid AND x.status = 'resolved'
	           AND x.resolved_at > $2 AND x.resolved_at <= $3
	           AND x.resolved_at >= $4 AND x.resolved_at < $5
	         ORDER BY x.resolved_at DESC, x.id DESC
	         LIMIT $6) i
	 ORDER BY i.resolved_at DESC, i.id DESC
	 LIMIT $6`

// historyMonthAfterSQL reads a LATER page of a month: as historyMonthSQL, plus the keyset
// `(resolved_at, id) < ($6, $7)`, then $8 the row limit. It is a separate statement on purpose
// (review round 2): one statement for both pages needed `$6 IS NULL OR (…) < (…)`, which a generic
// plan — what pgx's cached statements reach on their own — cannot use as an index condition, so the
// keyset became a Filter that walked the month from its newest row down to the cursor. A plain
// tuple comparison on the index's (resolved_at, id) is an index condition in custom and generic
// plans alike, and the scan starts AT the cursor.
var historyMonthAfterSQL = `
	SELECT ` + incidentColumnsI + `
	  FROM unnest($1::uuid[]) AS p(pid)
	 CROSS JOIN LATERAL (
	        SELECT * FROM incidents x
	         WHERE x.project_id = p.pid AND x.status = 'resolved'
	           AND x.resolved_at > $2 AND x.resolved_at <= $3
	           AND x.resolved_at >= $4 AND x.resolved_at < $5
	           AND (x.resolved_at, x.id) < ($6::timestamptz, $7::uuid)
	         ORDER BY x.resolved_at DESC, x.id DESC
	         LIMIT $8) i
	 ORDER BY i.resolved_at DESC, i.id DESC
	 LIMIT $8`

// historyCountsSQL counts the window's past incidents per UTC month. Unlike the lists it cannot
// stop early: an exact count reads every past incident of the window, through the partial index
// (resolved_at is in it, so no heap row is needed for the value). That cost is bounded by the
// 90-day window and the page's projects, not by the response size — the contract asks for exact
// per-month counts, and NFR-033's bound is on what is materialized, enriched and served.
const historyCountsSQL = `
	SELECT to_char(i.resolved_at AT TIME ZONE 'UTC', 'YYYY-MM'), count(*)
	  FROM incidents i
	 WHERE i.project_id = ANY($1) AND i.status = 'resolved'
	   AND i.resolved_at > $2 AND i.resolved_at <= $3
	 GROUP BY 1`

// IncidentHistory reads the per-month counts of the window (since, until] and one page of the
// month [monthStart, monthEnd) inside it after `after`, in ONE read-only REPEATABLE READ snapshot,
// so the count shown for a month agrees with the list read with it (§13.4 rule 3). At most `limit`
// incidents are returned; one more is read only to set More.
func (s *Store) IncidentHistory(ctx context.Context, projectIDs []string, since, until, monthStart, monthEnd time.Time,
	after *HistoryCursor, limit int) (IncidentHistoryPage, error) {
	out := IncidentHistoryPage{Counts: map[string]int{}, Incidents: []domain.Incident{}}
	ids := dedupe(projectIDs)
	if len(ids) == 0 {
		return out, nil
	}
	if limit < 1 {
		return out, fmt.Errorf("store: incident history: limit %d", limit)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, fmt.Errorf("store: incident history: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; nothing to commit

	rows, err := tx.Query(ctx, historyCountsSQL, ids, since, until)
	if err != nil {
		return out, fmt.Errorf("store: incident history: counts: %w", err)
	}
	for rows.Next() {
		var month string
		var n int
		if err := rows.Scan(&month, &n); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: incident history: scan count: %w", err)
		}
		out.Counts[month] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: incident history: counts: %w", err)
	}
	if s.historyBetweenReads != nil {
		s.historyBetweenReads()
	}

	var incs []domain.Incident
	if after == nil {
		incs, err = s.queryIncidents(ctx, tx, historyMonthSQL, ids, since, until, monthStart, monthEnd, limit+1)
	} else {
		incs, err = s.queryIncidents(ctx, tx, historyMonthAfterSQL, ids, since, until, monthStart, monthEnd,
			after.ResolvedAt, after.ID, limit+1)
	}
	if err != nil {
		return out, err
	}
	if len(incs) > limit {
		incs, out.More = incs[:limit], true
	}
	out.Incidents = incs
	return out, nil
}

// incidentsByPredicate runs ONE incident query with the caller's WHERE/ORDER, reusing the shared
// column list and scanner so a page cannot decode an incident differently from anywhere else.
func (s *Store) incidentsByPredicate(ctx context.Context, q windowQuerier, predicate string, args ...any) ([]domain.Incident, error) {
	return s.queryIncidents(ctx, q, `SELECT `+incidentColumns+` FROM incidents i `+predicate, args...)
}

// queryIncidents runs a complete statement whose columns are incidentColumns (or incidentColumnsI)
// and scans each row with the shared scanner.
func (s *Store) queryIncidents(ctx context.Context, q windowQuerier, stmt string, args ...any) ([]domain.Incident, error) {
	rows, err := q.Query(ctx, stmt, args...)
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
