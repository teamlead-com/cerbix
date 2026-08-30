package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/reliability"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// FR-021 phase 2 — the reporting compute core (§11, §12.1). Everything is read from SEALED
// facts over [sealed_through − window, sealed_through): the window NEVER ends at now (§11.3).
// Numbers that cannot be honestly stated are absent with a status and a reason — never 100%,
// never 0× (§11.1, invariant 40). §11.2's "both must pass" is ENFORCED, not decoration: a
// storage gap withholds every aggregate, budget and burn rate, because rows that survived a
// hole cannot vouch for the window — and cannot even prove how many definition revisions the
// window really spans.
//
// One report is ONE database snapshot and ONE clock: the whole assembly runs inside a
// read-only REPEATABLE READ transaction whose first statement supplies `as_of` from
// statement_timestamp(). Independent pool reads would let a concurrent invalidation
// interleave a pre-mutation watermark with post-mutation facts — a state that never existed.

// minDecidableCoverage is §10.10's fixed threshold: below it a window is `partial`. It is
// deliberately not a knob.
const minDecidableCoverage = 0.95

// serviceBurnWindows is the fixed reporting pair (§12.2's card, D-0163). THIS PAIR never alerts —
// not "not yet": phase 5 gave services paging through the operator's declared `burn_rules` on
// `sla_targets` (§16.4), which is a different object with its own windows, latches and generations.
// The card stayed exactly as it was, so a reader who finds a burn alert in production must look at
// `burn_rules`, never here.
var serviceBurnWindows = []sla.Window{
	{Name: "1h", Duration: time.Hour},
	{Name: "6h", Duration: 6 * time.Hour},
}

// factSums is one (epoch × reconstruction-part) aggregation row.
type factSums struct {
	epochID    string
	epochSeq   int64
	revisionID string
	revision   int64
	reconPart  bool
	buckets    int64
	minBucket  time.Time
	maxBucket  time.Time
	d          domain.ReliabilityDurations
}

// serviceWindowVerdict is the §11.2/§11.3 decision about ONE window of sealed facts: the two
// honesty axes judged independently, and whether the aggregate may be quoted at all.
type serviceWindowVerdict struct {
	Status domain.ServiceReportStatus
	Reason string
	// AggregateWithheld is set when a number exists in the facts but may not be quoted as one.
	AggregateWithheld string
	// Availability is nil whenever the number is withheld — never a zero standing in for unknown.
	Availability *float64
}

// decideServiceWindow is the ONE owner of the withholding rule. The public status page and the
// authenticated report BOTH call it, because a page that quoted a number the report withholds (or
// the reverse) would be two different claims about the same facts — and §15.0 is explicit that the
// projection adds no second semantics owner.
//
// Worst-first, and each axis on its own: a window reaching before the materialization era is
// insufficient history; a gap that era cannot explain is a storage gap (the §10.5 watermark
// contract makes it unreachable — checked anyway, because "checked independently" is the invariant,
// not "derived from the watermark"); zero decidable time has nothing to average; low coverage keeps
// its number WITH the fraction and reason.
//
// The AGGREGATE additionally requires (a) storage continuity, because rows that survived a hole
// cannot vouch for the window and a hole can hide an entire definition revision, and (b) all facts
// under ONE definition revision (§12.1, invariant 43) — across revisions the segments are the whole
// answer, "not even labelled".
func decideServiceWindow(
	d domain.ReliabilityDurations, sealedBuckets, expectedBuckets int64, revisions int,
	from, era time.Time,
) serviceWindowVerdict {
	var v serviceWindowVerdict
	continuity := sealedBuckets == expectedBuckets
	coverage := decidableCoverage(d)
	measured := d.GoodUs + d.BadUs
	switch {
	case from.Before(era):
		v.Status, v.Reason = domain.ServiceReportInsufficientHistory, domain.ServiceReportReasonEraShort
	case !continuity:
		v.Status, v.Reason = domain.ServiceReportPartial, domain.ServiceReportReasonStorageGap
	case measured == 0:
		v.Status, v.Reason = domain.ServiceReportUnavailable, domain.ServiceReportReasonZeroDecidable
	case coverage < minDecidableCoverage:
		v.Status, v.Reason = domain.ServiceReportPartial, domain.ServiceReportReasonLowCoverage
	default:
		v.Status = domain.ServiceReportOK
	}
	if revisions > 1 {
		// The spans-revisions label is a CLAIM and is made only when the stored facts actually show
		// more than one revision ([170] P2-1): a zero-fact gap has nothing to span, and its known
		// reason is already the storage_gap status.
		v.AggregateWithheld = domain.ServiceReportReasonSpansRevisions
	}
	quotable := revisions == 1 && continuity && measured > 0 &&
		(v.Status == domain.ServiceReportOK ||
			(v.Status == domain.ServiceReportPartial && v.Reason == domain.ServiceReportReasonLowCoverage))
	if quotable {
		v.Availability = availabilityPercent(d)
	}
	return v
}

// beginReportSnapshot opens the one snapshot a report is assembled in and reads its clock.
func (s *Store) beginReportSnapshot(ctx context.Context) (pgx.Tx, time.Time, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: begin report snapshot: %w", err)
	}
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		_ = tx.Rollback(ctx)
		return nil, time.Time{}, fmt.Errorf("store: report clock: %w", err)
	}
	return tx, asOf.UTC(), nil
}

// ServiceReliabilityReport computes the §11 answer for one service and one window.
func (s *Store) ServiceReliabilityReport(ctx context.Context, projectID, serviceID string, window sla.Window) (domain.ServiceWindowReport, error) {
	tx, asOf, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return domain.ServiceWindowReport{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	rep, err := s.serviceReliabilityReportTx(ctx, tx, projectID, serviceID, window, asOf)
	if err != nil {
		return rep, err
	}
	return rep, tx.Commit(ctx)
}

// serviceReliabilityReportTx assembles the report entirely inside the given snapshot; asOf
// is that snapshot's own statement_timestamp — the DB clock, never the application's.
func (s *Store) serviceReliabilityReportTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, window sla.Window, asOf time.Time) (domain.ServiceWindowReport, error) {
	rep := domain.ServiceWindowReport{
		ServiceID: serviceID,
		Window:    window.Name,
		AsOf:      asOf,
		Segments:  []domain.ReliabilitySegment{},
	}

	// era_start, not materialization_start: the CURRENT contiguous era is what the §10.5
	// watermark guarantees and what every phase-2 window anchors inside (00071's declared
	// discontinuities live before it, and a window reaching past the era's left edge is
	// insufficient history, not a storage gap).
	var era time.Time
	var sealed, retractedAt, retractedTo *time.Time
	err := tx.QueryRow(ctx, `
		SELECT era_start, sealed_through, retracted_at, retracted_to
		  FROM service_materialization WHERE service_id = $1 AND project_id = $2`,
		serviceID, projectID).Scan(&era, &sealed, &retractedAt, &retractedTo)
	switch {
	case noRows(err):
		// No watermark row at all. First distinguish "no such service in this project" from
		// "a service with no SLO": answering a nonexistent or foreign ID with a 200-shaped
		// no_sli report would make the report an existence oracle and a wrong-tenant answer
		// at once (iter-0141: the API maps ErrNotFound to 404). Then: an empty sli[] creates
		// no watermark and has NO SLO (invariant 41); a declared SLI that has not
		// materialized yet is a sealed-coverage problem instead.
		var owned bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1 AND project_id = $2)`,
			serviceID, projectID).Scan(&owned); err != nil {
			return rep, fmt.Errorf("store: report service scope: %w", err)
		}
		if !owned {
			return rep, ErrNotFound
		}
		var sliMembers int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM service_member_refs
			 WHERE service_id = $1 AND project_id = $2 AND role = 'sli'`,
			serviceID, projectID).Scan(&sliMembers); err != nil {
			return rep, fmt.Errorf("store: report sli members: %w", err)
		}
		if sliMembers == 0 {
			rep.Status, rep.Reason = domain.ServiceReportUnavailable, domain.ServiceReportReasonNoSLI
		} else {
			rep.Status, rep.Reason = domain.ServiceReportInsufficientSealed, domain.ServiceReportReasonNothingSealed
		}
		return rep, nil
	case err != nil:
		return rep, fmt.Errorf("store: report watermark: %w", err)
	}
	rep.RetractedAt, rep.RetractedTo = retractedAt, retractedTo
	if sealed == nil {
		rep.Status, rep.Reason = domain.ServiceReportInsufficientSealed, domain.ServiceReportReasonNothingSealed
		// The objective is a CURRENT-VIEW parameter (§11.3), orthogonal to sealed data: an
		// operator who just set a target must be able to see it before the first bucket
		// seals (iter-0144 — without this, a stored objective was invisible until sealing
		// began, because no other read exposes service targets). No budget and no burn are
		// computed here: there is nothing to compute them FROM.
		if err := reportObjective(ctx, tx, serviceID, window.Name, &rep); err != nil {
			return rep, err
		}
		return rep, nil
	}
	rep.SealedThrough = sealed
	rep.From, rep.To = sealed.Add(-window.Duration), *sealed
	rep.ExpectedBuckets = int64(window.Duration / domain.CanonicalBucket)

	segments, err := reportSegments(ctx, tx, projectID, serviceID, rep.From, rep.To)
	if err != nil {
		return rep, err
	}
	revisions := map[string]bool{}
	for _, seg := range segments {
		rep.SealedBuckets += seg.buckets
		rep.Durations = addDurations(rep.Durations, seg.d)
		revisions[seg.revisionID] = true
		rep.Segments = append(rep.Segments, domain.ReliabilitySegment{
			RevisionID:             seg.revisionID,
			Revision:               seg.revision,
			EpochID:                seg.epochID,
			EpochSeq:               seg.epochSeq,
			From:                   seg.minBucket,
			To:                     seg.maxBucket.Add(domain.CanonicalBucket),
			Buckets:                seg.buckets,
			Durations:              seg.d,
			Availability:           availabilityPercent(seg.d),
			Coverage:               decidableCoverage(seg.d),
			DeclaredReconstruction: seg.reconPart,
		})
	}
	rep.StorageContinuity = rep.SealedBuckets == rep.ExpectedBuckets
	rep.Coverage = decidableCoverage(rep.Durations)

	measured := rep.Durations.GoodUs + rep.Durations.BadUs
	verdict := decideServiceWindow(rep.Durations, rep.SealedBuckets, rep.ExpectedBuckets,
		len(revisions), rep.From, era)
	rep.Status, rep.Reason, rep.AggregateWithheld = verdict.Status, verdict.Reason, verdict.AggregateWithheld
	quotable := verdict.Availability != nil
	if quotable {
		rep.Availability = verdict.Availability
	}

	// Objective, budget, burn: only from a SERVICE-scoped target for THIS window — never
	// defaulted, never borrowed from monitor or project scope. The budget always states
	// which objective produced it (§11.3).
	if err := reportObjective(ctx, tx, serviceID, window.Name, &rep); err != nil {
		return rep, err
	}
	if rep.Objective != nil {
		objective, objectiveAt := rep.Objective, rep.ObjectiveUpdatedAt
		if quotable {
			b := sla.ErrorBudget(*objective, rep.Durations.GoodUs, measured)
			rep.Budget = &domain.ServiceBudget{
				Objective:            b.Objective,
				ObjectiveUpdatedAt:   *objectiveAt,
				AllowedDowntimeRatio: b.AllowedDowntimeRatio,
				ActualDowntimeRatio:  b.ActualDowntimeRatio,
				RemainingRatio:       b.RemainingRatio,
				BurnedPercent:        b.BurnedPercent,
				Met:                  b.Met,
			}
		}
		burn, err := reportBurn(ctx, tx, projectID, serviceID, *objective, era, *sealed, asOf)
		if err != nil {
			return rep, err
		}
		rep.Burn = burn
	}

	// Repair-in-progress intervals are rendered as such, never as data (§12.1).
	rows, err := tx.Query(ctx, `
		SELECT range_start, range_end FROM service_repair_ranges
		 WHERE service_id = $1 AND project_id = $2 AND state IN ('pending', 'running')
		   AND range_end > $3 AND range_start < $4
		 ORDER BY range_start`, serviceID, projectID, rep.From, rep.To)
	if err != nil {
		return rep, fmt.Errorf("store: report repairing: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var iv domain.RepairingInterval
		if err := rows.Scan(&iv.From, &iv.To); err != nil {
			return rep, fmt.Errorf("store: scan repairing: %w", err)
		}
		if iv.From.Before(rep.From) {
			iv.From = rep.From
		}
		if iv.To.After(rep.To) {
			iv.To = rep.To
		}
		rep.Repairing = append(rep.Repairing, iv)
	}
	return rep, rows.Err()
}

// reportObjective loads the SERVICE-scoped target for the window into the report — never
// defaulted, never borrowed from another scope. It is called on the sealed path AND the
// nothing-sealed path: the objective is a current-view parameter, not a derived number.
func reportObjective(ctx context.Context, tx pgx.Tx, serviceID, window string, rep *domain.ServiceWindowReport) error {
	var objective *float64
	var objectiveAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT objective::float8, updated_at FROM sla_targets
		 WHERE service_id = $1 AND window_name = $2`,
		serviceID, window).Scan(&objective, &objectiveAt); err != nil && !noRows(err) {
		return fmt.Errorf("store: report objective: %w", err)
	}
	rep.Objective, rep.ObjectiveUpdatedAt = objective, objectiveAt
	return nil
}

// reportSegments aggregates sealed facts grouped by (epoch × reconstruction-part), ordered
// by first bucket. The reconstruction part is revision 1's retroactive range — buckets
// before CeilToBucket(created_at) when effective_at < created_at (§6.6) — split into its own
// segment so ONLY the backfilled span carries the declared-reconstruction label.
func reportSegments(ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time) ([]factSums, error) {
	var reconBoundary *time.Time
	{
		var createdAt, effectiveAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT created_at, effective_at FROM service_definition_revisions
			 WHERE service_id = $1 AND project_id = $2 AND revision = 1 AND state = 'effective'`,
			serviceID, projectID).Scan(&createdAt, &effectiveAt)
		if err != nil && !noRows(err) {
			return nil, fmt.Errorf("store: report revision 1: %w", err)
		}
		if err == nil && effectiveAt.Before(createdAt) {
			b := domain.CeilToBucket(createdAt)
			reconBoundary = &b
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT e.id, e.epoch_seq, e.revision_id, r.revision,
		       ($5::timestamptz IS NOT NULL AND r.revision = 1 AND b.bucket_start < $5) AS recon,
		       count(*), min(b.bucket_start), max(b.bucket_start),
		       COALESCE(sum(b.good_us), 0)::bigint, COALESCE(sum(b.bad_us), 0)::bigint,
		       COALESCE(sum(b.unknown_us), 0)::bigint, COALESCE(sum(b.excluded_us), 0)::bigint,
		       COALESCE(sum(b.healthy_us), 0)::bigint, COALESCE(sum(b.degraded_us), 0)::bigint,
		       COALESCE(sum(b.down_us), 0)::bigint, COALESCE(sum(b.health_unknown_us), 0)::bigint
		  FROM service_reliability_buckets b
		  JOIN service_evaluation_epochs e ON e.id = b.epoch_id
		  JOIN service_definition_revisions r ON r.id = e.revision_id
		 WHERE b.service_id = $1 AND b.project_id = $2
		   AND b.bucket_start >= $3 AND b.bucket_start < $4
		   AND b.state = 'sealed'
		 GROUP BY e.id, e.epoch_seq, e.revision_id, r.revision, recon
		 ORDER BY min(b.bucket_start)`,
		serviceID, projectID, from, to, reconBoundary)
	if err != nil {
		return nil, fmt.Errorf("store: report segments: %w", err)
	}
	defer rows.Close()
	var out []factSums
	for rows.Next() {
		var seg factSums
		if err := rows.Scan(&seg.epochID, &seg.epochSeq, &seg.revisionID, &seg.revision,
			&seg.reconPart, &seg.buckets, &seg.minBucket, &seg.maxBucket,
			&seg.d.GoodUs, &seg.d.BadUs, &seg.d.UnknownUs, &seg.d.ExcludedUs,
			&seg.d.HealthyUs, &seg.d.DegradedUs, &seg.d.DownUs, &seg.d.HealthUnknownUs); err != nil {
			return nil, fmt.Errorf("store: scan segment: %w", err)
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

// reportBurn computes the fixed reporting pair (D-0163) over sealed facts by asking §16.4's
// ONE burn math owner for its two durations — the same owner the alerting rule path calls
// with the rule's arbitrary pair, so the card and the page cannot disagree about whether a
// number may be quoted. The honesty rules and the staleness test live there
// (decideBurnWindow); this function owns only WHICH windows the card shows.
func reportBurn(ctx context.Context, tx pgx.Tx, projectID, serviceID string, objective float64, era, sealed, asOf time.Time) ([]domain.ServiceBurnWindow, error) {
	reqs := make([]burnWindowRequest, 0, len(serviceBurnWindows))
	for _, w := range serviceBurnWindows {
		reqs = append(reqs, burnWindowRequest{
			serviceID: serviceID, projectID: projectID,
			label: w.Name, duration: w.Duration,
			objective: objective, era: era, sealed: sealed,
		})
	}
	return computeBurnWindows(ctx, tx, reqs, asOf)
}

// ServiceHealthNow is the categorical LIVE signal (§11.3): a different named thing than the
// percentage, explicitly unstable, computed from PROVISIONAL inputs — the CURRENT bucket is
// evaluated ON READ through the same projection the materializer uses (epoch snapshot,
// ordinary-ingest heartbeats, maintenance spans, the pure reducer), and NOTHING here reads
// stored facts: a sealed HEALTHY bucket left by a stalled materializer must never impersonate
// "now". Diagnostics answers from monitors[] current statuses — §12.2's two layers, where a
// diagnostic monitor can never touch the customer-facing layer.
func (s *Store) ServiceHealthNow(ctx context.Context, projectID, serviceID string) (domain.ServiceHealthNow, error) {
	tx, asOf, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return domain.ServiceHealthNow{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	h, err := serviceHealthNowTx(ctx, tx, projectID, serviceID, asOf)
	if err != nil {
		return h, err
	}
	return h, tx.Commit(ctx)
}

// ServiceNeighbourHealth evaluates the two-layer health of a whole neighbour
// set in ONE report snapshot with a CONSTANT number of SQL statements (§14.4,
// invariant 60): four set-wise reads — scope+epoch, observations, maintenance
// spans, diagnostics — regardless of how many neighbours are asked for, then
// the SHARED pure evaluator (reliability.StateAt, the same one
// serviceHealthNowTx uses) applied per service. The per-neighbour loop this
// replaces issued up to five statements EACH, and the downstream fan-in is
// bounded only by the project service cap — 199 neighbours meant ~995
// statements for one detail GET ([288] P1-2).
//
// Semantics have ONE owner: the inputs are the same four relations
// serviceHealthNowTx reads, at the same instant, and the verdict is the same
// pure function; only the loading is batched. A neighbour that does not exist
// in the project is absent from the map rather than failing it.
func (s *Store) ServiceNeighbourHealth(ctx context.Context, projectID string, serviceIDs []string) (map[string]domain.ServiceHealthNow, error) {
	if len(serviceIDs) == 0 {
		return map[string]domain.ServiceHealthNow{}, nil
	}
	tx, asOf, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	out, err := serviceNeighbourHealthTx(ctx, tx, projectID, dedupe(serviceIDs), asOf)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// serviceNeighbourHealthTx is the batched body under an injectable snapshot and
// instant (the seam the fixed-statement-count regression drives).
func serviceNeighbourHealthTx(
	ctx context.Context, tx pgx.Tx, projectID string, ids []string, asOf time.Time,
) (map[string]domain.ServiceHealthNow, error) {
	out := make(map[string]domain.ServiceHealthNow, len(ids))

	// (1) Scope + the epoch in force at as_of, for every requested service at once.
	// The LATERAL mirrors epochAt's ordering exactly, so the resolved epoch is the
	// same row that owner would pick.
	type svcEpoch struct {
		members  []reliability.Member
		policies domain.ServicePolicies
		hasEpoch bool
	}
	perService := make(map[string]*svcEpoch, len(ids))
	rows, err := tx.Query(ctx, `
		SELECT s.id, e.id, e.snapshot, e.policies
		  FROM services s
		  LEFT JOIN LATERAL (
		      SELECT ep.id, ep.snapshot, r.policies
		        FROM service_evaluation_epochs ep
		        JOIN service_definition_revisions r ON r.id = ep.revision_id
		       WHERE ep.service_id = s.id AND ep.state = 'effective' AND ep.effective_at <= $3
		       ORDER BY ep.effective_at DESC, ep.epoch_seq DESC
		       LIMIT 1
		  ) e ON true
		 WHERE s.id = ANY($1) AND s.project_id = $2`, ids, projectID, asOf)
	if err != nil {
		return nil, fmt.Errorf("store: neighbour health scope: %w", err)
	}
	var allMembers []reliability.Member
	for rows.Next() {
		var sid string
		var epochID *string
		var snapshot, policyJSON []byte
		if err := rows.Scan(&sid, &epochID, &snapshot, &policyJSON); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan neighbour epoch: %w", err)
		}
		se := &svcEpoch{}
		perService[sid] = se
		if epochID == nil {
			continue
		}
		var snap []domain.EpochMember
		if err := json.Unmarshal(snapshot, &snap); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: decode neighbour epoch snapshot: %w", err)
		}
		if err := json.Unmarshal(policyJSON, &se.policies); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: decode neighbour policies: %w", err)
		}
		se.hasEpoch = true
		for _, m := range snap {
			member := reliability.Member{
				MonitorID:  m.MonitorID,
				Type:       m.Semantics.Type,
				Region:     m.Semantics.Region,
				Enabled:    m.Semantics.Enabled,
				StaleAfter: m.StaleAfter,
				ArmedAt:    m.ArmedAt,
			}
			se.members = append(se.members, member)
			allMembers = append(allMembers, member)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(perService) == 0 {
		return out, nil
	}

	// (2)+(3) The evaluator inputs, once for the UNION of every neighbour's members.
	// Extra rows are harmless: the pure evaluator indexes observations by monitor and
	// applies a span only to the members it names (or project-wide), so a neighbour
	// never sees another's data.
	end := asOf.Add(time.Microsecond)
	var observations []reliability.Observation
	var spans []reliability.MaintenanceSpan
	if len(allMembers) > 0 {
		if observations, err = observationsFor(ctx, tx, allMembers, asOf, end); err != nil {
			return nil, err
		}
		if spans, err = maintenanceSpansFor(ctx, tx, projectID, allMembers, asOf, end); err != nil {
			return nil, err
		}
	}

	// (4) Diagnostics: monitors[] current statuses for every requested service at once.
	type diag struct {
		failing   []string
		members   int
		undecided int
	}
	diags := make(map[string]*diag, len(perService))
	rows, err = tx.Query(ctx, `
		SELECT DISTINCT ref.service_id, m.name, m.status
		  FROM service_member_refs ref
		  JOIN monitors m ON m.id = ref.monitor_id
		 WHERE ref.service_id = ANY($1) AND ref.project_id = $2
		 ORDER BY ref.service_id, m.name`, ids, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: neighbour health members: %w", err)
	}
	for rows.Next() {
		var sid, name, status string
		if err := rows.Scan(&sid, &name, &status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan neighbour member: %w", err)
		}
		d := diags[sid]
		if d == nil {
			d = &diag{}
			diags[sid] = d
		}
		d.members++
		switch status {
		case string(domain.StatusDown):
			d.failing = append(d.failing, name)
		case string(domain.StatusUp):
		default:
			d.undecided++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The verdicts: the SAME pure evaluator and the SAME two-layer rules as
	// serviceHealthNowTx, per service, over the batched inputs.
	for sid, se := range perService {
		h := domain.ServiceHealthNow{Unstable: true, AsOf: asOf, SLI: "unknown", Diagnostics: "unknown"}
		if se.hasEpoch && len(se.members) > 0 {
			switch reliability.StateAt(se.members, observations, spans, se.policies, asOf).Health {
			case reliability.HealthDown:
				h.SLI = "down"
			case reliability.HealthDegraded:
				h.SLI = "degraded"
			case reliability.HealthHealthy:
				h.SLI = "healthy"
			}
		}
		if d := diags[sid]; d != nil {
			h.FailingMonitors = d.failing
			sort.Strings(h.FailingMonitors)
			switch {
			case len(h.FailingMonitors) > 0:
				h.Diagnostics = "failing"
			case d.undecided > 0 || d.members == 0:
				h.Diagnostics = "unknown"
			default:
				h.Diagnostics = "ok"
			}
		}
		out[sid] = h
	}
	return out, nil
}

// serviceHealthNowTx is the body under an injectable snapshot and instant — the explicit
// seam the boundary regressions drive (an observation, a stale deadline or a maintenance
// start landing EXACTLY at as_of is not reproducible through a wrapper that mints its own
// clock).
func serviceHealthNowTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, asOf time.Time) (domain.ServiceHealthNow, error) {
	h := domain.ServiceHealthNow{Unstable: true, AsOf: asOf, SLI: "unknown", Diagnostics: "unknown"}

	// Tenant scope first: the epoch lookup below is keyed by service alone (it serves the
	// materializer, which already owns a scoped row), so a wrong-project caller must be
	// stopped here rather than answered from another tenant's declaration — and a service
	// that does not exist in the project is ErrNotFound, not an unknown-shaped answer
	// (iter-0141: the API maps it to 404).
	var owned bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1 AND project_id = $2)`,
		serviceID, projectID).Scan(&owned); err != nil {
		return h, fmt.Errorf("store: health service scope: %w", err)
	}
	if !owned {
		return h, ErrNotFound
	}

	// The SLI layer: the declared semantics evaluated AT the as_of instant — not the worst
	// state anywhere in the current minute ([170] P1-1), not the left limit at as_of⁻
	// ([175] P1), and not any fixed-width window at all ([178] P1: freshness durations are
	// nanosecond-granular through time.ParseDuration, so a derived stale deadline can fall
	// strictly inside ANY window and split it). reliability.StateAt is a true POINT
	// evaluation — the reducer's own censusAt/aggregate keyed at as_of, right-continuous:
	// an observation, a stale deadline, a maintenance edge or an epoch boundary effective
	// exactly at as_of is included, sub-µs or not. The input reads keep their instant
	// bounds: observations with ts ≤ as_of (in force or carry-in; latestAt would ignore a
	// later row anyway — two independent fences against accepted-but-future inputs), spans
	// in force over the instant, the epoch resolved at as_of.
	end := asOf.Add(time.Microsecond)
	epochID, members, policies, err := epochAt(ctx, tx, serviceID, asOf)
	if err != nil {
		return h, err
	}
	if epochID != "" && len(members) > 0 {
		observations, err := observationsFor(ctx, tx, members, asOf, end)
		if err != nil {
			return h, err
		}
		spans, err := maintenanceSpansFor(ctx, tx, projectID, members, asOf, end)
		if err != nil {
			return h, err
		}
		// Excluded (a maintenance exclusion in force at as_of) and unknown both read as
		// "unknown" — the four declared categories, nothing invented.
		switch reliability.StateAt(members, observations, spans, policies, asOf).Health {
		case reliability.HealthDown:
			h.SLI = "down"
		case reliability.HealthDegraded:
			h.SLI = "degraded"
		case reliability.HealthHealthy:
			h.SLI = "healthy"
		}
	}

	// Diagnostics: monitors[] current statuses. Any DOWN member fails the layer; a member
	// that is neither up nor down (pending — never confirmed either way) makes the layer
	// UNKNOWN rather than silently passing as healthy.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT m.name, m.status
		  FROM service_member_refs ref
		  JOIN monitors m ON m.id = ref.monitor_id
		 WHERE ref.service_id = $1 AND ref.project_id = $2
		 ORDER BY m.name`,
		serviceID, projectID)
	if err != nil {
		return h, fmt.Errorf("store: health members: %w", err)
	}
	defer rows.Close()
	memberCount, undecided := 0, 0
	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			return h, fmt.Errorf("store: scan member: %w", err)
		}
		memberCount++
		switch status {
		case string(domain.StatusDown):
			h.FailingMonitors = append(h.FailingMonitors, name)
		case string(domain.StatusUp):
		default:
			undecided++
		}
	}
	if err := rows.Err(); err != nil {
		return h, err
	}
	sort.Strings(h.FailingMonitors)
	switch {
	case len(h.FailingMonitors) > 0:
		h.Diagnostics = "failing"
	case undecided > 0 || memberCount == 0:
		h.Diagnostics = "unknown"
	default:
		h.Diagnostics = "ok"
	}
	return h, nil
}

// ServiceReliabilitySeries returns hour or day rollups computed on read: exact integer sums
// of both axes over sealed canonical buckets, keyed by epoch and NEVER merged across an
// epoch boundary (§10.2, §12.1) — one step that spans a boundary yields one point per epoch.
// Provisional buckets — reachable in production through a repair of a range younger than the
// late-arrival grace — are rolled up separately (Provisional=true) so the timeline can render
// them at reduced opacity without ever mixing them into a sealed number. The existence check
// and the rollup read share ONE repeatable-read snapshot ([192] P1-4).
func (s *Store) ServiceReliabilitySeries(ctx context.Context, projectID, serviceID string, from, to time.Time, step time.Duration) ([]domain.ReliabilitySeriesPoint, error) {
	tx, _, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	points, err := serviceReliabilitySeriesTx(ctx, tx, projectID, serviceID, from, to, step)
	if err != nil {
		return points, err
	}
	return points, tx.Commit(ctx)
}

// serviceReliabilitySeriesTx runs the existence check and the rollup read in ONE snapshot
// ([192] P1-4): two pool statements let a concurrent service deletion produce a 200/empty
// answer assembled from two different worlds — existence from before the delete, data from
// after — when the promised answer for a gone service is ErrNotFound.
func serviceReliabilitySeriesTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time, step time.Duration) ([]domain.ReliabilitySeriesPoint, error) {
	var trunc string
	switch step {
	case time.Hour:
		trunc = "hour"
	case 24 * time.Hour:
		trunc = "day"
	default:
		return nil, fmt.Errorf("store: unsupported series step %s (hour or day)", step)
	}
	// A nonexistent or foreign service is ErrNotFound, not an empty series (iter-0141).
	var owned bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1 AND project_id = $2)`,
		serviceID, projectID).Scan(&owned); err != nil {
		return nil, fmt.Errorf("store: series service scope: %w", err)
	}
	if !owned {
		return nil, ErrNotFound
	}
	return reliabilityStepRollupTx(ctx, tx, projectID, serviceID, from, to, trunc)
}

// reliabilityStepRollupTx is the ONE rollup statement behind the series AND the change
// comparison (FR-025 D8, NFR-020): exact integer sums per (step × epoch × revision × state)
// over `[from, to)`. The comparison sums these cells further; it never issues a second
// aggregation of the facts, which is what makes its parity with the series true by
// construction rather than by test.
func reliabilityStepRollupTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time, trunc string) ([]domain.ReliabilitySeriesPoint, error) {
	// date_trunc over the UTC PROJECTION: session-TimeZone-proof, the same lesson the
	// adoption recovery learned (iter-0136).
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT (date_trunc('%s', b.bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS step_start,
		       b.epoch_id, e.revision_id, (b.state = 'provisional') AS provisional, count(*),
		       COALESCE(sum(b.good_us), 0)::bigint, COALESCE(sum(b.bad_us), 0)::bigint,
		       COALESCE(sum(b.unknown_us), 0)::bigint, COALESCE(sum(b.excluded_us), 0)::bigint,
		       COALESCE(sum(b.healthy_us), 0)::bigint, COALESCE(sum(b.degraded_us), 0)::bigint,
		       COALESCE(sum(b.down_us), 0)::bigint, COALESCE(sum(b.health_unknown_us), 0)::bigint
		  FROM service_reliability_buckets b
		  JOIN service_evaluation_epochs e ON e.id = b.epoch_id
		 WHERE b.service_id = $1 AND b.project_id = $2
		   AND b.bucket_start >= $3 AND b.bucket_start < $4
		 GROUP BY step_start, b.epoch_id, e.revision_id, provisional
		 ORDER BY step_start, min(b.bucket_start), provisional`, trunc),
		serviceID, projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: reliability series: %w", err)
	}
	defer rows.Close()
	// An empty interval is an EMPTY ARRAY on the wire ([192] P2-1), matching every other
	// list surface — never null.
	out := []domain.ReliabilitySeriesPoint{}
	for rows.Next() {
		var p domain.ReliabilitySeriesPoint
		if err := rows.Scan(&p.Start, &p.EpochID, &p.RevisionID, &p.Provisional, &p.Buckets,
			&p.Durations.GoodUs, &p.Durations.BadUs, &p.Durations.UnknownUs, &p.Durations.ExcludedUs,
			&p.Durations.HealthyUs, &p.Durations.DegradedUs, &p.Durations.DownUs, &p.Durations.HealthUnknownUs); err != nil {
			return nil, fmt.Errorf("store: scan series point: %w", err)
		}
		p.Start = p.Start.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── The before/after comparison (FR-025 D8) — an EXTENSION of the series owner ───────────

// ServiceReliabilityCompare is D8's read for one change identity: the group's terminal phase
// fixes T (its occurred_at floored to the canonical bucket); `horizon` ∈ {15m, 1h, 6h, 24h}
// (else `horizon_invalid`); a group with only `started` is `no_terminal_phase`; an identity
// with no row, or a foreign/unknown service, is ErrNotFound. The two sides are computed by
// serviceReliabilityCompareTx inside ONE report snapshot with the group read, so the change and
// the facts come from one instant. Nothing is stored or cached.
func (s *Store) ServiceReliabilityCompare(ctx context.Context, projectID, serviceID, source, externalID string, horizon time.Duration) (domain.ChangeComparison, error) {
	if !domain.ValidChangeCompareHorizon(horizon) {
		return domain.ChangeComparison{}, domain.NewChangeError(domain.ChangeErrHorizonInvalid, "horizon",
			"must be one of 15m|1h|6h|24h, got %s", horizon)
	}
	if err := domain.ValidateChangeSource(source); err != nil {
		return domain.ChangeComparison{}, err
	}
	if _, err := domain.ValidateChangeExternalID(externalID); err != nil {
		return domain.ChangeComparison{}, err
	}
	tx, asOf, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return domain.ChangeComparison{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	if err := serviceExistsOn(ctx, tx, projectID, serviceID); err != nil {
		return domain.ChangeComparison{}, err
	}
	group, err := changeGroupTx(ctx, tx, projectID, serviceID, source, externalID)
	if err != nil {
		return domain.ChangeComparison{}, err
	}
	terminal := group.TerminalPhase()
	if terminal == nil {
		return domain.ChangeComparison{}, domain.NewChangeError(domain.ChangeErrNoTerminalPhase, "phase",
			"the change has no terminal phase yet; only started is recorded")
	}
	cmp := domain.ChangeComparison{
		Source: source, ExternalID: externalID, Change: *terminal,
		T:       domain.FloorToBucket(terminal.OccurredAt),
		Horizon: horizon.String(),
		AsOf:    asOf,
	}
	cmp.Before, cmp.After, cmp.SealedThrough, err = serviceReliabilityCompareTx(ctx, tx, projectID, serviceID, cmp.T, horizon)
	if err != nil {
		return domain.ChangeComparison{}, err
	}
	if cmp.Before.Figure != nil && cmp.After.Figure != nil {
		d := cmp.After.Figure.Availability - cmp.Before.Figure.Availability
		cmp.Delta = &d
	}
	return cmp, tx.Commit(ctx)
}

// serviceReliabilityCompareTx is serviceReliabilitySeriesTx EXTENDED with a bucket-aligned range
// sum (D8): for `[T − h, T)` and `[T, T + h)` it sums the SAME hourly rollup cells the series
// returns (reliabilityStepRollupTx), keeps only sealed cells (provisional time never enters a
// stated number, as on the page), and applies the page's own verdict — decideServiceWindow,
// the ONE owner of the withholding rule — over the summed durations, continuity and era. Each
// side is a figure or is withheld with one reason from the page's vocabulary:
//
//   - `pending` (after only): T + h is beyond sealed_through — stated, no partial figure;
//   - `no_facts`: no sealed bucket in the range (also both sides when nothing is sealed);
//   - `definition_changed`: more than one epoch or revision inside the side;
//   - `undecidable`: the page would withhold the aggregate — the page's reason rides in Detail
//     (`storage_gap`, `zero_decidable_time`, `window_precedes_materialization_era`, and for a
//     `before` side that reaches past the watermark, `sealed_through_behind_window`).
//
// The watermark is read in the same snapshot; the third result is sealed_through (nil when the
// service has sealed nothing).
func serviceReliabilityCompareTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, t time.Time, h time.Duration) (before, after domain.ChangeCompareSide, sealedThrough *time.Time, err error) {
	var era time.Time
	var sealed *time.Time
	err = tx.QueryRow(ctx, `
		SELECT era_start, sealed_through FROM service_materialization
		 WHERE service_id = $1 AND project_id = $2`, serviceID, projectID).Scan(&era, &sealed)
	if err != nil && !noRows(err) {
		return before, after, nil, fmt.Errorf("store: compare watermark: %w", err)
	}
	if sealed != nil {
		u := sealed.UTC()
		sealed = &u
	}
	before, err = compareSideTx(ctx, tx, projectID, serviceID, t.Add(-h), t, era, sealed, false)
	if err != nil {
		return before, after, sealed, err
	}
	after, err = compareSideTx(ctx, tx, projectID, serviceID, t, t.Add(h), era, sealed, true)
	return before, after, sealed, err
}

func compareSideTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to, era time.Time, sealed *time.Time, isAfter bool) (domain.ChangeCompareSide, error) {
	side := domain.ChangeCompareSide{From: from, To: to}
	withhold := func(reason, detail string) domain.ChangeCompareSide {
		side.Withheld = &domain.ChangeCompareWithheld{Reason: reason, Detail: detail, SealedThrough: sealed}
		return side
	}
	if sealed == nil {
		return withhold(domain.ChangeCompareWithheldNoFacts, ""), nil
	}
	// The sealed_through clamp: a side that ends beyond the watermark is not yet a fact.
	if to.After(*sealed) {
		if isAfter {
			return withhold(domain.ChangeCompareWithheldPending, ""), nil
		}
		return withhold(domain.ChangeCompareWithheldUndecidable, domain.ServiceReportReasonStaleWatermark), nil
	}
	points, err := reliabilityStepRollupTx(ctx, tx, projectID, serviceID, from, to, "hour")
	if err != nil {
		return side, err
	}
	var d domain.ReliabilityDurations
	var buckets int64
	epochs, revisions := map[string]bool{}, map[string]bool{}
	for _, p := range points {
		if p.Provisional {
			continue
		}
		d = addDurations(d, p.Durations)
		buckets += p.Buckets
		epochs[p.EpochID] = true
		revisions[p.RevisionID] = true
	}
	if buckets == 0 {
		return withhold(domain.ChangeCompareWithheldNoFacts, ""), nil
	}
	if len(epochs) > 1 || len(revisions) > 1 {
		return withhold(domain.ChangeCompareWithheldDefinitionChanged, ""), nil
	}
	expected := int64(to.Sub(from) / domain.CanonicalBucket)
	verdict := decideServiceWindow(d, buckets, expected, 1, from, era)
	if verdict.Availability == nil {
		return withhold(domain.ChangeCompareWithheldUndecidable, verdict.Reason), nil
	}
	side.Figure = &domain.ChangeCompareFigure{
		Availability:    *verdict.Availability,
		GoodSeconds:     float64(d.GoodUs) / 1e6,
		BadSeconds:      float64(d.BadUs) / 1e6,
		UnknownSeconds:  float64(d.UnknownUs) / 1e6,
		ExcludedSeconds: float64(d.ExcludedUs) / 1e6,
		Buckets:         buckets,
		Durations:       d,
	}
	return side, nil
}

// UpsertServiceSLATarget sets the service-scoped objective for one window. Burn alerting on
// a service scope is rejected here AND by the schema CHECK — reporting only until phase 5
// (§13, invariant 47). There is deliberately no burnAlert parameter: the application layer
// does not even offer the knob.
func (s *Store) UpsertServiceSLATarget(ctx context.Context, projectID, serviceID, window string, objective float64) error {
	if _, ok := sla.WindowByName(window); !ok {
		return fmt.Errorf("store: unknown SLA window %q", window)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective)
		SELECT s.id, $3, $4 FROM services s WHERE s.id = $1 AND s.project_id = $2
		ON CONFLICT (service_id, window_name) WHERE service_id IS NOT NULL
		DO UPDATE SET objective = EXCLUDED.objective, updated_at = now()`,
		serviceID, projectID, window, objective)
	if err != nil {
		return fmt.Errorf("store: upsert service SLA target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListServiceSLATargets is the service's SLO target INVENTORY (FR-024 AC-0163-8): every window
// it has declared an objective for, in canonical window order (24h, 7d, 30d, 90d — the order of
// sla.StandardWindows). It exists because no other read answers "which windows does this
// service have a target for?": the reliability report states its objective only once a seal
// watermark exists, so a brand-new service with a target looks target-less there, and the gate
// policy editor must offer exactly the windows a target exists for.
//
// ONE query: the service row is the anchor (LEFT JOIN), so a service in the project with no
// targets is an EMPTY, non-nil slice, while a service that is not in the project is ErrNotFound
// — the two are different answers and a client must not have to guess which one it got.
func (s *Store) ListServiceSLATargets(ctx context.Context, projectID, serviceID string) ([]domain.ServiceSLATarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.window_name, t.objective::float8, t.updated_at
		  FROM services s
		  LEFT JOIN sla_targets t ON t.service_id = s.id
		 WHERE s.id = $1 AND s.project_id = $2
		 ORDER BY CASE t.window_name
		            WHEN '24h' THEN 1 WHEN '7d' THEN 2 WHEN '30d' THEN 3 WHEN '90d' THEN 4
		            ELSE 5 END, t.window_name`,
		serviceID, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list service SLA targets: %w", err)
	}
	defer rows.Close()
	out := []domain.ServiceSLATarget{}
	found := false
	for rows.Next() {
		found = true
		var window *string
		var objective *float64
		var updatedAt *time.Time
		if err := rows.Scan(&window, &objective, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan service SLA target: %w", err)
		}
		if window == nil {
			continue // the service row alone: no target declared
		}
		out = append(out, domain.ServiceSLATarget{Window: *window, Objective: *objective, UpdatedAt: updatedAt.UTC()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list service SLA targets: %w", err)
	}
	if !found {
		return nil, ErrNotFound
	}
	return out, nil
}

func addDurations(a, b domain.ReliabilityDurations) domain.ReliabilityDurations {
	a.GoodUs += b.GoodUs
	a.BadUs += b.BadUs
	a.UnknownUs += b.UnknownUs
	a.ExcludedUs += b.ExcludedUs
	a.HealthyUs += b.HealthyUs
	a.DegradedUs += b.DegradedUs
	a.DownUs += b.DownUs
	a.HealthUnknownUs += b.HealthUnknownUs
	return a
}

// availabilityPercent is §11.1's formula as a percentage (0..100, matching the product's
// monitor SLO display), nil on a zero denominator — never 100% (invariant 40). excluded_us
// appears in NO denominator.
func availabilityPercent(d domain.ReliabilityDurations) *float64 {
	measured := d.GoodUs + d.BadUs
	if measured <= 0 {
		return nil
	}
	v := float64(d.GoodUs) / float64(measured) * 100
	return &v
}

// decidableCoverage is §11.1's coverage fraction; 0 on a zero denominator.
func decidableCoverage(d domain.ReliabilityDurations) float64 {
	total := d.GoodUs + d.BadUs + d.UnknownUs
	if total <= 0 {
		return 0
	}
	return float64(d.GoodUs+d.BadUs) / float64(total)
}
