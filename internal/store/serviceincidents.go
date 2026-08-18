package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Service incidents (FR-022, D-0170). A Service can own an incident, and this file owns the two things
// that makes structurally different from a monitor incident: opening one INSIDE a caller's transaction
// (so the incident and the announcement that caused it are one commit — FR-022 invariant 7), and the
// member snapshot a postmortem reads after the world has moved.
//
// What is deliberately NOT here: any change to how a monitor incident opens, resolves or renders. NFR-017
// is the requirement that adding the second anchor changes no answer about the first, and the way to keep
// that promise is to add a path rather than to generalize the existing one.

// OpenServiceIncidentTx opens an auto-incident anchored to a service, inside the caller's transaction,
// and snapshots the service's member set as of that instant.
//
// It returns the incident and whether it was CREATED. A second open for a service that already has one is
// not an error: the partial unique index `incidents_service_open_auto_idx` is the arbiter, and a
// concurrent evaluator losing that race has done nothing wrong — one incident is open, which is the
// invariant (FR-022 invariant 4). The caller decides what to do with `created == false`; the evaluator
// uses it to avoid announcing twice.
func (s *Store) OpenServiceIncidentTx(
	ctx context.Context, tx pgx.Tx, serviceID, projectID, title string, confirmedOver int,
) (domain.Incident, bool, error) {
	row := tx.QueryRow(ctx,
		`INSERT INTO incidents (project_id, service_id, title, status, impact, source)
		 VALUES ($1, $2, $3, 'investigating', 'major', 'auto')
		 ON CONFLICT (service_id) WHERE source = 'auto' AND status <> 'resolved' AND service_id IS NOT NULL
		 DO NOTHING
		 RETURNING `+incidentColumns, projectID, serviceID, title)
	inc, err := scanIncident(row)
	if err != nil {
		if noRows(err) {
			// The index refused it: an open auto-incident for this service already exists. Read it, so the
			// caller can annotate the one that is open instead of believing it opened a new one.
			existing, ferr := s.FindOpenAutoIncidentByService(ctx, tx, serviceID)
			if ferr != nil {
				return domain.Incident{}, false, ferr
			}
			return existing, false, nil
		}
		return domain.Incident{}, false, fmt.Errorf("store: open service incident: %w", err)
	}

	if err := snapshotServiceMembersTx(ctx, tx, inc.ID, projectID, serviceID); err != nil {
		return domain.Incident{}, false, err
	}
	// The opening update states WHAT confirmed it. An operator who finds an incident nobody typed must be
	// able to see the machine that opened it and why — and `confirmedOver` is the same number that governs
	// whether the service pages at all (§16.3).
	body := fmt.Sprintf("Opened automatically: service DOWN confirmed over %d evaluation(s).", confirmedOver)
	if _, err := tx.Exec(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author) VALUES ($1, 'investigating', $2, 'system')`,
		inc.ID, body); err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: open service incident note: %w", err)
	}
	return inc, true, nil
}

// FindOpenAutoIncidentByService reads the currently-open auto-incident for a service, or ErrNotFound.
// It takes a queryer so it can run inside the opening transaction or on the pool.
func (s *Store) FindOpenAutoIncidentByService(ctx context.Context, q dbConn, serviceID string) (domain.Incident, error) {
	row := q.QueryRow(ctx,
		`SELECT `+incidentColumns+`
		   FROM incidents
		  WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved'
		  ORDER BY started_at DESC LIMIT 1`, serviceID)
	inc, err := scanIncident(row)
	if err != nil {
		if noRows(err) {
			return domain.Incident{}, ErrNotFound
		}
		return domain.Incident{}, fmt.Errorf("store: find open service incident: %w", err)
	}
	return inc, nil
}

// snapshotServiceMembersTx stores the service's declared members AS OF this instant (FR-022 invariant 13,
// spec D6). A postmortem is read after the world moved — a member may be renamed, dropped from the
// declaration or deleted outright — and a live join would then render "3 members" it cannot name. Same
// device as phase 5's immutable recipient snapshot, for the same reason.
func snapshotServiceMembersTx(ctx context.Context, tx pgx.Tx, incidentID, projectID, serviceID string) error {
	// `service_definition_members` already carries `monitor_name` as of the declaration — the effective
	// revision IS a snapshot of the membership, which is why this reads it instead of joining `monitors`:
	// a join would render an empty name for a member deleted since, and naming the members after the world
	// moved is the entire purpose of this table.
	rows, err := tx.Query(ctx, `
		SELECT mem.monitor_id::text, min(mem.monitor_name), array_agg(mem.role ORDER BY mem.role)
		  FROM service_definition_members mem
		 WHERE mem.revision_id = (
		       SELECT id FROM service_definition_revisions
		        WHERE service_id = $1 AND state = 'effective'
		        ORDER BY effective_at DESC LIMIT 1)
		 GROUP BY mem.monitor_id
		 ORDER BY min(mem.monitor_name)`, serviceID)
	if err != nil {
		return fmt.Errorf("store: read members for snapshot: %w", err)
	}
	defer rows.Close()
	members := []domain.IncidentMember{}
	for rows.Next() {
		var m domain.IncidentMember
		if err := rows.Scan(&m.MonitorID, &m.Name, &m.Roles); err != nil {
			return fmt.Errorf("store: scan member for snapshot: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate members for snapshot: %w", err)
	}
	payload, err := json.Marshal(members)
	if err != nil {
		return fmt.Errorf("store: encode member snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO incident_member_snapshots (incident_id, project_id, members)
		 VALUES ($1, $2, $3::jsonb)
		 ON CONFLICT (incident_id) DO NOTHING`, incidentID, projectID, payload); err != nil {
		return fmt.Errorf("store: write member snapshot: %w", err)
	}
	return nil
}

// IncidentMemberSnapshot returns the members an incident's service had when it opened. An empty slice and
// a missing snapshot are DIFFERENT answers: a service can genuinely have had no members, and the caller
// must be able to tell that from "this incident has no snapshot" (a monitor or project-level incident).
func (s *Store) IncidentMemberSnapshot(ctx context.Context, incidentID string) ([]domain.IncidentMember, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT members FROM incident_member_snapshots WHERE incident_id = $1`, incidentID).Scan(&raw)
	if err != nil {
		if noRows(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("store: read member snapshot: %w", err)
	}
	var out []domain.IncidentMember
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("store: decode member snapshot: %w", err)
	}
	return out, true, nil
}
