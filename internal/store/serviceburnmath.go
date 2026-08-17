package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// FR-021 phase 5 — §16.4's ONE owner of the burn math.
//
// Three callers ask the same question with different windows: the reporting card asks for the fixed
// 1h/6h pair (D-0163), a burn RULE asks for its own long/short pair, and one alert slice asks for
// two windows per rule per service. They are the same computation over the same sealed facts, so
// there is one implementation of it — a card that says "quotable" and a page that says "held" about
// the same service at the same instant would be two opinions about one number.
//
// The aggregates for the WHOLE slice are computed in ONE statement. One query per window is fine
// for a two-window card and is not fine for the evaluator, where the round-trip count would be
// (services × rules × 2) and the leader's cadence would depend on how many rules an operator
// happened to configure.

// burnWindowRequest is ONE window computation. Objective, era and sealed travel WITH the request
// rather than as batch-wide parameters: a slice spans many services, and each window is anchored at
// its OWN service's watermark and judged against its own materialization era.
type burnWindowRequest struct {
	serviceID string
	projectID string
	// label names the window in the answer — the card's "1h"/"6h", the rule path's window role.
	label     string
	duration  time.Duration
	objective float64
	// era is the service's materialization era start: a window reaching before it covers time that
	// was never measured, which no quantity of stored buckets turns into a rate.
	era time.Time
	// sealed is the service's sealed_through watermark. Every window is
	// [sealed − duration, sealed): a burn window never ends at now (§11.3).
	sealed time.Time
}

// burnWindowAggregate is what the batched query returns for one request.
type burnWindowAggregate struct {
	sealedBuckets int64
	revisions     int64
	d             domain.ReliabilityDurations
}

// computeBurnWindows answers every request in reqs, in order, from ONE round trip.
//
// asOf is the snapshot instant shared by the whole batch — the same instant the caller's
// transaction read its watermarks at, since §11.3's staleness test compares the two.
func computeBurnWindows(
	ctx context.Context, tx pgx.Tx, reqs []burnWindowRequest, asOf time.Time,
) ([]domain.ServiceBurnWindow, error) {
	out := make([]domain.ServiceBurnWindow, len(reqs))
	if len(reqs) == 0 {
		return out, nil
	}
	idx := make([]int64, len(reqs))
	services := make([]string, len(reqs))
	projects := make([]string, len(reqs))
	fromTS := make([]time.Time, len(reqs))
	toTS := make([]time.Time, len(reqs))
	for i, r := range reqs {
		idx[i], services[i], projects[i] = int64(i), r.serviceID, r.projectID
		fromTS[i], toTS[i] = r.sealed.Add(-r.duration), r.sealed
	}

	// The lateral is an UNGROUPED aggregate, so it yields exactly one row per want row — zeros for a
	// window with no sealed buckets at all, which is the honest input to the verdict below (a window
	// nobody stored is a storage gap, not an absent answer).
	aggs := make([]burnWindowAggregate, len(reqs))
	rows, err := tx.Query(ctx, `
		WITH want AS (
		    SELECT * FROM unnest($1::bigint[], $2::uuid[], $3::uuid[],
		                         $4::timestamptz[], $5::timestamptz[])
		        AS t(idx, service_id, project_id, from_ts, to_ts)
		)
		SELECT w.idx, agg.sealed_buckets, agg.revisions, agg.good_us, agg.bad_us, agg.unknown_us
		  FROM want w
		  LEFT JOIN LATERAL (
		      SELECT count(*) AS sealed_buckets,
		             count(DISTINCT e.revision_id) AS revisions,
		             COALESCE(sum(b.good_us), 0)::bigint AS good_us,
		             COALESCE(sum(b.bad_us), 0)::bigint AS bad_us,
		             COALESCE(sum(b.unknown_us), 0)::bigint AS unknown_us
		        FROM service_reliability_buckets b
		        JOIN service_evaluation_epochs e ON e.id = b.epoch_id
		       WHERE b.service_id = w.service_id AND b.project_id = w.project_id
		         AND b.bucket_start >= w.from_ts AND b.bucket_start < w.to_ts
		         AND b.state = 'sealed'
		  ) agg ON true`, idx, services, projects, fromTS, toTS)
	if err != nil {
		return nil, fmt.Errorf("store: burn windows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var i int64
		var a burnWindowAggregate
		if err := rows.Scan(&i, &a.sealedBuckets, &a.revisions,
			&a.d.GoodUs, &a.d.BadUs, &a.d.UnknownUs); err != nil {
			return nil, fmt.Errorf("store: scan burn window: %w", err)
		}
		if i < 0 || i >= int64(len(reqs)) {
			return nil, fmt.Errorf("store: burn window index %d out of range", i)
		}
		aggs[i] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: burn windows: %w", err)
	}
	for i, r := range reqs {
		out[i] = decideBurnWindow(r, aggs[i], asOf)
	}
	return out, nil
}

// decideBurnWindow applies §11.2/§11.3 to ONE window's aggregate, and it is the only place that
// decides whether a burn number may be quoted.
//
// The honesty rules are the main window's: storage continuity is measured per window and a gap
// withholds the rate (a burn window with one surviving bucket returning "0×" would be exactly the
// fabricated confidence §11.1 forbids); a window spanning definition revisions offers no rate
// (invariant 43); low decidable coverage keeps its rate WITH the fraction and reason (§11.2).
// §11.3's staleness rule: when the equivalent real-time window [asOf − w, asOf) contains no sealed
// time at all — sealed_through ≤ asOf − w — the answer is insufficient_sealed_coverage, not a stale
// rate.
func decideBurnWindow(
	r burnWindowRequest, a burnWindowAggregate, asOf time.Time,
) domain.ServiceBurnWindow {
	bw := domain.ServiceBurnWindow{
		Window:          r.label,
		ExpectedBuckets: int64(r.duration / domain.CanonicalBucket),
		SealedBuckets:   a.sealedBuckets,
	}
	from := r.sealed.Add(-r.duration)

	// The anchored window's OWN verdict is computed FIRST, unconditionally ([170] P1-2): the fields
	// describe [sealed_through − w, sealed_through), and a stale or era-short status must not
	// relabel a fully stored, fully measured window as 0/N with zero coverage — staleness qualifies
	// the ANSWER, it does not rewrite the window's storage facts.
	bw.StorageContinuity = bw.SealedBuckets == bw.ExpectedBuckets
	bw.Coverage = decidableCoverage(a.d)
	measured := a.d.GoodUs + a.d.BadUs

	switch {
	case !r.sealed.After(asOf.Add(-r.duration)):
		bw.Status, bw.Reason = domain.ServiceReportInsufficientSealed, domain.ServiceReportReasonStaleWatermark
	case from.Before(r.era):
		bw.Status, bw.Reason = domain.ServiceReportInsufficientHistory, domain.ServiceReportReasonEraShort
	case !bw.StorageContinuity:
		// A gap withholds the rate: the surviving rows cannot vouch for the window
		// and cannot even prove it spans one revision.
		bw.Status, bw.Reason = domain.ServiceReportPartial, domain.ServiceReportReasonStorageGap
	case a.revisions > 1:
		// A burn window is a window: no aggregate across a definition boundary
		// (invariant 43).
		bw.Status, bw.Reason = domain.ServiceReportUnavailable, domain.ServiceReportReasonSpansRevisions
	case measured == 0:
		bw.Status, bw.Reason = domain.ServiceReportUnavailable, domain.ServiceReportReasonZeroDecidable
	default:
		rate := sla.BurnRate(r.objective, a.d.GoodUs, measured)
		bw.Rate = &rate
		if bw.Coverage < minDecidableCoverage {
			bw.Status, bw.Reason = domain.ServiceReportPartial, domain.ServiceReportReasonLowCoverage
		} else {
			bw.Status = domain.ServiceReportOK
		}
	}
	return bw
}
