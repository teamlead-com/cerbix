package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// gateAllWindowReportsTx loads every configured target window with a fixed number of
// statements. The all-window gate must not turn its bounded four-window inventory into N
// independent report reads: the service watermark, sealed segments, burn windows and repair
// ranges are each loaded once for the complete snapshot inventory.
func gateAllWindowReportsTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, targets []gateTarget, asOf time.Time,
) ([]domain.ServiceWindowReport, error) {
	reports := make([]domain.ServiceWindowReport, len(targets))
	if len(targets) == 0 {
		return reports, nil
	}
	for index, target := range targets {
		reports[index] = domain.ServiceWindowReport{
			ServiceID: serviceID,
			Window:    target.window,
			AsOf:      asOf,
			Segments:  []domain.ReliabilitySegment{},
		}
		objective, objectiveAt := target.objective, target.objectiveUpdatedAt
		reports[index].Objective, reports[index].ObjectiveUpdatedAt = &objective, &objectiveAt
	}

	var era time.Time
	var sealed, retractedAt, retractedTo *time.Time
	err := tx.QueryRow(ctx, `
		SELECT era_start, sealed_through, retracted_at, retracted_to
		  FROM service_materialization WHERE service_id = $1 AND project_id = $2`,
		serviceID, projectID).Scan(&era, &sealed, &retractedAt, &retractedTo)
	if err != nil && !noRows(err) {
		return nil, fmt.Errorf("store: all-window report watermark: %w", err)
	}
	if noRows(err) || sealed == nil {
		for index := range reports {
			reports[index].Status = domain.ServiceReportInsufficientSealed
			reports[index].Reason = domain.ServiceReportReasonNothingSealed
		}
		return reports, nil
	}

	segments, err := gateAllWindowSegmentsTx(ctx, tx, projectID, serviceID, targets, *sealed)
	if err != nil {
		return nil, err
	}
	burnRequests := make([]burnWindowRequest, 0, len(targets)*len(serviceBurnWindows))
	for index, target := range targets {
		window, ok := sla.WindowByName(target.window)
		if !ok {
			return nil, fmt.Errorf("store: gate decision target window %q is not an SLA window", target.window)
		}
		rep := &reports[index]
		sealedAt := sealed.UTC()
		rep.SealedThrough = &sealedAt
		rep.From, rep.To = sealedAt.Add(-window.Duration), sealedAt
		rep.ExpectedBuckets = int64(window.Duration / domain.CanonicalBucket)
		rep.RetractedAt, rep.RetractedTo = retractedAt, retractedTo
		populateGateWindowReport(rep, segments[target.window], era)

		for _, burnWindow := range serviceBurnWindows {
			burnRequests = append(burnRequests, burnWindowRequest{
				serviceID: serviceID, projectID: projectID,
				label: burnWindow.Name, duration: burnWindow.Duration,
				objective: target.objective, era: era, sealed: sealedAt,
			})
		}
	}

	burn, err := computeBurnWindows(ctx, tx, burnRequests, asOf)
	if err != nil {
		return nil, err
	}
	for index := range reports {
		start := index * len(serviceBurnWindows)
		reports[index].Burn = burn[start : start+len(serviceBurnWindows)]
	}
	if err := gateAllWindowRepairingTx(ctx, tx, projectID, serviceID, reports); err != nil {
		return nil, err
	}
	return reports, nil
}

func gateAllWindowSegmentsTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, targets []gateTarget, sealed time.Time,
) (map[string][]factSums, error) {
	var reconBoundary *time.Time
	var createdAt, effectiveAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT created_at, effective_at FROM service_definition_revisions
		 WHERE service_id = $1 AND project_id = $2 AND revision = 1 AND state = 'effective'`,
		serviceID, projectID).Scan(&createdAt, &effectiveAt)
	if err != nil && !noRows(err) {
		return nil, fmt.Errorf("store: all-window report revision 1: %w", err)
	}
	if err == nil && effectiveAt.Before(createdAt) {
		boundary := domain.CeilToBucket(createdAt)
		reconBoundary = &boundary
	}

	windows := make([]string, len(targets))
	from := make([]time.Time, len(targets))
	to := make([]time.Time, len(targets))
	for index, target := range targets {
		window, ok := sla.WindowByName(target.window)
		if !ok {
			return nil, fmt.Errorf("store: gate decision target window %q is not an SLA window", target.window)
		}
		windows[index], from[index], to[index] = target.window, sealed.Add(-window.Duration), sealed
	}
	rows, err := tx.Query(ctx, `
		WITH want AS (
		    SELECT * FROM unnest($1::text[], $2::timestamptz[], $3::timestamptz[])
		        AS t(window_name, from_ts, to_ts)
		)
		SELECT w.window_name, e.id, e.epoch_seq, e.revision_id, r.revision,
		       ($6::timestamptz IS NOT NULL AND r.revision = 1 AND b.bucket_start < $6) AS recon,
		       count(*), min(b.bucket_start), max(b.bucket_start),
		       COALESCE(sum(b.good_us), 0)::bigint, COALESCE(sum(b.bad_us), 0)::bigint,
		       COALESCE(sum(b.unknown_us), 0)::bigint, COALESCE(sum(b.excluded_us), 0)::bigint,
		       COALESCE(sum(b.healthy_us), 0)::bigint, COALESCE(sum(b.degraded_us), 0)::bigint,
		       COALESCE(sum(b.down_us), 0)::bigint, COALESCE(sum(b.health_unknown_us), 0)::bigint
		  FROM want w
		  JOIN service_reliability_buckets b
		    ON b.service_id = $4 AND b.project_id = $5
		   AND b.bucket_start >= w.from_ts AND b.bucket_start < w.to_ts
		   AND b.state = 'sealed'
		  JOIN service_evaluation_epochs e ON e.id = b.epoch_id
		  JOIN service_definition_revisions r ON r.id = e.revision_id
		 GROUP BY w.window_name, e.id, e.epoch_seq, e.revision_id, r.revision, recon`,
		windows, from, to, serviceID, projectID, reconBoundary)
	if err != nil {
		return nil, fmt.Errorf("store: all-window report segments: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]factSums, len(targets))
	for rows.Next() {
		var window string
		var segment factSums
		if err := rows.Scan(&window, &segment.epochID, &segment.epochSeq, &segment.revisionID, &segment.revision,
			&segment.reconPart, &segment.buckets, &segment.minBucket, &segment.maxBucket,
			&segment.d.GoodUs, &segment.d.BadUs, &segment.d.UnknownUs, &segment.d.ExcludedUs,
			&segment.d.HealthyUs, &segment.d.DegradedUs, &segment.d.DownUs, &segment.d.HealthUnknownUs); err != nil {
			return nil, fmt.Errorf("store: scan all-window report segment: %w", err)
		}
		out[window] = append(out[window], segment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: all-window report segments: %w", err)
	}
	for window := range out {
		sort.Slice(out[window], func(i, j int) bool { return out[window][i].minBucket.Before(out[window][j].minBucket) })
	}
	return out, nil
}

func populateGateWindowReport(rep *domain.ServiceWindowReport, segments []factSums, era time.Time) {
	revisions := make(map[string]bool, len(segments))
	for _, segment := range segments {
		rep.SealedBuckets += segment.buckets
		rep.Durations = addDurations(rep.Durations, segment.d)
		revisions[segment.revisionID] = true
		rep.Segments = append(rep.Segments, domain.ReliabilitySegment{
			RevisionID:             segment.revisionID,
			Revision:               segment.revision,
			EpochID:                segment.epochID,
			EpochSeq:               segment.epochSeq,
			From:                   segment.minBucket,
			To:                     segment.maxBucket.Add(domain.CanonicalBucket),
			Buckets:                segment.buckets,
			Durations:              segment.d,
			Availability:           availabilityPercent(segment.d),
			Coverage:               decidableCoverage(segment.d),
			DeclaredReconstruction: segment.reconPart,
		})
	}
	rep.StorageContinuity = rep.SealedBuckets == rep.ExpectedBuckets
	rep.Coverage = decidableCoverage(rep.Durations)
	verdict := decideServiceWindow(rep.Durations, rep.SealedBuckets, rep.ExpectedBuckets, len(revisions), rep.From, era)
	rep.Status, rep.Reason, rep.AggregateWithheld = verdict.Status, verdict.Reason, verdict.AggregateWithheld
	if verdict.Availability == nil {
		return
	}
	rep.Availability = verdict.Availability
	measured := rep.Durations.GoodUs + rep.Durations.BadUs
	budget := sla.ErrorBudget(*rep.Objective, rep.Durations.GoodUs, measured)
	rep.Budget = &domain.ServiceBudget{
		Objective:            budget.Objective,
		ObjectiveUpdatedAt:   *rep.ObjectiveUpdatedAt,
		AllowedDowntimeRatio: budget.AllowedDowntimeRatio,
		ActualDowntimeRatio:  budget.ActualDowntimeRatio,
		RemainingRatio:       budget.RemainingRatio,
		BurnedPercent:        budget.BurnedPercent,
		Met:                  budget.Met,
	}
}

func gateAllWindowRepairingTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, reports []domain.ServiceWindowReport,
) error {
	from, to := reports[0].From, reports[0].To
	for _, report := range reports[1:] {
		if report.From.Before(from) {
			from = report.From
		}
		if report.To.After(to) {
			to = report.To
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT range_start, range_end FROM service_repair_ranges
		 WHERE service_id = $1 AND project_id = $2 AND state IN ('pending', 'running')
		   AND range_end > $3 AND range_start < $4
		 ORDER BY range_start`, serviceID, projectID, from, to)
	if err != nil {
		return fmt.Errorf("store: all-window report repairing: %w", err)
	}
	defer rows.Close()
	var intervals []domain.RepairingInterval
	for rows.Next() {
		var interval domain.RepairingInterval
		if err := rows.Scan(&interval.From, &interval.To); err != nil {
			return fmt.Errorf("store: scan all-window report repairing: %w", err)
		}
		intervals = append(intervals, interval)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: all-window report repairing: %w", err)
	}
	for index := range reports {
		for _, interval := range intervals {
			if !interval.To.After(reports[index].From) || !interval.From.Before(reports[index].To) {
				continue
			}
			clipped := interval
			if clipped.From.Before(reports[index].From) {
				clipped.From = reports[index].From
			}
			if clipped.To.After(reports[index].To) {
				clipped.To = reports[index].To
			}
			reports[index].Repairing = append(reports[index].Repairing, clipped)
		}
	}
	return nil
}
