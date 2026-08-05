package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"git.example.com/monitoring/cerbix/internal/domain"
)

const escalationPolicyColumns = "id, project_id, name, repeat_last, steps, created_at, updated_at"
const oncallColumns = "id, project_id, name, shift_seconds, anchor_at, participants, created_at, updated_at"

func scanEscalationPolicy(row pgx.Row) (domain.EscalationPolicy, error) {
	var (
		p     domain.EscalationPolicy
		steps []byte
	)
	if err := row.Scan(&p.ID, &p.ProjectID, &p.Name, &p.RepeatLast, &steps, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return domain.EscalationPolicy{}, err
	}
	if len(steps) > 0 {
		if err := json.Unmarshal(steps, &p.Steps); err != nil {
			return domain.EscalationPolicy{}, fmt.Errorf("store: decode escalation steps: %w", err)
		}
	}
	return p, nil
}

// CreateEscalationPolicy inserts a validated policy.
func (s *Store) CreateEscalationPolicy(ctx context.Context, p domain.EscalationPolicy) (domain.EscalationPolicy, error) {
	if err := p.Validate(); err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: invalid escalation policy: %w", err)
	}
	steps, err := json.Marshal(p.Steps)
	if err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: encode escalation steps: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO escalation_policies (project_id, name, repeat_last, steps)
		 VALUES ($1,$2,$3,$4) RETURNING `+escalationPolicyColumns,
		p.ProjectID, p.Name, p.RepeatLast, steps)
	created, err := scanEscalationPolicy(row)
	if err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: create escalation policy: %w", err)
	}
	return created, nil
}

// GetEscalationPolicy returns a policy by id, or ErrNotFound.
func (s *Store) GetEscalationPolicy(ctx context.Context, id string) (domain.EscalationPolicy, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+escalationPolicyColumns+` FROM escalation_policies WHERE id = $1`, id)
	p, err := scanEscalationPolicy(row)
	if noRows(err) {
		return domain.EscalationPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: get escalation policy: %w", err)
	}
	return p, nil
}

// ListEscalationPolicies returns a project's policies (newest first).
func (s *Store) ListEscalationPolicies(ctx context.Context, projectID string) ([]domain.EscalationPolicy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+escalationPolicyColumns+` FROM escalation_policies WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list escalation policies: %w", err)
	}
	defer rows.Close()
	var out []domain.EscalationPolicy
	for rows.Next() {
		p, err := scanEscalationPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan escalation policy: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateEscalationPolicy updates a policy's mutable fields. ErrNotFound if gone.
func (s *Store) UpdateEscalationPolicy(ctx context.Context, p domain.EscalationPolicy) (domain.EscalationPolicy, error) {
	if err := p.Validate(); err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: invalid escalation policy: %w", err)
	}
	steps, err := json.Marshal(p.Steps)
	if err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: encode escalation steps: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE escalation_policies SET name = $2, repeat_last = $3, steps = $4, updated_at = now()
		  WHERE id = $1 RETURNING `+escalationPolicyColumns,
		p.ID, p.Name, p.RepeatLast, steps)
	updated, err := scanEscalationPolicy(row)
	if noRows(err) {
		return domain.EscalationPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.EscalationPolicy{}, fmt.Errorf("store: update escalation policy: %w", err)
	}
	return updated, nil
}

// DeleteEscalationPolicy removes a policy (monitors referencing it are detached by
// the ON DELETE SET NULL fk). ErrNotFound if gone.
func (s *Store) DeleteEscalationPolicy(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM escalation_policies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete escalation policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanOnCallSchedule(row pgx.Row) (domain.OnCallSchedule, error) {
	var (
		sc           domain.OnCallSchedule
		participants []byte
	)
	if err := row.Scan(&sc.ID, &sc.ProjectID, &sc.Name, &sc.ShiftSeconds, &sc.AnchorAt, &participants, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
		return domain.OnCallSchedule{}, err
	}
	if len(participants) > 0 {
		if err := json.Unmarshal(participants, &sc.Participants); err != nil {
			return domain.OnCallSchedule{}, fmt.Errorf("store: decode participants: %w", err)
		}
	}
	return sc, nil
}

// CreateOnCallSchedule inserts a validated rotation.
func (s *Store) CreateOnCallSchedule(ctx context.Context, sc domain.OnCallSchedule) (domain.OnCallSchedule, error) {
	if err := sc.Validate(); err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: invalid on-call schedule: %w", err)
	}
	participants, err := json.Marshal(sc.Participants)
	if err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: encode participants: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at, participants)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+oncallColumns,
		sc.ProjectID, sc.Name, sc.ShiftSeconds, sc.AnchorAt, participants)
	created, err := scanOnCallSchedule(row)
	if err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: create on-call schedule: %w", err)
	}
	return created, nil
}

// GetOnCallSchedule returns a schedule by id, or ErrNotFound.
func (s *Store) GetOnCallSchedule(ctx context.Context, id string) (domain.OnCallSchedule, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+oncallColumns+` FROM oncall_schedules WHERE id = $1`, id)
	sc, err := scanOnCallSchedule(row)
	if noRows(err) {
		return domain.OnCallSchedule{}, ErrNotFound
	}
	if err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: get on-call schedule: %w", err)
	}
	if sc.Overrides, err = loadOverrides(ctx, s.pool, sc.ID); err != nil {
		return domain.OnCallSchedule{}, err
	}
	return sc, nil
}

// ListOnCallSchedules returns a project's rotations (newest first).
func (s *Store) ListOnCallSchedules(ctx context.Context, projectID string) ([]domain.OnCallSchedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+oncallColumns+` FROM oncall_schedules WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list on-call schedules: %w", err)
	}
	defer rows.Close()
	var out []domain.OnCallSchedule
	for rows.Next() {
		sc, err := scanOnCallSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan on-call schedule: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// UpdateOnCallSchedule updates a rotation's mutable fields. ErrNotFound if gone.
func (s *Store) UpdateOnCallSchedule(ctx context.Context, sc domain.OnCallSchedule) (domain.OnCallSchedule, error) {
	if err := sc.Validate(); err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: invalid on-call schedule: %w", err)
	}
	participants, err := json.Marshal(sc.Participants)
	if err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: encode participants: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE oncall_schedules SET name = $2, shift_seconds = $3, anchor_at = $4, participants = $5, updated_at = now()
		  WHERE id = $1 RETURNING `+oncallColumns,
		sc.ID, sc.Name, sc.ShiftSeconds, sc.AnchorAt, participants)
	updated, err := scanOnCallSchedule(row)
	if noRows(err) {
		return domain.OnCallSchedule{}, ErrNotFound
	}
	if err != nil {
		return domain.OnCallSchedule{}, fmt.Errorf("store: update on-call schedule: %w", err)
	}
	return updated, nil
}

// DeleteOnCallSchedule removes a rotation. ErrNotFound if gone.
func (s *Store) DeleteOnCallSchedule(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM oncall_schedules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete on-call schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const overrideColumns = "id, schedule_id, channel_id, starts_at, ends_at, created_at"

// rowQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, so override loading works
// on the pool and inside a transaction.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadOverrides(ctx context.Context, q rowQuerier, scheduleID string) ([]domain.OnCallOverride, error) {
	rows, err := q.Query(ctx, `SELECT `+overrideColumns+` FROM oncall_overrides WHERE schedule_id = $1 ORDER BY starts_at`, scheduleID)
	if err != nil {
		return nil, fmt.Errorf("store: load overrides: %w", err)
	}
	defer rows.Close()
	var out []domain.OnCallOverride
	for rows.Next() {
		var o domain.OnCallOverride
		if err := rows.Scan(&o.ID, &o.ScheduleID, &o.ChannelID, &o.StartsAt, &o.EndsAt, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan override: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AddOnCallOverride inserts a validated override for a schedule.
func (s *Store) AddOnCallOverride(ctx context.Context, o domain.OnCallOverride) (domain.OnCallOverride, error) {
	if err := o.Validate(); err != nil {
		return domain.OnCallOverride{}, fmt.Errorf("store: invalid override: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO oncall_overrides (schedule_id, channel_id, starts_at, ends_at)
		 VALUES ($1,$2,$3,$4) RETURNING `+overrideColumns,
		o.ScheduleID, o.ChannelID, o.StartsAt, o.EndsAt)
	var created domain.OnCallOverride
	if err := row.Scan(&created.ID, &created.ScheduleID, &created.ChannelID, &created.StartsAt, &created.EndsAt, &created.CreatedAt); err != nil {
		return domain.OnCallOverride{}, fmt.Errorf("store: add override: %w", err)
	}
	return created, nil
}

// ListOnCallOverrides returns a schedule's overrides (soonest first).
func (s *Store) ListOnCallOverrides(ctx context.Context, scheduleID string) ([]domain.OnCallOverride, error) {
	return loadOverrides(ctx, s.pool, scheduleID)
}

// GetOnCallOverride returns an override by id (for access checks), or ErrNotFound.
func (s *Store) GetOnCallOverride(ctx context.Context, id string) (domain.OnCallOverride, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+overrideColumns+` FROM oncall_overrides WHERE id = $1`, id)
	var o domain.OnCallOverride
	if err := row.Scan(&o.ID, &o.ScheduleID, &o.ChannelID, &o.StartsAt, &o.EndsAt, &o.CreatedAt); err != nil {
		if noRows(err) {
			return domain.OnCallOverride{}, ErrNotFound
		}
		return domain.OnCallOverride{}, fmt.Errorf("store: get override: %w", err)
	}
	return o, nil
}

// DeleteOnCallOverride removes an override. ErrNotFound if gone.
func (s *Store) DeleteOnCallOverride(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM oncall_overrides WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EnabledChannelsByIDs returns the enabled notification channels among the given ids
// (missing/disabled ids are simply absent) — used to deliver a resolved escalation step.
func (s *Store) EnabledChannelsByIDs(ctx context.Context, ids []string) ([]domain.NotificationChannel, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+channelColumns+` FROM notification_channels WHERE id = ANY($1) AND enabled`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: channels by ids: %w", err)
	}
	defer rows.Close()
	return s.collectChannels(rows)
}

// AdvanceEscalations drives every open, unacknowledged auto-incident whose monitor has
// an escalation policy: it fires each ladder step whose offset from the incident start
// has elapsed (resolving targets — channels + whoever is on call — to concrete channel
// ids at fire time), latching progress on the incident so a step fires once. When all
// steps are done and the policy repeats, it re-sends the last step on the monitor's
// renotify cadence. Acknowledgement or recovery removes an incident from the query.
// Returns the number of step notifications enqueued. Leader-only.
func (s *Store) AdvanceEscalations(ctx context.Context) (fired int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin advance escalations: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return 0, fmt.Errorf("store: escalation now: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT i.id, i.monitor_id, i.started_at, i.escalation_step, i.last_escalated_at,
		        m.name, m.escalation_policy_id, m.renotify_seconds
		   FROM incidents i
		   JOIN monitors m ON m.id = i.monitor_id
		  WHERE i.source = 'auto' AND i.status <> 'resolved' AND i.acknowledged_at IS NULL
		    AND m.escalation_policy_id IS NOT NULL
		    -- Dependency suppression (D-0100): the ladder pauses while a
		    -- (transitive) parent is down — the parent's own alerting owns the
		    -- page. It resumes on the next tick after the parent recovers if this
		    -- monitor is still down.
		    AND NOT EXISTS (
		        WITH RECURSIVE anc AS (
		            SELECT d.depends_on_id, 1 AS depth
		              FROM monitor_dependencies d
		             WHERE d.monitor_id = m.id
		            UNION
		            SELECT d.depends_on_id, anc.depth + 1
		              FROM monitor_dependencies d
		              JOIN anc ON d.monitor_id = anc.depends_on_id
		             WHERE anc.depth < 10
		        )
		        SELECT 1 FROM anc JOIN monitors p ON p.id = anc.depends_on_id
		         WHERE p.status = 'down'
		    )
		  FOR UPDATE OF i`)
	if err != nil {
		return 0, fmt.Errorf("store: select escalating incidents: %w", err)
	}
	type openInc struct {
		id, monitorID, name, policyID string
		startedAt                     time.Time
		step                          int
		lastEscalated                 *time.Time
		renotifySeconds               int
	}
	var incs []openInc
	for rows.Next() {
		var o openInc
		if err := rows.Scan(&o.id, &o.monitorID, &o.startedAt, &o.step, &o.lastEscalated, &o.name, &o.policyID, &o.renotifySeconds); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan escalating incident: %w", err)
		}
		incs = append(incs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate escalating incidents: %w", err)
	}

	policies := map[string]domain.EscalationPolicy{}
	schedules := map[string]domain.OnCallSchedule{}
	loadPolicy := func(id string) (domain.EscalationPolicy, error) {
		if p, ok := policies[id]; ok {
			return p, nil
		}
		row := tx.QueryRow(ctx, `SELECT `+escalationPolicyColumns+` FROM escalation_policies WHERE id = $1`, id)
		p, err := scanEscalationPolicy(row)
		if err != nil {
			return domain.EscalationPolicy{}, err
		}
		policies[id] = p
		return p, nil
	}
	loadSchedule := func(id string) (domain.OnCallSchedule, error) {
		if sc, ok := schedules[id]; ok {
			return sc, nil
		}
		row := tx.QueryRow(ctx, `SELECT `+oncallColumns+` FROM oncall_schedules WHERE id = $1`, id)
		sc, err := scanOnCallSchedule(row)
		if err != nil {
			return domain.OnCallSchedule{}, err
		}
		if sc.Overrides, err = loadOverrides(ctx, tx, id); err != nil {
			return domain.OnCallSchedule{}, err
		}
		schedules[id] = sc
		return sc, nil
	}
	resolveTargets := func(step domain.EscalationStep) ([]string, error) {
		seen := map[string]bool{}
		var out []string
		add := func(id string) {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		for _, t := range step.Targets {
			switch t.Type {
			case domain.EscalationTargetChannel:
				add(t.ID)
			case domain.EscalationTargetSchedule:
				sc, err := loadSchedule(t.ID)
				if err != nil {
					if noRows(err) {
						continue // schedule deleted — skip its target
					}
					return nil, err
				}
				add(sc.OnCall(now))
			}
		}
		return out, nil
	}
	enqueueStep := func(inc openInc, step int, repeat bool, channelIDs []string) error {
		if len(channelIDs) == 0 {
			return nil // nothing resolved (e.g. empty schedule) — no-op, still advance
		}
		payload, err := json.Marshal(domain.EscalationStepAlert{
			IncidentID: inc.id, MonitorID: inc.monitorID, MonitorName: inc.name,
			Step: step, Repeat: repeat, ChannelIDs: channelIDs,
		})
		if err != nil {
			return fmt.Errorf("store: marshal escalation step: %w", err)
		}
		return enqueueOutboxTx(ctx, tx, domain.TopicEscalationStep, payload)
	}

	for _, inc := range incs {
		p, err := loadPolicy(inc.policyID)
		if err != nil {
			if noRows(err) {
				continue // policy deleted since the monitor referenced it
			}
			return 0, fmt.Errorf("store: load escalation policy: %w", err)
		}
		step := inc.step
		changed := false
		lastEsc := inc.lastEscalated
		// Fire every step whose offset from the incident start has elapsed.
		for step < len(p.Steps) {
			due := inc.startedAt.Add(time.Duration(p.Steps[step].AfterSeconds) * time.Second)
			if now.Before(due) {
				break
			}
			targets, err := resolveTargets(p.Steps[step])
			if err != nil {
				return 0, err
			}
			if err := enqueueStep(inc, step, false, targets); err != nil {
				return 0, err
			}
			if len(targets) > 0 {
				fired++
				t := now
				lastEsc = &t
			}
			step++
			changed = true
		}
		// All steps done: optionally repeat the last on the renotify cadence.
		if step >= len(p.Steps) && p.RepeatLast && inc.renotifySeconds > 0 && len(p.Steps) > 0 {
			ready := lastEsc == nil || !now.Before(lastEsc.Add(time.Duration(inc.renotifySeconds)*time.Second))
			if ready {
				last := len(p.Steps) - 1
				targets, err := resolveTargets(p.Steps[last])
				if err != nil {
					return 0, err
				}
				if err := enqueueStep(inc, last, true, targets); err != nil {
					return 0, err
				}
				if len(targets) > 0 {
					fired++
					t := now
					lastEsc = &t
					changed = true
				}
			}
		}
		if changed {
			if _, err := tx.Exec(ctx,
				`UPDATE incidents SET escalation_step = $2, last_escalated_at = $3 WHERE id = $1`,
				inc.id, step, lastEsc); err != nil {
				return 0, fmt.Errorf("store: latch escalation state: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit advance escalations: %w", err)
	}
	return fired, nil
}
