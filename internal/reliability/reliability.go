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
	"fmt"
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

// AggMode is the within-region aggregation mode (§9.3).
type AggMode string

const (
	AggAll    AggMode = "all"
	AggAny    AggMode = "any"
	AggQuorum AggMode = "quorum"
)

// RegionMode is the across-region combination mode (§9.4). `any_region` and `all_regions`
// are sugar over per_region thresholds and are normalized away by Defaults.
type RegionMode string

const (
	RegionPer RegionMode = "per_region"
	RegionAny RegionMode = "any_region"
	RegionAll RegionMode = "all_regions"
)

// MissingData decides what an UNKNOWN member contributes (§8.1).
type MissingData string

const (
	// MissingUnknown keeps an undecided member undecided. The default.
	MissingUnknown MissingData = "unknown"
	// MissingBad counts an undecided member as failing.
	MissingBad MissingData = "bad"
	// MissingIgnore drops an undecided member from the aggregation — but ONLY while other
	// known members keep the interval decidable, and never by moving its time into the
	// excluded duration (§8.1).
	MissingIgnore MissingData = "ignore"
)

// Aggregation is the within-region policy. Thresholds are declared against the DECLARED
// member cardinality and clamped to the eligible one at evaluation time (§9.3).
type Aggregation struct {
	Mode AggMode
	// DegradedMin is the availability threshold: at least this many good members means
	// the service was serving.
	DegradedMin int
	// HealthyMin only splits GOOD into HEALTHY and DEGRADED. Since DegradedMin <=
	// HealthyMin, availability has exactly ONE good branch.
	HealthyMin int
}

// RegionPolicy combines per-region results (§9.4).
type RegionPolicy struct {
	Mode               RegionMode
	DegradedMinRegions int
	HealthyMinRegions  int
}

// Policies is the part of a definition revision the evaluator reads.
type Policies struct {
	Aggregation Aggregation
	Region      RegionPolicy
	MissingData MissingData
	// MaintenanceExcludes mirrors today's rule that maintenance heartbeats leave both the
	// numerator and the denominator. It is the only maintenance policy in phase 1.
	MaintenanceExcludes bool
}

// Freshness resolves a member's staleness deadline from its type and cadence (§7.1). It
// is applied once, when the evaluation epoch snapshots the member, so a recompute of an
// old range uses the deadline in force then rather than today's.
type Freshness struct {
	// ActiveMultiplier and ActiveFloor bound an active probe's tolerance: a result is
	// held until max(ActiveMultiplier*interval, ActiveFloor) has passed.
	ActiveMultiplier int
	ActiveFloor      time.Duration
}

// DefaultFreshness matches the mock and §9.5.
func DefaultFreshness() Freshness {
	return Freshness{ActiveMultiplier: 3, ActiveFloor: 90 * time.Second}
}

// ResolveStaleAfter returns how long a member's last observation stays effective.
//
// For `push` this deliberately mirrors the product's own dead-man cutoff — the stale-push
// sweep uses `interval_seconds + grace_seconds` — so a service and the monitor it is built
// from cannot disagree about when a missing ping became a failure.
func ResolveStaleAfter(f Freshness, typ domain.MonitorType, interval, grace time.Duration) time.Duration {
	if typ == domain.MonitorPush {
		return interval + grace
	}
	d := time.Duration(f.ActiveMultiplier) * interval
	if d < f.ActiveFloor {
		d = f.ActiveFloor
	}
	return d
}

// Member is one declared SLI member, exactly as the evaluation epoch snapshotted it.
// Nothing here is read from the live monitor row: that is what makes a recompute of an
// old range reproduce the state in force then rather than today's (§6.2).
type Member struct {
	MonitorID string
	Type      domain.MonitorType
	Region    string
	Enabled   bool
	// StaleAfter is the resolved freshness deadline (see ResolveStaleAfter).
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

// Defaults fills the policy fields a declaration may omit and normalizes the region-mode
// sugar, so the evaluator only ever sees per_region thresholds.
//
// declaredPerRegion is the declared member count per region, which the region thresholds
// default against; expectedRegions is the size of the expected region set (§9.4).
func Defaults(p Policies, declaredPerRegion map[string]int, expectedRegions int) Policies {
	if p.Aggregation.Mode == "" {
		// `all` is the conservative reading of "these count": every declared reliability
		// input must be good.
		p.Aggregation.Mode = AggAll
	}
	if p.MissingData == "" {
		p.MissingData = MissingUnknown
	}
	switch p.Region.Mode {
	case "", RegionPer:
		p.Region.Mode = RegionPer
		if p.Region.DegradedMinRegions == 0 {
			p.Region.DegradedMinRegions = 1
		}
		if p.Region.HealthyMinRegions == 0 {
			// Every expected region must be healthy — so one dark vantage point makes the
			// service degraded, not down.
			p.Region.HealthyMinRegions = expectedRegions
		}
	case RegionAny:
		p.Region = RegionPolicy{Mode: RegionPer, DegradedMinRegions: 1, HealthyMinRegions: 1}
	case RegionAll:
		p.Region = RegionPolicy{Mode: RegionPer, DegradedMinRegions: expectedRegions, HealthyMinRegions: expectedRegions}
	}
	if p.Aggregation.Mode == AggQuorum {
		if p.Aggregation.DegradedMin == 0 {
			p.Aggregation.DegradedMin = 1
		}
		if p.Aggregation.HealthyMin == 0 {
			maxDeclared := 0
			for _, n := range declaredPerRegion {
				if n > maxDeclared {
					maxDeclared = n
				}
			}
			p.Aggregation.HealthyMin = maxDeclared
		}
	}
	return p
}

// Validate enforces the write-time thresholds of §9.5 against the DECLARED cardinality.
// A policy momentarily unsatisfiable because members are excluded is NOT an error — that
// is the clamp of §9.3 — and conflating the two is what makes a planned maintenance window
// look like a definition error.
func Validate(p Policies, declaredPerRegion map[string]int, expectedRegions int) error {
	if len(declaredPerRegion) == 0 {
		return fmt.Errorf("reliability: a revision with reliability inputs declares at least one member")
	}
	for region, declared := range declaredPerRegion {
		if declared < 1 {
			return fmt.Errorf("reliability: region %q declares no members", region)
		}
	}
	switch p.Aggregation.Mode {
	case AggAll, AggAny:
	case AggQuorum:
		a := p.Aggregation
		if a.DegradedMin < 1 {
			return fmt.Errorf("reliability: degraded_min must be at least 1, got %d", a.DegradedMin)
		}
		if a.HealthyMin < a.DegradedMin {
			return fmt.Errorf("reliability: healthy_min %d is below degraded_min %d", a.HealthyMin, a.DegradedMin)
		}
		for region, declared := range declaredPerRegion {
			if a.HealthyMin > declared {
				return fmt.Errorf("reliability: healthy_min %d exceeds the %d members declared in region %q", a.HealthyMin, declared, region)
			}
		}
	default:
		return fmt.Errorf("reliability: unknown aggregation mode %q", p.Aggregation.Mode)
	}
	r := p.Region
	if r.Mode != RegionPer {
		return fmt.Errorf("reliability: region mode %q must be normalized by Defaults before validation", r.Mode)
	}
	if r.DegradedMinRegions < 1 {
		return fmt.Errorf("reliability: degraded_min_regions must be at least 1, got %d", r.DegradedMinRegions)
	}
	if r.HealthyMinRegions < r.DegradedMinRegions {
		return fmt.Errorf("reliability: healthy_min_regions %d is below degraded_min_regions %d", r.HealthyMinRegions, r.DegradedMinRegions)
	}
	if r.HealthyMinRegions > expectedRegions {
		return fmt.Errorf("reliability: healthy_min_regions %d exceeds the %d expected regions", r.HealthyMinRegions, expectedRegions)
	}
	switch p.MissingData {
	case MissingUnknown, MissingBad, MissingIgnore:
	default:
		return fmt.Errorf("reliability: unknown missing_data policy %q", p.MissingData)
	}
	return nil
}
