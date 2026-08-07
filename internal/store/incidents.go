package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const incidentColumns = "id, project_id, monitor_id, title, status, impact, source, external_key, started_at, resolved_at, acknowledged_at, acknowledged_by, escalation_step, last_escalated_at, created_at, updated_at"

func scanIncident(row pgx.Row) (domain.Incident, error) {
	var (
		inc         domain.Incident
		monitorID   *string
		externalKey *string
		resolved    *time.Time
		ackedAt     *time.Time
		ackedBy     *string
		lastEsc     *time.Time
	)
	if err := row.Scan(&inc.ID, &inc.ProjectID, &monitorID, &inc.Title, &inc.Status, &inc.Impact,
		&inc.Source, &externalKey, &inc.StartedAt, &resolved, &ackedAt, &ackedBy,
		&inc.EscalationStep, &lastEsc, &inc.CreatedAt, &inc.UpdatedAt); err != nil {
		return domain.Incident{}, err
	}
	inc.LastEscalatedAt = lastEsc
	if monitorID != nil {
		inc.MonitorID = *monitorID
	}
	if externalKey != nil {
		inc.ExternalKey = *externalKey
	}
	inc.ResolvedAt = resolved
	inc.AcknowledgedAt = ackedAt
	if ackedBy != nil {
		inc.AcknowledgedBy = *ackedBy
	}
	return inc, nil
}

// AcknowledgeIncident marks an open incident as acknowledged (stopping escalation),
// idempotently: acknowledging an already-acked incident keeps the first ack. Returns
// the updated incident, or ErrNotFound if it is gone / already resolved.
func (s *Store) AcknowledgeIncident(ctx context.Context, id, by string) (domain.Incident, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE incidents
		    SET acknowledged_at = COALESCE(acknowledged_at, now()),
		        acknowledged_by = COALESCE(acknowledged_by, $2),
		        updated_at = now()
		  WHERE id = $1 AND status <> 'resolved'
		  RETURNING `+incidentColumns,
		id, by)
	inc, err := scanIncident(row)
	if noRows(err) {
		return domain.Incident{}, ErrNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: acknowledge incident: %w", err)
	}
	return inc, nil
}

// CreateIncident inserts an incident together with its opening timeline update in
// one transaction, so every incident has a timeline from the start.
func (s *Store) CreateIncident(ctx context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error) {
	if err := inc.Validate(); err != nil {
		return domain.Incident{}, fmt.Errorf("store: invalid incident: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: begin create incident: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var monitorID, externalKey *string
	if inc.MonitorID != "" {
		monitorID = &inc.MonitorID
	}
	if inc.ExternalKey != "" {
		externalKey = &inc.ExternalKey
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO incidents (project_id, monitor_id, title, status, impact, source, external_key)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+incidentColumns,
		inc.ProjectID, monitorID, inc.Title, inc.Status, inc.Impact, inc.Source, externalKey)
	created, err := scanIncident(row)
	if err != nil {
		// The partial unique index rejects a second open auto-incident for the same
		// monitor — a concurrent down transition raced us. Benign: one is open.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "incidents_one_open_auto" {
			return domain.Incident{}, ErrAlreadyOpen
		}
		return domain.Incident{}, fmt.Errorf("store: create incident: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author) VALUES ($1,$2,$3,$4)`,
		created.ID, created.Status, openingBody, author); err != nil {
		return domain.Incident{}, fmt.Errorf("store: create opening update: %w", err)
	}
	// Enqueue the webhook event in the same transaction — the event is durable iff
	// the incident is (no dual-write).
	payload, err := json.Marshal(domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: created})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: marshal incident event: %w", err)
	}
	if err := enqueueOutboxTx(ctx, tx, domain.TopicIncidentEvent, payload); err != nil {
		return domain.Incident{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Incident{}, fmt.Errorf("store: commit create incident: %w", err)
	}
	return created, nil
}

// GetIncident returns an incident by id, or ErrNotFound.
func (s *Store) GetIncident(ctx context.Context, id string) (domain.Incident, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = $1`, id)
	inc, err := scanIncident(row)
	if noRows(err) {
		return domain.Incident{}, ErrNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: get incident: %w", err)
	}
	return inc, nil
}

// FindOpenAutoIncidentByMonitor returns the currently-open auto-incident for a
// monitor (source auto, not yet resolved), or ErrNotFound. Used to avoid opening
// a duplicate while one is already active.
func (s *Store) FindOpenAutoIncidentByMonitor(ctx context.Context, monitorID string) (domain.Incident, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+incidentColumns+` FROM incidents
		 WHERE monitor_id = $1 AND source = 'auto' AND status <> 'resolved'
		 ORDER BY started_at DESC LIMIT 1`, monitorID)
	inc, err := scanIncident(row)
	if noRows(err) {
		return domain.Incident{}, ErrNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: find open auto incident: %w", err)
	}
	return inc, nil
}

// FindOpenIncidentByExternalKey returns the currently-open incident for a project
// correlated by external key (not yet resolved), or ErrNotFound. Used by the
// Alertmanager receiver to reuse/close the incident a prior "firing" alert opened.
func (s *Store) FindOpenIncidentByExternalKey(ctx context.Context, projectID, key string) (domain.Incident, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+incidentColumns+` FROM incidents
		 WHERE project_id = $1 AND external_key = $2 AND status <> 'resolved'
		 ORDER BY started_at DESC LIMIT 1`, projectID, key)
	inc, err := scanIncident(row)
	if noRows(err) {
		return domain.Incident{}, ErrNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: find open incident by external key: %w", err)
	}
	return inc, nil
}

// ListIncidentsByProject lists a project's incidents, newest first.
func (s *Store) ListIncidentsByProject(ctx context.Context, projectID string) ([]domain.Incident, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+incidentColumns+` FROM incidents WHERE project_id = $1 ORDER BY started_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list incidents: %w", err)
	}
	defer rows.Close()
	var out []domain.Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan incident: %w", err)
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate incidents: %w", err)
	}
	return out, nil
}

const incidentUpdateColumns = "id, incident_id, status, body, author, created_at"

func scanIncidentUpdate(row pgx.Row) (domain.IncidentUpdate, error) {
	var u domain.IncidentUpdate
	if err := row.Scan(&u.ID, &u.IncidentID, &u.Status, &u.Body, &u.Author, &u.CreatedAt); err != nil {
		return domain.IncidentUpdate{}, err
	}
	return u, nil
}

// AddIncidentUpdate appends a timeline update and syncs the incident's status to
// it, stamping resolved_at when the incident first reaches Resolved — all in one
// transaction.
func (s *Store) AddIncidentUpdate(ctx context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error) {
	if err := upd.Validate(); err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: invalid incident update: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: begin add update: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	row := tx.QueryRow(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author)
		 VALUES ($1,$2,$3,$4) RETURNING `+incidentUpdateColumns,
		upd.IncidentID, upd.Status, upd.Body, upd.Author)
	created, err := scanIncidentUpdate(row)
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: add incident update: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE incidents
		    SET status = $2,
		        updated_at = now(),
		        resolved_at = CASE WHEN $2 = 'resolved' AND resolved_at IS NULL THEN now() ELSE resolved_at END
		  WHERE id = $1`,
		upd.IncidentID, upd.Status); err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: sync incident status: %w", err)
	}
	// Load the now-updated incident and enqueue the lifecycle event in the same
	// transaction. The event type follows the update's status.
	evInc, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = $1`, upd.IncidentID))
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: reload incident for event: %w", err)
	}
	eventType := domain.EventIncidentUpdated
	if upd.Status == domain.IncidentResolved {
		eventType = domain.EventIncidentResolved
	}
	payload, err := json.Marshal(domain.IncidentEvent{Type: eventType, Incident: evInc, Update: &created})
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: marshal incident event: %w", err)
	}
	if err := enqueueOutboxTx(ctx, tx, domain.TopicIncidentEvent, payload); err != nil {
		return domain.IncidentUpdate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: commit add update: %w", err)
	}
	return created, nil
}

// ListIncidentUpdates lists an incident's timeline in chronological order.
func (s *Store) ListIncidentUpdates(ctx context.Context, incidentID string) ([]domain.IncidentUpdate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+incidentUpdateColumns+` FROM incident_updates WHERE incident_id = $1 ORDER BY created_at ASC`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("store: list incident updates: %w", err)
	}
	defer rows.Close()
	var out []domain.IncidentUpdate
	for rows.Next() {
		u, err := scanIncidentUpdate(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan incident update: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate incident updates: %w", err)
	}
	return out, nil
}

const postmortemColumns = "id, incident_id, body, author, published_at, created_at, updated_at"

func scanPostmortem(row pgx.Row) (domain.Postmortem, error) {
	var p domain.Postmortem
	if err := row.Scan(&p.ID, &p.IncidentID, &p.Body, &p.Author, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return domain.Postmortem{}, err
	}
	return p, nil
}

// UpsertPostmortem creates or replaces the postmortem attached to an incident.
func (s *Store) UpsertPostmortem(ctx context.Context, incidentID, body, author string) (domain.Postmortem, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO postmortems (incident_id, body, author) VALUES ($1,$2,$3)
		 ON CONFLICT (incident_id)
		 DO UPDATE SET body = EXCLUDED.body, author = EXCLUDED.author, updated_at = now()
		 RETURNING `+postmortemColumns,
		incidentID, body, author)
	p, err := scanPostmortem(row)
	if err != nil {
		return domain.Postmortem{}, fmt.Errorf("store: upsert postmortem: %w", err)
	}
	return p, nil
}

// GetPostmortem returns an incident's postmortem, or ErrNotFound.
func (s *Store) GetPostmortem(ctx context.Context, incidentID string) (domain.Postmortem, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+postmortemColumns+` FROM postmortems WHERE incident_id = $1`, incidentID)
	p, err := scanPostmortem(row)
	if noRows(err) {
		return domain.Postmortem{}, ErrNotFound
	}
	if err != nil {
		return domain.Postmortem{}, fmt.Errorf("store: get postmortem: %w", err)
	}
	return p, nil
}
