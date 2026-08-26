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

const incidentColumns = "id, project_id, monitor_id, service_id, title, status, impact, source, external_key, started_at, resolved_at, acknowledged_at, acknowledged_by, escalation_step, last_escalated_at, created_at, updated_at"

func scanIncident(row pgx.Row) (domain.Incident, error) {
	var (
		inc         domain.Incident
		monitorID   *string
		serviceID   *string
		externalKey *string
		resolved    *time.Time
		ackedAt     *time.Time
		ackedBy     *string
		lastEsc     *time.Time
	)
	if err := row.Scan(&inc.ID, &inc.ProjectID, &monitorID, &serviceID, &inc.Title, &inc.Status, &inc.Impact,
		&inc.Source, &externalKey, &inc.StartedAt, &resolved, &ackedAt, &ackedBy,
		&inc.EscalationStep, &lastEsc, &inc.CreatedAt, &inc.UpdatedAt); err != nil {
		return domain.Incident{}, err
	}
	inc.LastEscalatedAt = lastEsc
	if monitorID != nil {
		inc.MonitorID = *monitorID
	}
	if serviceID != nil {
		inc.ServiceID = *serviceID
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
	// Same row, same rule as `AddIncidentUpdate`: lock, then read the clock, then write. A single
	// UPDATE looks atomic and is — but its `now()` is fixed when the statement's transaction began,
	// so an acknowledgement that waited behind a timeline update stamps `updated_at` BEFORE the
	// update it queued behind, walking the incident's own modification time backwards.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: begin acknowledge: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var status domain.IncidentStatus
	err = tx.QueryRow(ctx, `SELECT status FROM incidents WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if noRows(err) {
		return domain.Incident{}, ErrNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: lock incident for acknowledge: %w", err)
	}
	if status.Terminal() {
		// Unchanged answer: a resolved incident is not acknowledgeable, and the caller has always
		// been told that as ErrNotFound.
		return domain.Incident{}, ErrNotFound
	}
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return domain.Incident{}, fmt.Errorf("store: read acknowledge instant: %w", err)
	}
	row := tx.QueryRow(ctx,
		`UPDATE incidents
		    SET acknowledged_at = COALESCE(acknowledged_at, $3),
		        acknowledged_by = COALESCE(acknowledged_by, $2),
		        updated_at = $3
		  WHERE id = $1 AND status <> 'resolved'
		  RETURNING `+incidentColumns,
		id, by, asOf)
	inc, err := scanIncident(row)
	if noRows(err) {
		return domain.Incident{}, ErrNotFound
	}
	if err == nil {
		if cerr := tx.Commit(ctx); cerr != nil {
			return domain.Incident{}, fmt.Errorf("store: commit acknowledge: %w", cerr)
		}
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

	var monitorID, serviceID, externalKey *string
	if inc.MonitorID != "" {
		monitorID = &inc.MonitorID
	}
	// The OTHER anchor (FR-022). Nothing here decides exclusivity: `incidents_one_anchor_chk` does, so a
	// caller that sets both is refused by the database rather than by whichever branch happens to run.
	if inc.ServiceID != "" {
		serviceID = &inc.ServiceID
	}
	if inc.ExternalKey != "" {
		externalKey = &inc.ExternalKey
	}
	row := tx.QueryRow(ctx,
		// An incident CREATED as resolved is stamped at creation. D-0020 promises `resolved_at` the
		// first time an incident reaches Resolved, and creation is one of those times: the API and
		// the new-incident form both offer the whole status list, so recording a historical outage
		// after the fact is a supported act. Without the stamp such an incident falls out of BOTH
		// status-page lists at once — the active one filters `status <> 'resolved'`, the recent one
		// `resolved_at IS NOT NULL` — so the operator writes it down and it appears nowhere.
		`INSERT INTO incidents (project_id, monitor_id, service_id, title, status, impact, source, external_key,
		                        resolved_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,
		         CASE WHEN $5 = 'resolved' THEN statement_timestamp() ELSE NULL END)
		 RETURNING `+incidentColumns,
		inc.ProjectID, monitorID, serviceID, inc.Title, inc.Status, inc.Impact, inc.Source, externalKey)
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
	// A SERVICE-anchored incident freezes its ladder here too. No production caller creates one
	// through this door today — the evaluator uses `OpenServiceIncidentTx` — but an incident that
	// reached the table by a route that skipped the snapshot would silently never escalate, and
	// "the other door forgot" is the exact shape of three defects this iteration already fixed.
	if created.ServiceID != "" {
		if err := snapshotEscalationPolicyTx(ctx, tx, created.ID, created.ProjectID, created.ServiceID); err != nil {
			return domain.Incident{}, err
		}
	}
	// Enqueue the webhook event in the same transaction — the event is durable iff
	// the incident is (no dual-write).
	seq, err := nextIncidentEventSeqTx(ctx, tx, created.ID)
	if err != nil {
		return domain.Incident{}, err
	}
	payload, err := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentOpened, Incident: created, Seq: seq,
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: marshal incident event: %w", err)
	}
	if err := enqueueOutboxTx(ctx, tx, domain.TopicIncidentEvent, payload); err != nil {
		return domain.Incident{}, err
	}
	// An ANCHORED open also enqueues its correlation attempt (FR-021 §14.3) in this
	// same transaction — its OWN topic, so webhook death never blocks correlation and
	// a correlation failure never blocks incident delivery. The topic is fenced: an
	// old delivery owner in a mixed-version fleet cannot claim it (enqueueOutboxTx
	// sets the class from domain.FencedTopic). Either anchor qualifies: FR-022 gives a
	// service incident its impact links, and an incident anchored to NEITHER (a manual
	// project-level record) has no position in the graph to compute links from.
	if created.MonitorID != "" || created.ServiceID != "" {
		corr, err := json.Marshal(domain.IncidentCorrelation{IncidentID: created.ID})
		if err != nil {
			return domain.Incident{}, fmt.Errorf("store: marshal incident correlation: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicIncidentCorrelation, corr); err != nil {
			return domain.Incident{}, err
		}
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

	// THE ORDER OF THESE THREE STEPS IS THE INVARIANT: lock, then read the clock, then write.
	//
	// (1) The incident row is locked FOR UPDATE before anything is decided. Every rule below is
	// about the row's CURRENT status, and a rule evaluated against a value read outside the lock is
	// a rule about the past. The API's pre-check is exactly that, which is why it is a courtesy and
	// this is the enforcement.
	var current domain.IncidentStatus
	err = tx.QueryRow(ctx,
		`SELECT status FROM incidents WHERE id = $1 FOR UPDATE`, upd.IncidentID).Scan(&current)
	if noRows(err) {
		return domain.IncidentUpdate{}, ErrNotFound
	}
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: lock incident: %w", err)
	}

	// The keep-current intent is resolved HERE, against the locked row, never by the caller. A
	// handler that reads the incident and posts its status back is publishing a value that was true
	// when it looked, and landing it later reverts whatever happened in between.
	next := upd.Status
	if next == "" {
		next = current
	}
	switch {
	case current.Terminal():
		return domain.IncidentUpdate{}, ErrIncidentTerminal
	case !next.CanFollow(current):
		// Backwards. Sequentially or by race, the timeline would say the operators un-diagnosed
		// something, and the public page would show it.
		return domain.IncidentUpdate{}, fmt.Errorf("%w: %s cannot follow %s", ErrStatusRegression, next, current)
	}
	upd.Status = next

	// (2) ONE instant for everything this transaction writes, taken AFTER the lock. `now()` is the
	// transaction's start time, so a writer that waited on the lock would stamp its timeline entry
	// with a moment before the update it is actually following — the record would claim an order the
	// database did not have. `statement_timestamp()` in its own statement, after the wait, is the
	// first clock reading this writer is entitled to.
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: read write instant: %w", err)
	}

	// (3) The writes, all carrying that instant. The status guard stays in the statement as well:
	// belt and braces cost nothing here, and it keeps the SQL true on its own terms.
	evInc, err := scanIncident(tx.QueryRow(ctx,
		`UPDATE incidents
		    SET status = $2,
		        updated_at = $3,
		        resolved_at = CASE WHEN $2 = 'resolved' AND resolved_at IS NULL THEN $3 ELSE resolved_at END
		  WHERE id = $1 AND status <> 'resolved'
		 RETURNING `+incidentColumns,
		upd.IncidentID, upd.Status, asOf))
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: sync incident status: %w", err)
	}

	row := tx.QueryRow(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author, created_at)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+incidentUpdateColumns,
		upd.IncidentID, upd.Status, upd.Body, upd.Author, asOf)
	created, err := scanIncidentUpdate(row)
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: add incident update: %w", err)
	}
	// The lifecycle event rides the same transaction as the fact it announces (no dual-write).
	// The event type follows the update's status.
	eventType := domain.EventIncidentUpdated
	if upd.Status == domain.IncidentResolved {
		eventType = domain.EventIncidentResolved
	}
	seq, err := nextIncidentEventSeqTx(ctx, tx, upd.IncidentID)
	if err != nil {
		return domain.IncidentUpdate{}, err
	}
	payload, err := json.Marshal(domain.IncidentEvent{
		Type: eventType, Incident: evInc, Update: &created, Seq: seq,
	})
	if err != nil {
		return domain.IncidentUpdate{}, fmt.Errorf("store: marshal incident event: %w", err)
	}
	// The event is SCHEDULED on the same instant as the rows that caused it. Left on the default it
	// would carry the transaction's start, so a writer that waited would hand the outbox an event
	// due before the update it followed.
	if err := enqueueOutboxAtTx(ctx, tx, domain.TopicIncidentEvent, payload, asOf); err != nil {
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

// nextIncidentEventSeqTx advances the incident's lifecycle sequence and returns it. Every path that
// enqueues an `incident_event` calls it inside the same transaction as the fact, so the number in the
// payload and the number in the row can never disagree.
func nextIncidentEventSeqTx(ctx context.Context, tx pgx.Tx, incidentID string) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx,
		`UPDATE incidents SET event_seq = event_seq + 1 WHERE id = $1 RETURNING event_seq`,
		incidentID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: advance incident event sequence: %w", err)
	}
	return seq, nil
}
