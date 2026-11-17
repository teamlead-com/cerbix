package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
// `revisionID` is the declaration the OUTAGE was computed from, resolved by the caller that computed
// it. It is not optional and not re-derived here: see `snapshotServiceMembersTx`.
func (s *Store) OpenServiceIncidentTx(
	ctx context.Context, tx pgx.Tx, serviceID, projectID, title string,
	state domain.ServiceAlertState, mapping domain.ImpactMapping,
	confirmedOver int, revisionID *string,
) (domain.Incident, bool, error) {
	// The impact is the mapping's answer for the state that confirmed the onset (FR-042 §4.2), and the
	// mapping it was classified under is recorded beside it (D8): a later edit of the mapping must not
	// rewrite why this incident has the impact it has. Nothing is written when the state cannot open one.
	resolved := mapping.Resolved()
	impact, ok := resolved.For(state)
	if !ok {
		return domain.Incident{}, false, fmt.Errorf("store: open service incident: state %q cannot open an incident", state)
	}
	if err := resolved.Validate(); err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: open service incident: %w", err)
	}
	var svcName, svcSlug string
	if err := tx.QueryRow(ctx,
		`SELECT name, slug FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID,
	).Scan(&svcName, &svcSlug); err != nil {
		if noRows(err) {
			return domain.Incident{}, false, fmt.Errorf("store: open service incident: service %s not found", serviceID)
		}
		return domain.Incident{}, false, fmt.Errorf("store: open service incident: read service: %w", err)
	}
	basis := domain.ImpactBasis{
		Source: domain.ImpactBasisServiceMapping, Mapping: &resolved, State: state,
		Service: &domain.ImpactBasisService{Name: svcName, Slug: svcSlug},
	}
	if err := basis.Validate(); err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: open service incident: %w", err)
	}
	basisJSON, err := json.Marshal(basis)
	if err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: marshal impact basis: %w", err)
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO incidents (project_id, service_id, title, status, impact, source, impact_basis)
		 VALUES ($1, $2, $3, 'investigating', $4, 'auto', $5::jsonb)
		 ON CONFLICT (service_id) WHERE source = 'auto' AND status <> 'resolved' AND service_id IS NOT NULL
		 DO NOTHING
		 RETURNING `+incidentColumns, projectID, serviceID, title, string(impact), string(basisJSON))
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

	if err := snapshotServiceMembersTx(ctx, tx, inc.ID, projectID, serviceID, revisionID); err != nil {
		return domain.Incident{}, false, err
	}
	// The opening update states WHAT confirmed it. An operator who finds an incident nobody typed must be
	// able to see the machine that opened it and why — and `confirmedOver` is the same number that governs
	// whether the service pages at all (§16.3).
	body := domain.ServiceIncidentOpeningNote(state, confirmedOver)
	if _, err := tx.Exec(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author) VALUES ($1, 'investigating', $2, 'system')`,
		inc.ID, body); err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: open service incident note: %w", err)
	}
	// The correlation attempt, enqueued in this same transaction on its own fenced topic
	// (FR-021 §14.3, the shape `CreateIncident` uses). FR-022 promises a service incident
	// its impact links, and the enqueue belongs HERE rather than in the evaluator: an
	// incident that exists without its correlation event is the dual-write this outbox
	// exists to make impossible.
	corr, err := json.Marshal(domain.IncidentCorrelation{IncidentID: inc.ID})
	if err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: marshal service incident correlation: %w", err)
	}
	if err := enqueueOutboxTx(ctx, tx, domain.TopicIncidentCorrelation, corr); err != nil {
		return domain.Incident{}, false, err
	}
	// The ladder this incident will run, frozen NOW. FR-023 §8 makes retroactive escalation a
	// non-goal, and the live read could not honour it: a policy attached later would find every
	// step's delay already elapsed against this incident's `started_at`. Taking the STEPS rather
	// than the policy id also closes the third route to the same defect — `escalation_policies`
	// has no version, so an in-place edit of its `steps` would otherwise move the ladder under an
	// incident already climbing it.
	//
	// No policy at open means no snapshot, and no snapshot means this incident never escalates.
	// That IS the non-goal: attaching a policy to a service starts the next incident's ladder, not
	// this one's.
	if err := snapshotEscalationPolicyTx(ctx, tx, inc.ID, projectID, serviceID); err != nil {
		return domain.Incident{}, false, err
	}
	// The LIFECYCLE event, which this path did not send and the monitor's auto-path always has.
	// `incident_event` is what reaches incident webhooks and the confirmed subscribers of every
	// status page that surfaces the project, so without it a service outage was visible in the
	// database and the UI while the people who asked to be told heard nothing. The service's own
	// `service_alert` does not cover this: it pages the service's RECIPIENTS, a different audience
	// from the page's subscribers.
	//
	// It sits after the ON CONFLICT branch on purpose. A concurrent evaluator that lost the race
	// returns above with `created == false` and enqueues nothing, so one opening announces once.
	seq, err := nextIncidentEventSeqTx(ctx, tx, inc.ID)
	if err != nil {
		return domain.Incident{}, false, err
	}
	opened, err := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentOpened, Incident: inc, Seq: seq,
	})
	if err != nil {
		return domain.Incident{}, false, fmt.Errorf("store: marshal service incident event: %w", err)
	}
	if err := enqueueOutboxTx(ctx, tx, domain.TopicIncidentEvent, opened); err != nil {
		return domain.Incident{}, false, err
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
//
// The revision is PASSED IN, not resolved here. Resolving it locally is what this once did, with
// `state = 'effective' ORDER BY effective_at DESC LIMIT 1` and no time bound — and `state` defaults to
// `'effective'` the moment a revision is authored (00064), while its `effective_at` sits on the next
// bucket boundary. So an edit at 12:00:30 opened a window until 12:01 in which an incident snapshotted
// the members of a declaration that was not yet governing anything, while the outage that opened it
// had been computed from the previous one. The evaluator already resolved the governing revision for
// the verdict; handing that exact id down is the only way the two can agree by construction.
//
// A NULL revision snapshots NOTHING. No declaration governs, so there is no membership to name, and
// falling back to "the latest effective" would reintroduce the defect one level down.
func snapshotServiceMembersTx(
	ctx context.Context, tx pgx.Tx, incidentID, projectID, serviceID string, revisionID *string,
) error {
	if revisionID == nil || *revisionID == "" {
		return nil
	}
	// `service_definition_members` already carries `monitor_name` as of the declaration — the effective
	// revision IS a snapshot of the membership, which is why this reads it instead of joining `monitors`:
	// a join would render an empty name for a member deleted since, and naming the members after the world
	// moved is the entire purpose of this table.
	// The revision is SCOPED to the service and project it was resolved for, not trusted as a bare
	// id. Passing the id down (rather than resolving it here) fixed which declaration is read; it
	// also made this query's only predicate a value from outside, and a caller that ever passes a
	// revision belonging to another tenant's service would have its members copied into this
	// incident's snapshot. Today's one caller passes the right value — which is an argument for
	// checking it here, not against.
	// Ownership is established FIRST, and its failure is an error rather than an empty answer. Doing
	// the check by joining inside the members query would make a revision belonging to another
	// service produce zero rows — indistinguishable from a declaration that genuinely names nobody,
	// and stored as `members: []` as though that were the truth about this incident. A boundary that
	// answers "nothing" when it means "not yours" is not fail-closed; it is quietly wrong.
	var owns bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM service_definition_revisions
		 WHERE id = $1::uuid AND service_id = $2 AND project_id = $3`,
		*revisionID, serviceID, projectID).Scan(&owns)
	if noRows(err) {
		return fmt.Errorf("store: snapshot members: revision %s does not belong to service %s",
			*revisionID, serviceID)
	}
	if err != nil {
		return fmt.Errorf("store: verify revision ownership: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT mem.monitor_id::text, min(mem.monitor_name), array_agg(mem.role ORDER BY mem.role)
		  FROM service_definition_members mem
		 WHERE mem.revision_id = $1::uuid
		 GROUP BY mem.monitor_id
		 ORDER BY min(mem.monitor_name)`, *revisionID)
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

// ResolveServiceIncidentTx resolves the service's OPEN auto-incident inside the caller's transaction, and
// reports whether it resolved one. It is the other half of `OpenServiceIncidentTx` and the two exist as a
// pair for a reason spelled out in the spec's D1b: an incident that opens by machine and closes only by
// hand is a trap with two edges — the operator reads "investigating" on a service that recovered hours ago,
// and because at most one auto-incident may be open per service, the NEXT outage cannot open one, so the
// second failure is invisible in the timeline.
//
// `source = 'auto'` and `status <> 'resolved'` are both in the WHERE: an incident a HUMAN resolved, or one a
// human opened, is left alone. A machine must not reopen or re-annotate a conclusion a person drew.
func (s *Store) ResolveServiceIncidentTx(
	ctx context.Context, tx pgx.Tx, serviceID, body string,
) (bool, error) {
	return resolveServiceIncidentTx(ctx, tx, serviceID, body)
}

// resolveServiceIncidentTx is the same operation without a receiver, so the lifecycle closes — which
// are package-level functions holding only a transaction — end the incident through the SAME code the
// evaluator's recovery uses. Two spellings of "resolve the service's incident" is how one of them
// comes to forget the lifecycle event.
// The clock is taken HERE, after this function's own lock on the incident, and the caller's `asOf`
// is deliberately not used for it.
//
// A caller's instant is post-ITS-locks, not post-THIS-one. The evaluator takes `asOf` when it opens
// its snapshot — before the config-row locks and long before this UPDATE can queue behind a manual
// writer holding the incident row. Passing it down produced a resolution stamped BEFORE the wait that
// preceded it, which is the same defect D-0177 fixed for the manual paths wearing a parameter as a
// disguise. `statement_timestamp()` read after the lock is the only instant that is provably later
// than everything this transaction waited for.
//
// The `asOf` parameter is gone rather than ignored: a clock argument that the function overrides is
// an invitation to believe it is being honoured.
func resolveServiceIncidentTx(
	ctx context.Context, tx pgx.Tx, serviceID, body string,
) (bool, error) {
	// Take the row FIRST and hold it, then read the clock. `FOR UPDATE` here is what makes the
	// timestamp below post-wait: without it the UPDATE's own lock wait happens after
	// `statement_timestamp()` has already been read.
	var incidentID string
	err := tx.QueryRow(ctx,
		`SELECT id FROM incidents
		  WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved'
		  FOR UPDATE`, serviceID).Scan(&incidentID)
	if noRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: lock service incident: %w", err)
	}
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return false, fmt.Errorf("store: post-lock clock: %w", err)
	}

	// The guarded write returns the whole resolved row, so the announcement below describes the
	// incident as it now IS rather than as a second read hopes it is.
	inc, err := scanIncident(tx.QueryRow(ctx,
		`UPDATE incidents
		    SET status = 'resolved', resolved_at = $2, updated_at = $2
		  WHERE id = $1
		 RETURNING `+incidentColumns, incidentID, asOf))
	if err != nil {
		if noRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: resolve service incident: %w", err)
	}
	upd, err := scanIncidentUpdate(tx.QueryRow(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author, created_at)
		 VALUES ($1, 'resolved', $2, 'system', $3) RETURNING `+incidentUpdateColumns,
		inc.ID, body, asOf))
	if err != nil {
		return false, fmt.Errorf("store: resolve service incident note: %w", err)
	}
	// The closing half of the lifecycle, for the same audience as the opening half. An ending that
	// never reaches the people who were told about the beginning is the worse of the two omissions:
	// they are still watching an outage the system knows ended.
	seq, err := nextIncidentEventSeqTx(ctx, tx, inc.ID)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentResolved, Incident: inc, Update: &upd, Seq: seq,
	})
	if err != nil {
		return false, fmt.Errorf("store: marshal service incident event: %w", err)
	}
	// The OUTBOX row too. Invariant 103 says "the incident, its timeline note and its outbox row",
	// and this line was still `enqueueOutboxTx`, whose `created_at`/`updated_at`/`next_attempt_at`
	// default to the transaction clock — so the event was scheduled before the close that produced
	// it, and in an interleave could become due ahead of an earlier incident's.
	if err := enqueueOutboxAtTx(ctx, tx, domain.TopicIncidentEvent, payload, asOf); err != nil {
		return false, err
	}
	return true, nil
}

// snapshotEscalationPolicyTx freezes the service's CURRENT escalation policy onto the incident. A
// service with no policy attached writes nothing, which is how "no ladder" is represented.
func snapshotEscalationPolicyTx(ctx context.Context, tx pgx.Tx, incidentID, projectID, serviceID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO incident_escalation_snapshots
		  (incident_id, project_id, policy_id, policy_name, repeat_last, steps, due_base)
		SELECT $1, $2, p.id, p.name, p.repeat_last, p.steps, i.started_at
		  FROM services s
		  JOIN escalation_policies p ON p.id = s.escalation_policy_id AND p.project_id = s.project_id
		  JOIN incidents i ON i.id = $1 AND i.project_id = $2
		 WHERE s.id = $3 AND s.project_id = $2
		ON CONFLICT (incident_id) DO NOTHING`, incidentID, projectID, serviceID)
	if err != nil {
		return fmt.Errorf("store: snapshot escalation policy: %w", err)
	}
	return nil
}

// raiseServiceIncidentImpactTx — FR-042 §4.3. Reports whether it raised. Errors are wrapped with
// %w so a caller can see SQLSTATE 40001.
//
// It raises the service's open auto-incident to `mapping[state]` of the mapping FROZEN on the
// incident (D8; the service's live mapping is never read, I4) when that ranks above the impact it
// has, and does nothing otherwise: impact never falls (D2). Idempotency is the rank comparison, not
// a marker lookup (R9) — an incident may rise more than once, and a `⚡ Context:`-style guard allows
// one note per incident for its whole life.
//
// It is the sibling of resolveServiceIncidentTx and follows its order — lock, then clock, then
// write (D-0177) — with one difference: the resolve runs once per incident, this runs on EVERY
// evaluation while the incident is open and almost always has nothing to do. So it looks before it
// locks. The unlocked read is safe to trust for "nothing to do": impact never falls, the frozen
// mapping never changes and `resolved` is terminal, so a stale answer can only say "raise" where the
// lock then says "already raised", never the reverse. Locking on every tick would turn every
// acknowledge during an outage into a 40001 for the evaluator's whole slice (§4.3, concurrency).
func raiseServiceIncidentImpactTx(ctx context.Context, tx pgx.Tx, serviceID string,
	state domain.ServiceAlertState) (bool, error) {
	// Step 0 — no lock.
	var (
		incidentID string
		current    domain.IncidentImpact
		rawBasis   []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT id, impact, impact_basis FROM incidents
		  WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved'`, serviceID).
		Scan(&incidentID, &current, &rawBasis)
	if noRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read service incident impact: %w", err)
	}
	if rawBasis == nil { // D12: an incident with no basis never rises.
		return false, nil
	}
	var basis domain.ImpactBasis
	if err := json.Unmarshal(rawBasis, &basis); err != nil {
		return false, fmt.Errorf("store: decode impact basis of incident %s: %w", incidentID, err)
	}
	if basis.Source != domain.ImpactBasisServiceMapping || basis.Mapping == nil {
		return false, nil
	}
	to, ok := basis.Mapping.For(state)
	if !ok || to.Rank() <= current.Rank() {
		return false, nil
	}

	// Step 1 — a rise is due: take the row and compare again on what the lock returns.
	var (
		status domain.IncidentStatus
		from   domain.IncidentImpact
	)
	err = tx.QueryRow(ctx,
		`SELECT id, status, impact FROM incidents
		  WHERE id = $1 AND status <> 'resolved'
		  FOR UPDATE`, incidentID).Scan(&incidentID, &status, &from)
	if noRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: lock service incident for impact rise: %w", err)
	}
	if to.Rank() <= from.Rank() {
		return false, nil
	}

	// Step 2 — the clock, read AFTER the lock: the caller's `asOf` predates whatever step 1 waited for.
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return false, fmt.Errorf("store: post-lock clock: %w", err)
	}

	// Step 3 — the raise. `updated_at` is not optional: the public page and the feeds read it.
	inc, err := scanIncident(tx.QueryRow(ctx,
		`UPDATE incidents
		    SET impact = $2,
		        impact_basis = jsonb_set(impact_basis, '{state}', to_jsonb($3::text)),
		        updated_at = $4
		  WHERE id = $1
		 RETURNING `+incidentColumns, incidentID, to, string(state), asOf))
	if err != nil {
		return false, fmt.Errorf("store: raise service incident impact: %w", err)
	}

	// Step 4 — the public note, with the LOCKED status: a rise does not move the lifecycle.
	upd, err := scanIncidentUpdate(tx.QueryRow(ctx,
		`INSERT INTO incident_updates (incident_id, status, body, author, created_at)
		 VALUES ($1, $2, $3, 'system', $4) RETURNING `+incidentUpdateColumns,
		inc.ID, status, domain.ImpactRaisedNote(from, to, state), asOf))
	if err != nil {
		return false, fmt.Errorf("store: impact rise note: %w", err)
	}

	// Step 5 — an incident.updated event on the existing topic (D16), carrying the from/to pair.
	seq, err := nextIncidentEventSeqTx(ctx, tx, inc.ID)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentUpdated, Incident: inc, Update: &upd, Seq: seq,
		ImpactRaised: &domain.ImpactRaise{From: from, To: to},
	})
	if err != nil {
		return false, fmt.Errorf("store: marshal impact rise event: %w", err)
	}
	if err := enqueueOutboxAtTx(ctx, tx, domain.TopicIncidentEvent, payload, asOf); err != nil {
		return false, err
	}
	return true, nil
}

// IncidentImpactBasis reads the basis of an incident's impact by its own call (FR-042 D15), scoped to
// the project at the store boundary like ListIncidentImpacts: `WHERE id = $1 AND project_id = $2`.
// (nil, nil) means the basis was not recorded (D12). ErrNotFound means no such incident in that
// project, or a malformed id.
func (s *Store) IncidentImpactBasis(ctx context.Context, projectID, incidentID string) (*domain.ImpactBasis, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT impact_basis FROM incidents WHERE id = $1 AND project_id = $2`,
		incidentID, projectID).Scan(&raw)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read impact basis: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var b domain.ImpactBasis
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("store: decode impact basis of incident %s: %w", incidentID, err)
	}
	return &b, nil
}
