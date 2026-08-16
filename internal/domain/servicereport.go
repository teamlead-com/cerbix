package domain

import "time"

// FR-021 phase 2 (§11–§12): the reliability report payload. Everything here is computed from
// SEALED facts over [sealed_through − window, sealed_through) — never from now — except
// ServiceHealthNow, which is the spec's explicitly separate, explicitly unstable live signal
// (§11.3). Numbers that cannot be honestly stated are ABSENT with a status and a reason,
// never rendered as 100% or 0× (§11.1, invariant 40).

// ServiceReportStatus qualifies a report, a segment or a burn window.
type ServiceReportStatus string

const (
	// ServiceReportOK: continuity holds and decidable coverage is at or above the fixed
	// min_decidable_coverage (§10.10).
	ServiceReportOK ServiceReportStatus = "ok"
	// ServiceReportPartial: the number exists but under-measured or under-stored time makes
	// it partial; Reason and the coverage fraction say why (§11.2).
	ServiceReportPartial ServiceReportStatus = "partial"
	// ServiceReportUnavailable: a zero denominator — no decidable time at all, or no
	// measured time for availability. Never 100%, never 0× (§11.1).
	ServiceReportUnavailable ServiceReportStatus = "unavailable"
	// ServiceReportInsufficientHistory: the window reaches before the service's
	// materialization era; the covered fraction is reported.
	ServiceReportInsufficientHistory ServiceReportStatus = "insufficient_history"
	// ServiceReportInsufficientSealed: nothing sealed covers the asked window (§11.3's
	// "lying entirely to the right of sealed_through", and the nothing-sealed-yet case).
	ServiceReportInsufficientSealed ServiceReportStatus = "insufficient_sealed_coverage"
)

// ReliabilityDurations are exact integer-µs sums of BOTH §9.1 axes over some bucket range.
type ReliabilityDurations struct {
	GoodUs, BadUs, UnknownUs, ExcludedUs           int64
	HealthyUs, DegradedUs, DownUs, HealthUnknownUs int64
}

// ReliabilitySegment is one (epoch × reconstruction-part) slice of the window. A window
// spanning definition revisions is ONLY segments — no aggregate crosses that boundary
// (§12.1, invariant 43). Epoch boundaries within one revision are segmented too; visual
// grouping is presentation and happens client-side over identical semantics.
type ReliabilitySegment struct {
	RevisionID string               `json:"revision_id"`
	Revision   int64                `json:"revision"`
	EpochID    string               `json:"epoch_id"`
	EpochSeq   int64                `json:"epoch_seq"`
	From       time.Time            `json:"from"`
	To         time.Time            `json:"to"`
	Buckets    int64                `json:"buckets"`
	Durations  ReliabilityDurations `json:"durations"`
	// Availability is nil when the segment measured nothing (good+bad = 0).
	Availability *float64 `json:"availability,omitempty"`
	// Coverage is the segment's decidable fraction (good+bad)/(good+bad+unknown).
	Coverage float64 `json:"coverage"`
	// DeclaredReconstruction marks the retroactive part of a first-adoption backfill:
	// revision 1 with effective_at < created_at, buckets before CeilToBucket(created_at)
	// (§6.6, invariant 44). It is a label of provenance, not a quality score.
	DeclaredReconstruction bool `json:"declared_reconstruction"`
}

// RepairingInterval is a pending/running repair range intersected with the window — rendered
// as repair-in-progress, never as data (§12.1, invariant 38).
type RepairingInterval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// ServiceBudget is the error budget over the window's MEASURED time, stated together with
// the objective that produced it (§11.3: the objective is a current-view parameter).
type ServiceBudget struct {
	Objective            float64   `json:"objective"`
	ObjectiveUpdatedAt   time.Time `json:"objective_updated_at"`
	AllowedDowntimeRatio float64   `json:"allowed_downtime_ratio"`
	ActualDowntimeRatio  float64   `json:"actual_downtime_ratio"`
	RemainingRatio       float64   `json:"remaining_ratio"`
	BurnedPercent        float64   `json:"burned_percent"`
	Met                  bool      `json:"met"`
}

// ServiceBurnWindow is one reported burn window over sealed facts,
// [sealed_through − window, sealed_through), carrying its OWN §11.2 verdicts: storage
// continuity (a gap withholds the rate) and decidable coverage (a low fraction keeps the
// rate, partial, with the fraction and reason). When the equivalent real-time window
// [as_of − window, as_of) contains no sealed time at all, the status is
// insufficient_sealed_coverage and no rate is quoted (§11.3).
type ServiceBurnWindow struct {
	Window            string              `json:"window"`
	Status            ServiceReportStatus `json:"status"`
	Reason            string              `json:"reason,omitempty"`
	ExpectedBuckets   int64               `json:"expected_buckets"`
	SealedBuckets     int64               `json:"sealed_buckets"`
	StorageContinuity bool                `json:"storage_continuity"`
	Coverage          float64             `json:"coverage"`
	Rate              *float64            `json:"rate,omitempty"`
}

// ServiceWindowReport is the §11 reliability answer for one service and one window.
type ServiceWindowReport struct {
	ServiceID     string     `json:"service_id"`
	Window        string     `json:"window"`
	AsOf          time.Time  `json:"as_of"`
	SealedThrough *time.Time `json:"sealed_through"`
	// From/To are [sealed_through − window, sealed_through); zero when nothing is sealed.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Status ServiceReportStatus `json:"status"`
	Reason string              `json:"reason,omitempty"`

	// The two independent §11.2 axes: did we STORE the window, did we MEASURE it.
	StorageContinuity bool    `json:"storage_continuity"`
	ExpectedBuckets   int64   `json:"expected_buckets"`
	SealedBuckets     int64   `json:"sealed_buckets"`
	Coverage          float64 `json:"coverage"`

	Durations ReliabilityDurations `json:"durations"`

	// Availability is the window aggregate. It is nil when the window spans definition
	// revisions (AggregateWithheld says so), when nothing was measured, or when nothing is
	// sealed. Segments always carry their own numbers.
	Availability      *float64 `json:"availability,omitempty"`
	AggregateWithheld string   `json:"aggregate_withheld,omitempty"`

	// Objective/Budget/Burn are absent without a service-scoped sla_target for this window
	// — never defaulted, never borrowed from another scope.
	Objective          *float64            `json:"objective,omitempty"`
	ObjectiveUpdatedAt *time.Time          `json:"objective_updated_at,omitempty"`
	Budget             *ServiceBudget      `json:"budget,omitempty"`
	Burn               []ServiceBurnWindow `json:"burn,omitempty"`

	Segments  []ReliabilitySegment `json:"segments"`
	Repairing []RepairingInterval  `json:"repairing,omitempty"`

	// RetractedAt/RetractedTo annotate an audited watermark retraction (§10.5): a window
	// that got shorter is distinguishable from a bug.
	RetractedAt *time.Time `json:"retracted_at,omitempty"`
	RetractedTo *time.Time `json:"retracted_to,omitempty"`
}

// ServiceHealthNow is the categorical LIVE signal (§11.3): derived from provisional data,
// explicitly unstable, and deliberately not a percentage. The SLI layer answers for the
// declared sli[] members only; Diagnostics answers for monitors[] — the two-layer card of
// §12.2, where a diagnostic monitor can never degrade the customer-facing layer.
type ServiceHealthNow struct {
	Unstable bool      `json:"unstable"` // always true by construction
	AsOf     time.Time `json:"as_of"`
	// SLI is healthy|degraded|down|unknown: the declared SLI semantics evaluated ON READ at
	// the as_of instant — carry-in observations, maintenance spans and freshness deadlines
	// through the same projection the materializer uses; no stored fact is consulted. An
	// exclusion in force at as_of reads as unknown.
	SLI string `json:"sli"`
	// Diagnostics is ok|failing|unknown over the CURRENT monitors[] statuses.
	Diagnostics     string   `json:"diagnostics"`
	FailingMonitors []string `json:"failing_monitors,omitempty"`
}

// ServiceReportReason* are the fixed reason strings — tests and the UI switch on them, so
// they are constants rather than prose.
const (
	ServiceReportReasonNoSLI           = "no_sli"
	ServiceReportReasonNothingSealed   = "nothing_sealed"
	ServiceReportReasonEraShort        = "window_precedes_materialization_era"
	ServiceReportReasonStorageGap      = "storage_gap"
	ServiceReportReasonZeroDecidable   = "zero_decidable_time"
	ServiceReportReasonLowCoverage     = "decidable_coverage_below_min"
	ServiceReportReasonSpansRevisions  = "spans_definition_revisions"
	ServiceReportReasonNoObjective     = "no_objective"
	ServiceReportReasonStaleWatermark  = "sealed_through_behind_window"
	ServiceReportReasonNothingMeasured = "nothing_measured"
)

// ReliabilitySeriesPoint is one on-read rollup cell: exact integer sums over canonical
// buckets within one (step × epoch × state) — never merged across an epoch boundary (§10.2),
// never mixing provisional time into a sealed number.
type ReliabilitySeriesPoint struct {
	Start       time.Time            `json:"start"`
	EpochID     string               `json:"epoch_id"`
	RevisionID  string               `json:"revision_id"`
	Provisional bool                 `json:"provisional"`
	Buckets     int64                `json:"buckets"`
	Durations   ReliabilityDurations `json:"durations"`
}
