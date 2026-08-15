// Package reliability turns monitor observations into per-bucket service reliability
// facts. It is pure (no I/O and no clock): callers supply the observations, the
// evaluation-epoch snapshot, the maintenance spans and the bucket range, and this
// package returns the durations and the provenance for that bucket.
//
// It implements func-service-reliability.md §7–§9. Two properties are load-bearing and
// are asserted by the tests rather than assumed:
//
//   - The reducer is PIECEWISE. A bucket is split at every instant where some member's
//     effective state can change — an observation, a staleness deadline, a maintenance
//     edge — and the aggregation runs once per sub-interval. A member that is GOOD for
//     20s and UNKNOWN for 40s of one minute contributes both, exactly.
//   - Both result axes CONSERVE. The four availability durations sum to the bucket
//     length, and so do the four health durations plus the shared excluded duration.
//     Conservation is checked on every Reduce, because a reducer that silently loses
//     time produces a budget nobody can reconcile.
//
// Only the availability axis feeds SLO, error budget, burn rate and coverage. The health
// axis is history for presentation and is never substituted for availability.
package reliability

import (
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// State is a member's normalized state at one instant (§7.1). Normalization belongs to
// the monitor TYPE: an active probe and a dead-man's switch disagree about what a missing
// observation means, and the service layer never re-guesses it.
type State uint8

const (
	// StateUnknown is "we should have measured and did not". It costs coverage.
	StateUnknown State = iota
	// StateGood is an observed success still inside its freshness deadline.
	StateGood
	// StateBad is an observed failure, or a stale push member — for push the absence of
	// a ping IS the failure.
	StateBad
	// StateExcluded is declared out of scope: disabled in the epoch snapshot, or inside
	// a maintenance window. It costs nothing, which is why §8.1 forbids anything else
	// from being laundered into it.
	StateExcluded
)

func (s State) String() string {
	switch s {
	case StateGood:
		return "good"
	case StateBad:
		return "bad"
	case StateExcluded:
		return "excluded"
	default:
		return "unknown"
	}
}

// Availability is the only input to SLO and error budget (§9.1).
type Availability uint8

const (
	AvailUnknown Availability = iota
	AvailGood
	AvailBad
	// AvailExcluded is declared-out-of-scope time. It enters neither the availability
	// numerator/denominator nor the coverage denominator (§8.1).
	AvailExcluded
)

func (a Availability) String() string {
	switch a {
	case AvailGood:
		return "good"
	case AvailBad:
		return "bad"
	case AvailExcluded:
		return "excluded"
	default:
		return "unknown"
	}
}

// Health is presentation, with its own duration history so a bucket that was HEALTHY
// throughout is distinguishable from one that was HEALTHY for half of it (§9.1).
type Health uint8

const (
	HealthUnknown Health = iota
	HealthHealthy
	HealthDegraded
	HealthDown
	// HealthExcluded always co-occurs with AvailExcluded: declared exclusion is the same
	// time under either reading, which is why the two axes share one excluded duration.
	HealthExcluded
)

func (h Health) String() string {
	switch h {
	case HealthHealthy:
		return "healthy"
	case HealthDegraded:
		return "degraded"
	case HealthDown:
		return "down"
	case HealthExcluded:
		return "excluded"
	default:
		return "unknown"
	}
}

// Policies is the evaluator's view of a definition revision. The type itself lives in
// `domain`, because the same value is stored on the revision, validated at write time by
// ONE validator shared with the file provider, and read here — three consumers of one
// declaration, not three declarations.
type Policies = domain.ServicePolicies

// Member is one declared SLI member, exactly as the evaluation epoch snapshotted it.
// Nothing here is read from the live monitor row: that is what makes a recompute of an
// old range reproduce the state in force then rather than today's (§6.2).
type Member struct {
	MonitorID string
	Type      domain.MonitorType
	Region    string
	Enabled   bool
	// StaleAfter is the resolved freshness deadline (domain.ResolveStaleAfter).
	StaleAfter time.Duration
	// ArmedAt is the instant a `push` member's dead-man starts counting when it has no
	// observation yet. The product measures from COALESCE(GREATEST(push_armed_at,
	// last_result_ts), created_at), so a never-reported push monitor does eventually go
	// down; a service that treated it as permanently UNKNOWN would disagree with its own
	// monitor. Ignored for active probes, where absence is uncertainty and not failure.
	ArmedAt time.Time
}

// Observation is one heartbeat, reduced to what the evaluator reads. Confirmation and
// failure thresholds are deliberately absent: they change cadence and alerting, not
// measured state, and no history of confirmed transitions exists to read instead (§6.7).
type Observation struct {
	MonitorID string
	Ts        time.Time
	Up        bool
}

// MaintenanceSpan is one effective maintenance interval, half-open. A span with an empty
// MonitorID is project-wide and covers every member.
//
// The caller resolves the span from the retained row over
// [starts_at, min(ends_at, cancel_effective_at)) REGARDLESS of archived_at (§10.9): an
// archived window keeps its effect on already-sealed time, and only an explicit annul
// removes it from this input.
type MaintenanceSpan struct {
	ID        string
	MonitorID string
	From, To  time.Time
}

func (m MaintenanceSpan) covers(monitorID string, t time.Time) bool {
	if m.MonitorID != "" && m.MonitorID != monitorID {
		return false
	}
	return !t.Before(m.From) && t.Before(m.To)
}

// Durations are the stored truth of one bucket, in nanoseconds of wall time.
//
// Storage keeps these as integer MICROseconds (§7.2); time.Duration is nanoseconds and is
// exact for every value this package produces, because every boundary comes from a
// timestamptz that is itself microsecond-precision.
type Durations struct {
	// Availability axis. Good+Bad+Unknown+Excluded == bucket length.
	Good     time.Duration
	Bad      time.Duration
	Unknown  time.Duration
	Excluded time.Duration

	// Health axis. Healthy+Degraded+Down+HealthUnknown+Excluded == bucket length.
	Healthy       time.Duration
	Degraded      time.Duration
	Down          time.Duration
	HealthUnknown time.Duration
}

// Total returns the availability axis sum, which must equal the bucket length.
func (d Durations) Total() time.Duration { return d.Good + d.Bad + d.Unknown + d.Excluded }

// HealthTotal returns the health axis sum, which must equal the bucket length too.
func (d Durations) HealthTotal() time.Duration {
	return d.Healthy + d.Degraded + d.Down + d.HealthUnknown + d.Excluded
}

func (d *Durations) add(o Outcome, span time.Duration) {
	switch o.Availability {
	case AvailGood:
		d.Good += span
	case AvailBad:
		d.Bad += span
	case AvailExcluded:
		d.Excluded += span
	default:
		d.Unknown += span
	}
	switch o.Health {
	case HealthHealthy:
		d.Healthy += span
	case HealthDegraded:
		d.Degraded += span
	case HealthDown:
		d.Down += span
	case HealthExcluded:
		// shared with the availability axis; already counted above
	default:
		d.HealthUnknown += span
	}
}

// UnknownReason says why a member had no decidable state, so a window that is `partial`
// at 90 days is still explainable after the raw heartbeats are gone (§10.3).
type UnknownReason string

const (
	// ReasonNoObservation: the member has produced nothing at or before this instant.
	ReasonNoObservation UnknownReason = "no_observation"
	// ReasonStale: the member's last observation aged past its freshness deadline.
	ReasonStale UnknownReason = "stale"
)

// MemberCause is a bounded provenance reference to one member and why it mattered.
type MemberCause struct {
	MonitorID string
	Region    string
	Reason    UnknownReason // set only for unknown causes
}

// Weakened records that a threshold was clamped to the eligible cardinality, and by which
// cause. The distinction matters to the reader: a quorum weakened by a planned maintenance
// window is an operator's own decision, while one weakened because data went missing and
// the policy discarded it is a measurement problem wearing the same clamp (§9.3).
type Weakened struct {
	Region              string
	Declared            int
	Eligible            int
	ExcludedDisabled    int
	ExcludedMaintenance int
	Ignored             int
	DeclaredDegradedMin int
	DeclaredHealthyMin  int
	EffectiveDegraded   int
	EffectiveHealthy    int
}

// Provenance is the bounded record stored beside a fact. It is a fixed small structure per
// bucket, not an event log: anything the bound cannot carry is removed from the
// explainability promise rather than implied by it (§10.3).
type Provenance struct {
	Declared int
	// Bad and Unknown name the members that caused those durations, deduplicated over the
	// bucket and truncated at MaxCauses with Overflow counting what did not fit.
	Bad      []MemberCause
	Unknown  []MemberCause
	Excluded []MemberCause
	Overflow int
	// Weakened is non-empty when some sub-interval ran under a clamped threshold.
	Weakened []Weakened
}

// MaxCauses bounds each provenance cause set (§10.10 `max_provenance_causes`).
const MaxCauses = 8

// Bucket is the reducer's output for one canonical bucket.
type Bucket struct {
	Start, End time.Time
	Durations  Durations
	Provenance Provenance
}

// Outcome is the aggregation result for one sub-interval.
type Outcome struct {
	Availability Availability
	Health       Health
	// Weakened is set when a threshold was clamped for this sub-interval.
	Weakened []Weakened
}
