package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/reliability"
)

// FR-021 §15.0 — the PAGE-scoped projection ([314] P1-3).
//
// The first implementation batched per PROJECT, which is not one snapshot at all: an org-level page
// legitimately spans many projects, so a page with components from 40 projects took 40 snapshots
// and its status was never linearized at one instant. And it produced no `uptime_90d` and no
// withheld reason, which invariant 69 requires with the §11.2/§11.3 semantics.
//
// So the unit of work is the PAGE: a set of (project_id, service_id) pairs, evaluated in ONE report
// snapshot with a statement count that does not grow with the page. Five statements, always:
//
//	1. scope, epoch, era and watermark for every pair
//	2. the observations the evaluator needs, for the union of members
//	3. the maintenance spans, for the same union
//	4. the 90-day sealed aggregate per service, grouped by definition revision
//	5. the daily strip rows for every service        (skipped entirely when history is not wanted)
//
// The withholding decision is NOT re-implemented here: `decideServiceWindow` is the one owner, and
// the authenticated report calls exactly the same function over exactly the same inputs.

// ServiceRef is one (project, service) pair — the page's unit, since a component carries its own
// project and an org page mixes them.
type ServiceRef struct {
	ProjectID string
	ServiceID string
}

// ServicePageProjection is everything the public render needs about one service.
type ServicePageProjection struct {
	ProjectID string
	ServiceID string
	// SLI is the customer-facing layer: healthy | degraded | down | unknown.
	SLI string
	// Excluded is true when a declared maintenance window is in force AT the snapshot instant.
	Excluded bool
	// Reason carries why a non-measured SLI is not measured (operator-facing only).
	Reason string
	// SealedThrough is the watermark. The 90-day window ends HERE, never at the page's clock.
	SealedThrough time.Time
	// SealedInWindow is whether a sealed fact exists INSIDE that window, and it governs the STRIP
	// only — never the status. A live-healthy service before its first seal is operational.
	SealedInWindow bool
	// Uptime is the 90-day availability over decidable time, or nil when it must be withheld.
	Uptime *float64
	// UptimeWithheld names why `Uptime` is absent: the §11.2/§11.3 reason, never an empty gap.
	UptimeWithheld string
	// Coverage is the decidable fraction of the window, so a quoted number carries its basis.
	Coverage float64
	// Daily is the sealed 90-day strip, ascending, at most 90 points, absent days omitted.
	Daily []ServiceDayPoint
}

// ServicePageProjections evaluates every service on a page at ONE instant.
func (s *Store) ServicePageProjections(
	ctx context.Context, refs []ServiceRef, withHistory bool,
) (map[string]ServicePageProjection, error) {
	out := make(map[string]ServicePageProjection, len(refs))
	projects, services := splitRefs(refs)
	if len(services) == 0 {
		return out, nil
	}
	tx, asOf, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit

	// (1) Scope, epoch, era and watermark. The pair filter is what makes ONE query cover a page
	// that spans projects: a service is matched only under the project its component declared, so a
	// crafted component cannot make another tenant's service resolve.
	type svcRow struct {
		projectID string
		members   []reliability.Member
		policies  domain.ServicePolicies
		hasEpoch  bool
		sealedThr *time.Time
		hasMat    bool
		era       time.Time
		sliCount  int
	}
	per := make(map[string]*svcRow, len(services))
	rows, err := tx.Query(ctx, `
		WITH want AS (SELECT * FROM unnest($1::uuid[], $2::uuid[]) AS t(project_id, service_id))
		SELECT s.id, s.project_id,
		       m.sealed_through, COALESCE(m.era_start, 'epoch'::timestamptz),
		       (m.service_id IS NOT NULL) AS has_materialization,
		       (SELECT count(*) FROM service_member_refs mr
		         WHERE mr.service_id = s.id AND mr.project_id = s.project_id AND mr.role = 'sli'),
		       e.id, e.snapshot, e.policies
		  FROM want w
		  JOIN services s ON s.id = w.service_id AND s.project_id = w.project_id
		  LEFT JOIN service_materialization m ON m.service_id = s.id
		  LEFT JOIN LATERAL (
		      SELECT ep.id, ep.snapshot, r.policies
		        FROM service_evaluation_epochs ep
		        JOIN service_definition_revisions r ON r.id = ep.revision_id
		       WHERE ep.service_id = s.id AND ep.state = 'effective' AND ep.effective_at <= $3
		       ORDER BY ep.effective_at DESC, ep.epoch_seq DESC
		       LIMIT 1
		  ) e ON true`, projects, services, asOf)
	if err != nil {
		return nil, fmt.Errorf("store: page projection scope: %w", err)
	}
	var allMembers []reliability.Member
	for rows.Next() {
		var sid, pid string
		var sealed *time.Time
		var era time.Time
		var sliCount int
		var hasMat bool
		var epochID *string
		var snapshot, policyJSON []byte
		if err := rows.Scan(&sid, &pid, &sealed, &era, &hasMat, &sliCount,
			&epochID, &snapshot, &policyJSON); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan page projection: %w", err)
		}
		r := &svcRow{projectID: pid, sealedThr: sealed, hasMat: hasMat, era: era.UTC(), sliCount: sliCount}
		per[sid] = r
		if epochID == nil {
			continue
		}
		members, policies, err := decodeEpochSnapshot(snapshot, policyJSON)
		if err != nil {
			rows.Close()
			return nil, err
		}
		r.hasEpoch, r.members, r.policies = true, members, policies
		allMembers = append(allMembers, members...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(per) == 0 {
		return out, nil
	}

	// (2)+(3) The evaluator inputs, once for the union of members across the whole page.
	//
	// Maintenance is kept PER PROJECT and never merged into one list. A span with an empty
	// MonitorID is project-wide and covers EVERY member the evaluator is given
	// (`reliability.MaintenanceSpan.covers`), so a single merged slice made project A's
	// project-wide window mark project B's service as under maintenance on the same org page —
	// a false PUBLIC status, and the defect [318] P0-1 caught. One query across all projects,
	// each row tagged with the project it governs.
	end := asOf.Add(time.Microsecond)
	var observations []reliability.Observation
	spansByProject := map[string][]reliability.MaintenanceSpan{}
	if len(allMembers) > 0 {
		if observations, err = observationsFor(ctx, tx, allMembers, asOf, end); err != nil {
			return nil, err
		}
		memberIDs := make([]string, 0, len(allMembers))
		for _, m := range allMembers {
			memberIDs = append(memberIDs, m.MonitorID)
		}
		spansByProject, err = maintenanceSpansByProject(ctx, tx, dedupe(projects), memberIDs, asOf, end)
		if err != nil {
			return nil, err
		}
	}

	// (4) The 90-day sealed aggregate per service, grouped by definition revision so the
	// spans-revisions rule can be applied without a second query.
	aggregates, err := pageWindowAggregates(ctx, tx, projects, services)
	if err != nil {
		return nil, err
	}

	// (5) The strips, in one statement for the whole page.
	daily := map[string][]ServiceDayPoint{}
	if withHistory {
		if daily, err = pageDailyStrips(ctx, tx, projects, services); err != nil {
			return nil, err
		}
	}

	for sid, r := range per {
		p := ServicePageProjection{
			ProjectID: r.projectID, ServiceID: sid,
			SLI: "unknown", Reason: "no_sli_declared",
			Daily: daily[sid],
		}
		if r.sealedThr != nil {
			p.SealedThrough = r.sealedThr.UTC()
		}
		if r.hasEpoch && len(r.members) > 0 {
			// ITS OWN project's spans, never the page's union.
			o := reliability.StateAt(r.members, observations, spansByProject[r.projectID], r.policies, asOf)
			p.Excluded = o.Availability == reliability.AvailExcluded
			switch o.Health {
			case reliability.HealthDown:
				p.SLI, p.Reason = "down", ""
			case reliability.HealthDegraded:
				p.SLI, p.Reason = "degraded", ""
			case reliability.HealthHealthy:
				p.SLI, p.Reason = "healthy", ""
			default:
				p.SLI = "unknown"
				if p.Excluded {
					p.Reason = "excluded_by_maintenance"
				} else {
					p.Reason = "no_decidable_observation"
				}
			}
		}
		// The window aggregate. The PRE-CHECKS mirror the authenticated report exactly — an empty
		// `sli[]` creates no watermark and has no SLO (invariant 41), while a declared SLI that has
		// not materialized yet is a sealed-coverage problem — and the decision itself is the SHARED
		// §11.2/§11.3 owner. Anything else here would be a second claim about one set of facts,
		// which is what this test caught the first version doing: it answered `no_sli` where the
		// report answered `storage_gap`.
		agg := aggregates[sid]
		p.SealedInWindow = agg.buckets > 0
		switch {
		case !r.hasMat:
			// No watermark row at all — the branch where the report distinguishes the two causes:
			// an empty `sli[]` creates no watermark and has no SLO (invariant 41), while a declared
			// SLI that has not materialized yet is a sealed-coverage problem.
			if r.sliCount == 0 {
				p.UptimeWithheld = domain.ServiceReportReasonNoSLI
			} else {
				p.UptimeWithheld = domain.ServiceReportReasonNothingSealed
			}
		case r.sealedThr == nil:
			p.UptimeWithheld = domain.ServiceReportReasonNothingSealed
		default:
			windowFrom := r.sealedThr.Add(-90 * 24 * time.Hour)
			expected := int64((90 * 24 * time.Hour) / domain.CanonicalBucket)
			v := decideServiceWindow(agg.d, agg.buckets, expected, agg.revisions, windowFrom, r.era)
			p.Uptime, p.Coverage = v.Availability, decidableCoverage(agg.d)
			if v.Availability == nil {
				p.UptimeWithheld = v.AggregateWithheld
				if p.UptimeWithheld == "" {
					p.UptimeWithheld = v.Reason
				}
			}
		}
		out[sid] = p
	}
	return out, tx.Commit(ctx)
}

// windowAgg is one service's 90-day sealed totals plus how many definition revisions produced them.
type windowAgg struct {
	d         domain.ReliabilityDurations
	buckets   int64
	revisions int
}

// pageWindowAggregates sums each service's sealed facts over ITS OWN window — every service has a
// different watermark, so the window is derived per row rather than from one page-wide instant.
func pageWindowAggregates(
	ctx context.Context, tx pgx.Tx, projects, services []string,
) (map[string]windowAgg, error) {
	out := map[string]windowAgg{}
	rows, err := tx.Query(ctx, `
		WITH want AS (SELECT * FROM unnest($1::uuid[], $2::uuid[]) AS t(project_id, service_id)),
		win AS (
		    SELECT w.service_id, w.project_id,
		           m.sealed_through AS ends_at,
		           m.sealed_through - interval '90 days' AS starts_at
		      FROM want w JOIN service_materialization m ON m.service_id = w.service_id
		)
		SELECT b.service_id, count(*)::bigint, count(DISTINCT e.revision_id)::int,
		       COALESCE(sum(b.good_us),0)::bigint, COALESCE(sum(b.bad_us),0)::bigint,
		       COALESCE(sum(b.unknown_us),0)::bigint, COALESCE(sum(b.excluded_us),0)::bigint
		  FROM service_reliability_buckets b
		  JOIN win ON win.service_id = b.service_id AND win.project_id = b.project_id
		  JOIN service_evaluation_epochs e ON e.id = b.epoch_id
		 WHERE b.state = 'sealed'
		   AND b.bucket_start >= win.starts_at AND b.bucket_start < win.ends_at
		 GROUP BY b.service_id`, projects, services)
	if err != nil {
		return nil, fmt.Errorf("store: page window aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var a windowAgg
		if err := rows.Scan(&sid, &a.buckets, &a.revisions,
			&a.d.GoodUs, &a.d.BadUs, &a.d.UnknownUs, &a.d.ExcludedUs); err != nil {
			return nil, fmt.Errorf("store: scan page aggregate: %w", err)
		}
		out[sid] = a
	}
	return out, rows.Err()
}

// pageDailyStrips builds every service's strip in ONE statement.
//
// The per-service rules are unchanged and each is load-bearing: UTC days converted in SQL (a
// session-time-zone truncation would shift every boundary while still calling them UTC days),
// NEWEST-first under the 90-point cap (a 90-day window whose watermark is not on a midnight
// boundary spans 91 calendar days, so an ascending cap drops the most recent day), sealed facts
// only, and days with zero decidable time omitted rather than published as 0%.
func pageDailyStrips(
	ctx context.Context, tx pgx.Tx, projects, services []string,
) (map[string][]ServiceDayPoint, error) {
	out := map[string][]ServiceDayPoint{}
	rows, err := tx.Query(ctx, `
		WITH want AS (SELECT * FROM unnest($1::uuid[], $2::uuid[]) AS t(project_id, service_id)),
		win AS (
		    SELECT w.service_id, w.project_id,
		           m.sealed_through AS ends_at,
		           m.sealed_through - interval '90 days' AS starts_at
		      FROM want w JOIN service_materialization m ON m.service_id = w.service_id
		),
		days AS (
		    SELECT b.service_id,
		           (b.bucket_start AT TIME ZONE 'UTC')::date AS day,
		           sum(b.good_us)               AS good_us,
		           sum(b.good_us + b.bad_us)    AS decidable_us,
		           sum(b.bucket_size_us)        AS total_us
		      FROM service_reliability_buckets b
		      JOIN win ON win.service_id = b.service_id AND win.project_id = b.project_id
		     WHERE b.state = 'sealed'
		       AND b.bucket_start >= win.starts_at AND b.bucket_start < win.ends_at
		     GROUP BY 1, 2
		    HAVING sum(b.good_us + b.bad_us) > 0
		),
		capped AS (
		    SELECT *, row_number() OVER (PARTITION BY service_id ORDER BY day DESC) AS newest
		      FROM days
		)
		SELECT service_id, day, good_us, decidable_us, total_us
		  FROM capped WHERE newest <= 90
		 ORDER BY service_id, day`, projects, services)
	if err != nil {
		return nil, fmt.Errorf("store: page daily strips: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var day time.Time
		var goodUS, decidableUS, totalUS int64
		if err := rows.Scan(&sid, &day, &goodUS, &decidableUS, &totalUS); err != nil {
			return nil, fmt.Errorf("store: scan page day: %w", err)
		}
		p := ServiceDayPoint{Day: day.UTC()}
		if decidableUS > 0 {
			p.Uptime = float64(goodUS) / float64(decidableUS) * 100
		}
		if totalUS > 0 {
			p.DecidableFraction = float64(decidableUS) / float64(totalUS)
		}
		out[sid] = append(out[sid], p)
	}
	return out, rows.Err()
}

// splitRefs turns the pair list into the two parallel arrays the queries above unnest, deduplicated
// so a page that renders one service twice still costs one row.
func splitRefs(refs []ServiceRef) (projects, services []string) {
	seen := make(map[ServiceRef]bool, len(refs))
	for _, r := range refs {
		if r.ProjectID == "" || r.ServiceID == "" || seen[r] {
			continue
		}
		seen[r] = true
		projects = append(projects, r.ProjectID)
		services = append(services, r.ServiceID)
	}
	return projects, services
}

// maintenanceSpansByProject reads the effective maintenance spans for a whole PAGE in ONE
// statement, grouped by the project that governs them.
//
// The grouping is the correctness half, not an optimization: a project-wide span (empty
// MonitorID) covers every member it is handed, so spans from different projects must never meet
// in the same evaluator call. The single statement is the cost half — a per-project loop made the
// public render O(projects), which is exactly the growth §15.0 forbids.
//
// The predicate mirrors `maintenanceSpansFor` verbatim, including the §10.9 rule that an archived
// window keeps its effect on already-sealed time and only an explicit annul removes it.
func maintenanceSpansByProject(
	ctx context.Context, tx pgx.Tx, projects, monitorIDs []string, start, end time.Time,
) (map[string][]reliability.MaintenanceSpan, error) {
	out := map[string][]reliability.MaintenanceSpan{}
	if len(projects) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT project_id, id, COALESCE(monitor_id::text, ''), starts_at,
		        LEAST(ends_at, COALESCE(cancel_effective_at, ends_at))
		   FROM maintenance_windows
		  WHERE project_id = ANY($1)
		    AND (monitor_id IS NULL OR monitor_id = ANY($2))
		    AND starts_at < $4
		    AND LEAST(ends_at, COALESCE(cancel_effective_at, ends_at)) > $3
		  ORDER BY project_id, id`, projects, monitorIDs, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: read page maintenance spans: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var projectID string
		var sp reliability.MaintenanceSpan
		if err := rows.Scan(&projectID, &sp.ID, &sp.MonitorID, &sp.From, &sp.To); err != nil {
			return nil, fmt.Errorf("store: scan page maintenance span: %w", err)
		}
		out[projectID] = append(out[projectID], sp)
	}
	return out, rows.Err()
}
