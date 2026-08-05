package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

const (
	// incidentContextWindow is the half-window around the incident start that
	// co-failures are correlated over.
	incidentContextWindow = 5 * time.Minute
	// incidentContextNameCap bounds how many co-failure names the summary lists.
	incidentContextNameCap = 5
	// incidentContextRowCap bounds the heartbeat rows scanned per summary.
	incidentContextRowCap = 2000
)

// IncidentContext builds the heuristic RCA summary for an auto-incident: which
// other monitors of the project had failing heartbeats within ±5m of the start,
// the dominant probe-error class, and whether everything failed in one region.
// One bounded window query; aggregation happens here so the classification stays
// a pure domain function.
func (s *Store) IncidentContext(ctx context.Context, inc domain.Incident) (domain.IncidentContext, error) {
	out := domain.IncidentContext{WindowMinutes: int(incidentContextWindow.Minutes())}
	from := inc.StartedAt.Add(-incidentContextWindow)
	to := inc.StartedAt.Add(incidentContextWindow)

	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.name, m.region, h.msg, h.code
		  FROM heartbeats h
		  JOIN monitors m ON m.id = h.monitor_id
		 WHERE m.project_id = $1 AND NOT h.up AND h.ts >= $2 AND h.ts < $3
		 LIMIT $4`,
		inc.ProjectID, from, to, incidentContextRowCap)
	if err != nil {
		return out, fmt.Errorf("store: incident context: %w", err)
	}
	defer rows.Close()

	classCount := map[string]int{}
	regions := map[string]bool{}
	coNames := map[string]string{} // monitor id -> name, excluding the incident's own
	for rows.Next() {
		var id, name, region, msg string
		var code int
		if err := rows.Scan(&id, &name, &region, &msg, &code); err != nil {
			return out, fmt.Errorf("store: incident context scan: %w", err)
		}
		classCount[domain.ClassifyProbeError(msg, code)]++
		regions[region] = true
		if id != inc.MonitorID {
			coNames[id] = name
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: incident context rows: %w", err)
	}

	// Dominant class: highest count, ties broken alphabetically (determinism).
	best, bestN := "", 0
	for class, n := range classCount {
		if n > bestN || (n == bestN && class < best) {
			best, bestN = class, n
		}
	}
	out.DominantClass = best

	if len(regions) == 1 {
		for r := range regions {
			out.Region = r
		}
	}

	out.CoFailureTotal = len(coNames)
	names := make([]string, 0, len(coNames))
	for _, n := range coNames {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > incidentContextNameCap {
		names = names[:incidentContextNameCap]
	}
	out.CoFailures = names

	// Dependency graph (D-0100): a down ancestor is the likely root cause.
	if inc.MonitorID != "" {
		if anc, err := s.DownAncestors(ctx, inc.MonitorID); err == nil && len(anc) > 0 {
			out.RootCause = anc[0].Name
		}
	}
	return out, nil
}

// AppendIncidentContext attaches the rendered context as one system-authored
// timeline update. Idempotent: a second call (outbox re-delivery) is a no-op
// because the marker-prefixed update already exists. Returns whether a row was
// inserted. The update carries the incident's current status (timeline entries
// must always name a valid status).
func (s *Store) AppendIncidentContext(ctx context.Context, incidentID, body string) (bool, error) {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO incident_updates (incident_id, status, body, author)
		SELECT i.id, i.status, $2, 'system'
		  FROM incidents i
		 WHERE i.id = $1
		   AND NOT EXISTS (
		       SELECT 1 FROM incident_updates u
		        WHERE u.incident_id = $1 AND u.author = 'system' AND u.body LIKE $3
		   )`,
		incidentID, body, domain.IncidentContextMarker+"%")
	if err != nil {
		return false, fmt.Errorf("store: append incident context: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}
