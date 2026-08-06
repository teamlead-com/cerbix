package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// monitorColumns includes a correlated aggregate for depends_on; the bare `id`
// inside it resolves to the outer monitors row under any table alias.
const monitorColumns = "id, project_id, name, type, target, interval_seconds, timeout_seconds, retries, conditions, enabled, status, created_at, updated_at, push_token, method, grace_seconds, config, auto_incident, failure_threshold, renotify_seconds, tags, region, escalation_policy_id, confirm_interval_seconds, consecutive_failures, " +
	"(SELECT COALESCE(array_agg(d.depends_on_id::text ORDER BY d.created_at), '{}') FROM monitor_dependencies d WHERE d.monitor_id = id) AS depends_on"

// methodOrGet keeps the NOT NULL method column concrete; the prober ignores it
// for non-HTTP monitors.
func methodOrGet(m domain.Monitor) string {
	if m.Method == "" {
		return "GET"
	}
	return m.Method
}

// nullableID maps an empty id to SQL NULL (empty string is not a valid uuid) and a
// set id to itself, for nullable-fk columns like monitors.escalation_policy_id.
func nullableID(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

func (s *Store) scanMonitor(row pgx.Row) (domain.Monitor, error) {
	var (
		m         domain.Monitor
		typ       string
		stat      string
		pushToken *string
		escPolicy *string
	)
	var config []byte
	err := row.Scan(&m.ID, &m.ProjectID, &m.Name, &typ, &m.Target,
		&m.IntervalSeconds, &m.TimeoutSeconds, &m.Retries, &m.Conditions,
		&m.Enabled, &stat, &m.CreatedAt, &m.UpdatedAt, &pushToken, &m.Method, &m.GraceSeconds, &config, &m.AutoIncident, &m.FailureThreshold, &m.RenotifySeconds, &m.Tags, &m.Region, &escPolicy, &m.ConfirmIntervalSeconds, &m.ConsecutiveFailures, &m.DependsOn)
	if err != nil {
		return domain.Monitor{}, err
	}
	m.Type = domain.MonitorType(typ)
	m.Status = domain.MonitorStatus(stat)
	if pushToken != nil {
		m.PushToken = *pushToken
	}
	if escPolicy != nil {
		m.EscalationPolicyID = *escPolicy
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &m.Config); err != nil {
			return domain.Monitor{}, fmt.Errorf("store: decode monitor config: %w", err)
		}
		for k := range domain.SecretMonitorConfigKeys {
			if v, ok := m.Config[k]; ok {
				plain, err := s.cipher.Decrypt(v)
				if err != nil {
					return domain.Monitor{}, fmt.Errorf("store: decrypt monitor config %q: %w", k, err)
				}
				m.Config[k] = plain
			}
		}
	}
	return m, nil
}

// marshalConfig encrypts secret config values (when a cipher is set) and encodes
// the config map to JSON for storage ('{}' when empty).
func (s *Store) marshalConfig(m domain.Monitor) ([]byte, error) {
	if len(m.Config) == 0 {
		return []byte("{}"), nil
	}
	enc := make(map[string]string, len(m.Config))
	for k, v := range m.Config {
		if domain.SecretMonitorConfigKeys[k] {
			ev, err := s.cipher.Encrypt(v)
			if err != nil {
				return nil, fmt.Errorf("store: encrypt monitor config %q: %w", k, err)
			}
			enc[k] = ev
		} else {
			enc[k] = v
		}
	}
	b, err := json.Marshal(enc)
	if err != nil {
		return nil, fmt.Errorf("store: encode monitor config: %w", err)
	}
	return b, nil
}

// MonitorStatuses returns the current status of each given monitor id (missing
// ids are simply absent from the map) — used by composite evaluation.
func (s *Store) MonitorStatuses(ctx context.Context, ids []string) (map[string]domain.MonitorStatus, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, status FROM monitors WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: monitor statuses: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.MonitorStatus, len(ids))
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, fmt.Errorf("store: scan monitor status: %w", err)
		}
		out[id] = domain.MonitorStatus(st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate monitor statuses: %w", err)
	}
	return out, nil
}

// CreateMonitor inserts a monitor. The caller validates via domain first.
func (s *Store) CreateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) {
	conditions := m.Conditions
	if conditions == nil {
		conditions = []string{}
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	region := m.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	var pushToken *string
	if m.PushToken != "" {
		pushToken = &m.PushToken
	}
	config, err := s.marshalConfig(m)
	if err != nil {
		return domain.Monitor{}, err
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO monitors (project_id, name, type, target, interval_seconds, timeout_seconds, retries, conditions, enabled, push_token, method, grace_seconds, config, auto_incident, failure_threshold, renotify_seconds, tags, region, escalation_policy_id, confirm_interval_seconds)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) RETURNING `+monitorColumns,
		m.ProjectID, m.Name, string(m.Type), m.Target, m.IntervalSeconds, m.TimeoutSeconds, m.Retries, conditions, m.Enabled, pushToken, methodOrGet(m), m.GraceSeconds, config, m.AutoIncident, m.FailureThreshold, m.RenotifySeconds, tags, region, nullableID(m.EscalationPolicyID), m.ConfirmIntervalSeconds)
	created, err := s.scanMonitor(row)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: create monitor: %w", err)
	}
	return created, nil
}

// GetMonitorByPushToken returns the push monitor with the given token, or ErrNotFound.
func (s *Store) GetMonitorByPushToken(ctx context.Context, token string) (domain.Monitor, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE push_token = $1`, token)
	m, err := s.scanMonitor(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: get monitor by push token: %w", err)
	}
	return m, nil
}

// UpdateMonitor updates a monitor's mutable fields (name, target, schedule,
// conditions, enabled) by id. Type and push_token are immutable. ErrNotFound if
// the monitor is gone. The caller validates via domain first.
func (s *Store) UpdateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) {
	conditions := m.Conditions
	if conditions == nil {
		conditions = []string{}
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	region := m.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	config, err := s.marshalConfig(m)
	if err != nil {
		return domain.Monitor{}, err
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE monitors
		    SET name = $2, target = $3, interval_seconds = $4, timeout_seconds = $5,
		        retries = $6, conditions = $7, enabled = $8, method = $9, grace_seconds = $10, config = $11, auto_incident = $12, failure_threshold = $13, renotify_seconds = $14, tags = $15, region = $16, escalation_policy_id = $17, confirm_interval_seconds = $18, updated_at = now()
		  WHERE id = $1 RETURNING `+monitorColumns,
		m.ID, m.Name, m.Target, m.IntervalSeconds, m.TimeoutSeconds, m.Retries, conditions, m.Enabled, methodOrGet(m), m.GraceSeconds, config, m.AutoIncident, m.FailureThreshold, m.RenotifySeconds, tags, region, nullableID(m.EscalationPolicyID), m.ConfirmIntervalSeconds)
	updated, err := s.scanMonitor(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: update monitor: %w", err)
	}
	return updated, nil
}

// GetMonitor returns a monitor by id.
func (s *Store) GetMonitor(ctx context.Context, id string) (domain.Monitor, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE id = $1`, id)
	m, err := s.scanMonitor(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: get monitor: %w", err)
	}
	return m, nil
}

// ListMonitorsByProject returns all monitors in a project.
func (s *Store) ListMonitorsByProject(ctx context.Context, projectID string) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list monitors by project: %w", err)
	}
	return s.collectMonitors(rows)
}

// MonitorRegions returns the region of each given monitor id (missing/deleted ids are
// absent). Used to verify a pull agent only posts results for its own region.
func (s *Store) MonitorRegions(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, region FROM monitors WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: monitor regions: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, region string
		if err := rows.Scan(&id, &region); err != nil {
			return nil, fmt.Errorf("store: scan monitor region: %w", err)
		}
		out[id] = region
	}
	return out, rows.Err()
}

// ListEnabledMonitors returns all enabled monitors (for the scheduler).
func (s *Store) ListEnabledMonitors(ctx context.Context) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled monitors: %w", err)
	}
	return s.collectMonitors(rows)
}

// StalePushMonitors returns enabled push monitors whose dead-man's switch has
// tripped: not already down, and with no heartbeat within their interval (falling
// back to created_at when they have never reported). One query with an
// index-backed correlated max(ts) replaces a per-monitor latest-heartbeat lookup.
func (s *Store) StalePushMonitors(ctx context.Context) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+monitorColumns+` FROM monitors m
		 WHERE m.type = 'push' AND m.enabled AND m.status <> 'down'
		   AND COALESCE((SELECT max(h.ts) FROM heartbeats h WHERE h.monitor_id = m.id), m.created_at)
		       < now() - make_interval(secs => m.interval_seconds + m.grace_seconds)`)
	if err != nil {
		return nil, fmt.Errorf("store: stale push monitors: %w", err)
	}
	return s.collectMonitors(rows)
}

// SetMonitorStatus updates a monitor's last-known status and returns the previous
// status, so callers can detect up/down transitions. ErrNotFound if the monitor
// is gone. Test support: production writes status through RecordCheckStatus
// (ingest); integration tests use this to arrange transition scenarios.
func (s *Store) SetMonitorStatus(ctx context.Context, id string, status domain.MonitorStatus) (domain.MonitorStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin set monitor status: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var prev domain.MonitorStatus
	err = tx.QueryRow(ctx,
		`WITH prev AS (SELECT id, status AS old FROM monitors WHERE id = $1)
		 UPDATE monitors m SET status = $2, updated_at = now()
		 FROM prev WHERE m.id = prev.id
		 RETURNING prev.old`,
		id, string(status)).Scan(&prev)
	if noRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: set monitor status: %w", err)
	}
	// On a real transition, enqueue the raw fact in the same transaction; the
	// outbox worker applies the notification policy. No dual-write.
	if prev != status {
		payload, err := json.Marshal(domain.MonitorTransition{MonitorID: id, Prev: prev, Cur: status})
		if err != nil {
			return "", fmt.Errorf("store: marshal transition: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicMonitorTransition, payload); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit set monitor status: %w", err)
	}
	return prev, nil
}

// RecordCheckStatus applies one check result to a monitor's status atomically,
// with alert confirmations and maintenance suppression:
//   - a down flip happens only after failure_threshold consecutive failures
//     (the live consecutive_failures counter); recovery (up) is immediate and
//     resets the counter;
//   - when the flip occurs inside an active maintenance window it is suppressed:
//     the status still changes (accuracy + SLA) but no transition notification is
//     enqueued and suppressed=true tells the caller not to open an incident.
//
// It returns the previous and new status plus whether the change was suppressed.
func (s *Store) RecordCheckStatus(ctx context.Context, monitorID string, up bool) (prev, cur domain.MonitorStatus, suppressed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("store: begin record status: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var prevS, curS string
	var inMaint bool
	var fails, threshold, confirmSec int
	err = tx.QueryRow(ctx,
		`WITH cur AS (
		   SELECT id, project_id, status AS old_status, consecutive_failures, failure_threshold
		   FROM monitors WHERE id = $1 FOR UPDATE
		 ),
		 maint AS (
		   SELECT EXISTS(
		     SELECT 1 FROM maintenance_windows mw, cur
		     WHERE mw.starts_at <= now() AND mw.ends_at >= now()
		       AND (mw.monitor_id = cur.id OR (mw.monitor_id IS NULL AND mw.project_id = cur.project_id))
		   ) AS in_maint
		 )
		 UPDATE monitors m SET
		   consecutive_failures = CASE WHEN $2 THEN 0 ELSE cur.consecutive_failures + 1 END,
		   status = CASE
		     WHEN $2 THEN 'up'
		     WHEN cur.consecutive_failures + 1 >= cur.failure_threshold THEN 'down'
		     ELSE m.status
		   END,
		   -- last_notified_at drives re-notify: stamped on a fresh (non-suppressed)
		   -- down, cleared on recovery, otherwise left for the reminder job to bump.
		   last_notified_at = CASE
		     WHEN $2 THEN NULL
		     WHEN cur.consecutive_failures + 1 >= cur.failure_threshold AND cur.old_status <> 'down' AND NOT maint.in_maint THEN now()
		     ELSE m.last_notified_at
		   END,
		   updated_at = now()
		 FROM cur, maint
		 WHERE m.id = cur.id
		 RETURNING cur.old_status, m.status, maint.in_maint, m.consecutive_failures, m.failure_threshold, m.confirm_interval_seconds`,
		monitorID, up).Scan(&prevS, &curS, &inMaint, &fails, &threshold, &confirmSec)
	if noRows(err) {
		return "", "", false, ErrNotFound
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: record check status: %w", err)
	}
	prev, cur = domain.MonitorStatus(prevS), domain.MonitorStatus(curS)
	// Confirmation phase entered/continuing (a failure counted, no verdict yet):
	// wake the scheduler leader so the next probe comes at the confirm interval
	// instead of the main one. Same-transaction NOTIFY — delivered on commit; a
	// missed signal is harmless (the snapshot refresh path catches up).
	if !up && cur != domain.StatusDown && fails > 0 && fails < threshold && confirmSec > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, ConfirmChannel, monitorID); err != nil {
			return "", "", false, fmt.Errorf("store: notify confirm phase: %w", err)
		}
	}
	// Notify only on a real flip that isn't muted by an active maintenance window.
	if prev != cur && !inMaint {
		payload, err := json.Marshal(domain.MonitorTransition{MonitorID: monitorID, Prev: prev, Cur: cur})
		if err != nil {
			return "", "", false, fmt.Errorf("store: marshal transition: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicMonitorTransition, payload); err != nil {
			return "", "", false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", false, fmt.Errorf("store: commit record status: %w", err)
	}
	return prev, cur, inMaint, nil
}

// EnqueueRenotifyReminders re-sends the down alert for monitors that have stayed
// down longer than their renotify_seconds since the last notification. It claims
// the due monitors, enqueues one reminder transition each, and bumps
// last_notified_at so the next reminder waits another interval — all in one
// transaction. Returns the number of reminders enqueued. Leader-only.
func (s *Store) EnqueueRenotifyReminders(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin renotify: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx,
		`SELECT id FROM monitors
		  WHERE status = 'down' AND renotify_seconds > 0 AND last_notified_at IS NOT NULL
		    AND escalation_policy_id IS NULL
		    AND last_notified_at <= now() - make_interval(secs => renotify_seconds)
		  FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("store: select renotify due: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan renotify id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate renotify due: %w", err)
	}
	for _, id := range ids {
		payload, err := json.Marshal(domain.MonitorTransition{MonitorID: id, Prev: domain.StatusDown, Cur: domain.StatusDown, Reminder: true})
		if err != nil {
			return 0, fmt.Errorf("store: marshal reminder: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicMonitorTransition, payload); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE monitors SET last_notified_at = now() WHERE id = $1`, id); err != nil {
			return 0, fmt.Errorf("store: bump last_notified_at: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit renotify: %w", err)
	}
	return len(ids), nil
}

// DeleteMonitor removes a monitor (and its heartbeats via cascade).
func (s *Store) DeleteMonitor(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM monitors WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete monitor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) collectMonitors(rows pgx.Rows) ([]domain.Monitor, error) {
	defer rows.Close()
	var out []domain.Monitor
	for rows.Next() {
		m, err := s.scanMonitor(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan monitor: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate monitors: %w", err)
	}
	return out, nil
}

// ListRegions returns the distinct worker-pool regions in use across all monitors,
// always including the default (core), sorted. Powers the region picker in the UI.
func (s *Store) ListRegions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT region FROM monitors WHERE region <> '' ORDER BY region`)
	if err != nil {
		return nil, fmt.Errorf("store: list regions: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{domain.DefaultRegion: true}
	out := []string{domain.DefaultRegion}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("store: scan region: %w", err)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate regions: %w", err)
	}
	return out, nil
}
