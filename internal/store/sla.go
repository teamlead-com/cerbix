package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// SLICounts are heartbeat aggregates over a window, with maintenance-window
// heartbeats already excluded.
type SLICounts struct {
	Total        int64
	Up           int64
	AvgLatencyMS float64
	P95LatencyMS float64
}

// MonitorSLI aggregates a monitor's heartbeats since `since`, excluding any that
// fall inside a maintenance window covering the monitor (monitor-scoped or its
// project-scoped windows).
func (s *Store) MonitorSLI(ctx context.Context, monitorID string, since time.Time) (SLICounts, error) {
	var c SLICounts
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)::bigint,
		        count(*) FILTER (WHERE h.up)::bigint,
		        COALESCE(avg(h.latency_ms) FILTER (WHERE h.up), 0)::float8,
		        COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY h.latency_ms) FILTER (WHERE h.up), 0)::float8
		 FROM heartbeats h
		 WHERE h.monitor_id = $1 AND h.ts >= $2
		 AND NOT EXISTS (
		     SELECT 1 FROM maintenance_windows mw
		     WHERE mw.starts_at <= h.ts AND mw.ends_at > h.ts
		       AND (mw.monitor_id = $1
		            OR (mw.monitor_id IS NULL
		                AND mw.project_id = (SELECT project_id FROM monitors WHERE id = $1)))
		 )`,
		monitorID, since).Scan(&c.Total, &c.Up, &c.AvgLatencyMS, &c.P95LatencyMS)
	if err != nil {
		return SLICounts{}, fmt.Errorf("store: monitor sli: %w", err)
	}
	return c, nil
}

// ProjectSLI aggregates all heartbeats of a project's monitors since `since`,
// excluding maintenance windows (monitor- or project-scoped).
func (s *Store) ProjectSLI(ctx context.Context, projectID string, since time.Time) (SLICounts, error) {
	var c SLICounts
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)::bigint,
		        count(*) FILTER (WHERE h.up)::bigint,
		        COALESCE(avg(h.latency_ms) FILTER (WHERE h.up), 0)::float8,
		        COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY h.latency_ms) FILTER (WHERE h.up), 0)::float8
		 FROM heartbeats h
		 JOIN monitors m ON m.id = h.monitor_id
		 WHERE m.project_id = $1 AND h.ts >= $2
		 AND NOT EXISTS (
		     SELECT 1 FROM maintenance_windows mw
		     WHERE mw.starts_at <= h.ts AND mw.ends_at > h.ts
		       AND (mw.monitor_id = h.monitor_id
		            OR (mw.monitor_id IS NULL AND mw.project_id = $1))
		 )`,
		projectID, since).Scan(&c.Total, &c.Up, &c.AvgLatencyMS, &c.P95LatencyMS)
	if err != nil {
		return SLICounts{}, fmt.Errorf("store: project sli: %w", err)
	}
	return c, nil
}

const slaTargetColumns = "id, monitor_id, project_id, objective, window_name, " +
	"burn_alert_enabled, burn_rules, created_at, updated_at"

func scanSLATarget(row pgx.Row) (domain.SLATarget, error) {
	var (
		t                 domain.SLATarget
		monitorID, projID *string
		rules             []byte
	)
	if err := row.Scan(&t.ID, &monitorID, &projID, &t.Objective, &t.Window,
		&t.BurnAlertEnabled, &rules, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return domain.SLATarget{}, err
	}
	if monitorID != nil {
		t.MonitorID = *monitorID
	}
	if projID != nil {
		t.ProjectID = *projID
	}
	if len(rules) > 0 {
		if err := json.Unmarshal(rules, &t.BurnRules); err != nil {
			return domain.SLATarget{}, fmt.Errorf("decode burn rules: %w", err)
		}
	}
	return t, nil
}

// UpsertMonitorSLATarget sets (or replaces) a monitor's SLO objective for a
// window and its burn-rate alerting. Rules semantics: enabling burn alerts with
// no rules provided keeps the target's existing rules (or seeds the SRE default
// pair on a fresh target); an explicit rules slice replaces the set. Firing
// latches survive edits for rules whose configuration is unchanged (matched by
// Key), so an in-flight alert isn't reset by an unrelated save.
func (s *Store) UpsertMonitorSLATarget(ctx context.Context, monitorID, window string, objective float64, burnAlert bool, rules []domain.BurnRule) (domain.SLATarget, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.SLATarget{}, fmt.Errorf("store: begin upsert sla target: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var existing []domain.BurnRule
	var raw []byte
	err = tx.QueryRow(ctx,
		`SELECT burn_rules FROM sla_targets WHERE monitor_id = $1 AND window_name = $2 FOR UPDATE`,
		monitorID, window).Scan(&raw)
	if err != nil && !noRows(err) {
		return domain.SLATarget{}, fmt.Errorf("store: read burn rules: %w", err)
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &existing)
	}

	next := rules
	if burnAlert && next == nil {
		next = existing
		if len(next) == 0 {
			next = domain.DefaultBurnRules()
		}
	}
	// Carry firing latches across the edit for unchanged rule configurations.
	latch := make(map[string]bool, len(existing))
	for _, r := range existing {
		latch[r.Key()] = r.Firing
	}
	for i := range next {
		next[i].Firing = latch[next[i].Key()]
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return domain.SLATarget{}, fmt.Errorf("store: encode burn rules: %w", err)
	}
	if next == nil {
		encoded = []byte(`[]`)
	}

	row := tx.QueryRow(ctx,
		`INSERT INTO sla_targets (monitor_id, objective, window_name, burn_alert_enabled, burn_rules) VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (monitor_id, window_name) WHERE monitor_id IS NOT NULL
		 DO UPDATE SET objective = EXCLUDED.objective, burn_alert_enabled = EXCLUDED.burn_alert_enabled,
		               burn_rules = EXCLUDED.burn_rules, updated_at = now()
		 RETURNING `+slaTargetColumns,
		monitorID, objective, window, burnAlert, encoded)
	t, err := scanSLATarget(row)
	if err != nil {
		return domain.SLATarget{}, fmt.Errorf("store: upsert monitor sla target: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.SLATarget{}, fmt.Errorf("store: commit upsert sla target: %w", err)
	}
	return t, nil
}

// EvaluateBurnAlerts scans every burn-enabled monitor SLA target and evaluates
// each of its burn rules over the rule's window PAIR (maintenance excluded): a
// rule fires only when the burn rate is at/over the threshold in both the long
// and the short window (multi-window multi-burn-rate, D-0098). Each rule latches
// its own firing state, so an alert is emitted once per crossing per rule. Runs
// on the scheduler leader. Returns the number of fired and resolved alerts.
func (s *Store) EvaluateBurnAlerts(ctx context.Context) (fired, resolved int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin burn eval: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx,
		`SELECT id, monitor_id, window_name, objective, burn_rules
		   FROM sla_targets
		  WHERE burn_alert_enabled AND monitor_id IS NOT NULL AND burn_rules <> '[]'
		  FOR UPDATE`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: select burn targets: %w", err)
	}
	type target struct {
		id, monitorID, window string
		objective             float64
		rules                 []domain.BurnRule
	}
	var targets []target
	for rows.Next() {
		var t target
		var raw []byte
		if err := rows.Scan(&t.id, &t.monitorID, &t.window, &t.objective, &raw); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan burn target: %w", err)
		}
		if err := json.Unmarshal(raw, &t.rules); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: decode burn rules: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: iterate burn targets: %w", err)
	}

	// burnWindowRate measures the maintenance-excluded burn rate over one window;
	// ok=false when the window holds no data (no decision, keep the latch).
	burnWindowRate := func(monitorID string, seconds int, objective float64) (float64, bool, error) {
		var total, up int64
		err := tx.QueryRow(ctx,
			`SELECT count(*)::bigint, count(*) FILTER (WHERE h.up)::bigint
			   FROM heartbeats h
			  WHERE h.monitor_id = $1 AND h.ts >= now() - make_interval(secs => $2)
			    AND NOT EXISTS (
			        SELECT 1 FROM maintenance_windows mw
			        WHERE mw.starts_at <= h.ts AND mw.ends_at > h.ts
			          AND (mw.monitor_id = $1
			               OR (mw.monitor_id IS NULL
			                   AND mw.project_id = (SELECT project_id FROM monitors WHERE id = $1)))
			    )`,
			monitorID, seconds).Scan(&total, &up)
		if err != nil {
			return 0, false, fmt.Errorf("store: burn window counts: %w", err)
		}
		if total == 0 {
			return 0, false, nil
		}
		return sla.BurnRate(objective, up, total), true, nil
	}

	for _, t := range targets {
		changed := false
		for i := range t.rules {
			rule := &t.rules[i]
			longRate, ok, err := burnWindowRate(t.monitorID, rule.LongWindowSeconds, t.objective)
			if err != nil {
				return 0, 0, err
			}
			if !ok {
				continue
			}
			shortRate := longRate
			if rule.ShortWindowSeconds != rule.LongWindowSeconds {
				shortRate, ok, err = burnWindowRate(t.monitorID, rule.ShortWindowSeconds, t.objective)
				if err != nil {
					return 0, 0, err
				}
				if !ok {
					continue
				}
			}
			nowFiring := longRate >= rule.Threshold && shortRate >= rule.Threshold
			if nowFiring == rule.Firing {
				continue // no edge for this rule
			}
			payload, err := json.Marshal(domain.SLOBurnAlert{
				MonitorID: t.monitorID, Window: t.window, WindowSeconds: rule.LongWindowSeconds,
				ShortWindowSeconds: rule.ShortWindowSeconds, Severity: rule.Severity,
				Objective: t.objective, BurnRate: longRate, Threshold: rule.Threshold, Firing: nowFiring,
			})
			if err != nil {
				return 0, 0, fmt.Errorf("store: marshal burn alert: %w", err)
			}
			if err := enqueueOutboxTx(ctx, tx, domain.TopicSLOBurnAlert, payload); err != nil {
				return 0, 0, err
			}
			rule.Firing = nowFiring
			changed = true
			if nowFiring {
				fired++
			} else {
				resolved++
			}
		}
		if changed {
			encoded, err := json.Marshal(t.rules)
			if err != nil {
				return 0, 0, fmt.Errorf("store: encode burn rules: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE sla_targets SET burn_rules = $2, burn_notified_at = now() WHERE id = $1`,
				t.id, encoded); err != nil {
				return 0, 0, fmt.Errorf("store: latch burn rules: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit burn eval: %w", err)
	}
	return fired, resolved, nil
}

// GetMonitorSLATarget returns a monitor's SLO target for a window, or ErrNotFound.
func (s *Store) GetMonitorSLATarget(ctx context.Context, monitorID, window string) (domain.SLATarget, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+slaTargetColumns+` FROM sla_targets WHERE monitor_id = $1 AND window_name = $2`, monitorID, window)
	t, err := scanSLATarget(row)
	if noRows(err) {
		return domain.SLATarget{}, ErrNotFound
	}
	if err != nil {
		return domain.SLATarget{}, fmt.Errorf("store: get monitor sla target: %w", err)
	}
	return t, nil
}

// slaReportWindows are the rolling windows summarized in a weekly SLA report.
var slaReportWindows = []struct {
	name     string
	duration time.Duration
}{
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

// SetProjectSLAReport toggles a project's weekly SLA report and returns the new
// state. Enabling it with no prior watermark makes a report due on the next tick.
func (s *Store) SetProjectSLAReport(ctx context.Context, projectID string, enabled bool) (bool, error) {
	var got bool
	err := s.pool.QueryRow(ctx,
		`UPDATE projects SET sla_report_weekly = $2, updated_at = now() WHERE id = $1 RETURNING sla_report_weekly`,
		projectID, enabled).Scan(&got)
	if noRows(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: set project sla report: %w", err)
	}
	return got, nil
}

// ProjectSLAReportEnabled reports whether a project's weekly SLA report is on.
func (s *Store) ProjectSLAReportEnabled(ctx context.Context, projectID string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT sla_report_weekly FROM projects WHERE id = $1`, projectID).Scan(&enabled)
	if noRows(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: get project sla report: %w", err)
	}
	return enabled, nil
}

// EnqueueDueSLAReports enqueues a weekly SLA report for each report-enabled project
// whose watermark is older than 7 days (or unset). It runs on the scheduler leader.
// The projects are claimed FOR UPDATE and their watermark bumped in the same
// transaction as the enqueue, so a report is emitted once per period even across
// replicas. Returns the number of reports enqueued.
func (s *Store) EnqueueDueSLAReports(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin sla reports: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx,
		`SELECT id, name FROM projects
		  WHERE sla_report_weekly
		    AND (sla_report_last_at IS NULL OR sla_report_last_at <= now() - interval '7 days')
		  FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("store: select due sla reports: %w", err)
	}
	type proj struct{ id, name string }
	var projects []proj
	for rows.Next() {
		var p proj
		if err := rows.Scan(&p.id, &p.name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan due project: %w", err)
		}
		projects = append(projects, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate due projects: %w", err)
	}

	now := time.Now()
	for _, p := range projects {
		report := domain.SLAReport{ProjectID: p.id, ProjectName: p.name, GeneratedAt: now}
		for _, w := range slaReportWindows {
			c, err := s.ProjectSLI(ctx, p.id, now.Add(-w.duration))
			if err != nil {
				return 0, err
			}
			report.Windows = append(report.Windows, domain.SLAReportWindow{
				Window: w.name, UptimePercent: sla.Uptime(c.Up, c.Total), Up: c.Up, Total: c.Total,
			})
		}
		payload, err := json.Marshal(report)
		if err != nil {
			return 0, fmt.Errorf("store: marshal sla report: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicSLAReport, payload); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE projects SET sla_report_last_at = $2 WHERE id = $1`, p.id, now); err != nil {
			return 0, fmt.Errorf("store: bump sla report watermark: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit sla reports: %w", err)
	}
	return len(projects), nil
}

const maintenanceColumns = "id, project_id, monitor_id, starts_at, ends_at, reason, created_at"

func scanMaintenance(row pgx.Row) (domain.MaintenanceWindow, error) {
	var (
		mw        domain.MaintenanceWindow
		monitorID *string
	)
	if err := row.Scan(&mw.ID, &mw.ProjectID, &monitorID, &mw.StartsAt, &mw.EndsAt, &mw.Reason, &mw.CreatedAt); err != nil {
		return domain.MaintenanceWindow{}, err
	}
	if monitorID != nil {
		mw.MonitorID = *monitorID
	}
	return mw, nil
}

// CreateMaintenanceWindow inserts a maintenance window (validated in domain).
func (s *Store) CreateMaintenanceWindow(ctx context.Context, mw domain.MaintenanceWindow) (domain.MaintenanceWindow, error) {
	if err := mw.Validate(); err != nil {
		return domain.MaintenanceWindow{}, fmt.Errorf("store: invalid maintenance window: %w", err)
	}
	var monitorID *string
	if mw.MonitorID != "" {
		monitorID = &mw.MonitorID
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO maintenance_windows (project_id, monitor_id, starts_at, ends_at, reason)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+maintenanceColumns,
		mw.ProjectID, monitorID, mw.StartsAt, mw.EndsAt, mw.Reason)
	created, err := scanMaintenance(row)
	if err != nil {
		return domain.MaintenanceWindow{}, fmt.Errorf("store: create maintenance window: %w", err)
	}
	return created, nil
}

// ListMaintenanceWindowsByProject lists a project's maintenance windows, newest first.
func (s *Store) ListMaintenanceWindowsByProject(ctx context.Context, projectID string) ([]domain.MaintenanceWindow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+maintenanceColumns+` FROM maintenance_windows WHERE project_id = $1 ORDER BY starts_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list maintenance windows: %w", err)
	}
	defer rows.Close()
	var out []domain.MaintenanceWindow
	for rows.Next() {
		mw, err := scanMaintenance(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan maintenance window: %w", err)
		}
		out = append(out, mw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate maintenance windows: %w", err)
	}
	return out, nil
}

// GetMaintenanceWindow returns a maintenance window by id.
func (s *Store) GetMaintenanceWindow(ctx context.Context, id string) (domain.MaintenanceWindow, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+maintenanceColumns+` FROM maintenance_windows WHERE id = $1`, id)
	mw, err := scanMaintenance(row)
	if noRows(err) {
		return domain.MaintenanceWindow{}, ErrNotFound
	}
	if err != nil {
		return domain.MaintenanceWindow{}, fmt.Errorf("store: get maintenance window: %w", err)
	}
	return mw, nil
}

// DeleteMaintenanceWindow removes a maintenance window.
func (s *Store) DeleteMaintenanceWindow(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM maintenance_windows WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete maintenance window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
