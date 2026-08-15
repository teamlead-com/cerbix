package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// A Service is the place where it is explicitly declared what reliability MEANS for one
// operational unit (func-service-reliability §1). It is not a monitor, not a status-page
// component, and not a catalog entry: fields live here only if they affect the reliability
// computation, the routing of operational response, or the interpretation of health.
type Service struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	// Slug is project-unique and immutable: the MaC reference key and the URL segment.
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Owner is a REFERENCE to existing routing primitives, never a free-text team label,
	// so that "who is responsible" is actionable rather than decorative.
	EscalationPolicyID string    `json:"escalation_policy_id,omitempty"`
	OncallScheduleID   string    `json:"oncall_schedule_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// MemberRole separates the two lists that must be declared INDEPENDENTLY.
type MemberRole string

const (
	// RoleContext is operational membership: what is shown on the service, what
	// diagnostics exist.
	RoleContext MemberRole = "context"
	// RoleSLI is a declared reliability input: what actually counts toward availability.
	RoleSLI MemberRole = "sli"
)

// ServiceDeclaration is what a human declares. Everything here is part of the definition
// revision, because changing any of it changes the meaning of the number.
type ServiceDeclaration struct {
	// Monitors is the operational context, by monitor id.
	Monitors []string `json:"monitors"`
	// SLI is the reliability inputs, by monitor id. It must be a subset of Monitors — an
	// SLI member outside the operational context would be a number nobody can see the
	// source of — but it is declared SEPARATELY, so that adding a diagnostic never
	// silently redefines availability.
	SLI      []string        `json:"sli"`
	Policies ServicePolicies `json:"policies"`
}

// AggMode is the within-region aggregation mode.
type AggMode string

const (
	AggAll    AggMode = "all"
	AggAny    AggMode = "any"
	AggQuorum AggMode = "quorum"
)

// RegionMode combines per-region results. `any_region` and `all_regions` are sugar over
// per_region thresholds and are normalized away by ApplyServicePolicyDefaults.
type RegionMode string

const (
	RegionPer RegionMode = "per_region"
	RegionAny RegionMode = "any_region"
	RegionAll RegionMode = "all_regions"
)

// MissingDataPolicy decides what an UNKNOWN member contributes.
type MissingDataPolicy string

const (
	// MissingUnknown keeps an undecided member undecided. The default.
	MissingUnknown MissingDataPolicy = "unknown"
	// MissingBad counts an undecided member as failing.
	MissingBad MissingDataPolicy = "bad"
	// MissingIgnore drops an undecided member from the aggregation — but only while other
	// KNOWN members keep the interval decidable, and never by moving its time into the
	// excluded duration. Without that restriction it is a one-line configuration change
	// that buys 100% coverage on a service which measured nothing.
	MissingIgnore MissingDataPolicy = "ignore"
)

// MaintenancePolicy decides what a maintenance window does to a member inside it.
//
// It is an enum rather than a bool because the DEFAULT is "exclude": a bool's zero value
// would silently mean "maintenance changes nothing", which contradicts both this document
// and the product's existing rule that maintenance heartbeats leave the numerator and the
// denominator alike. A type whose zero value is the wrong answer is the wrong type.
type MaintenancePolicy string

const (
	// MaintenanceExclude removes a member inside a window from the aggregation, and the
	// exclusion wins over whatever its type would otherwise normalize to.
	MaintenanceExclude MaintenancePolicy = "exclude"
)

// AggregationPolicy is the within-region policy. Thresholds are declared against the
// DECLARED member cardinality and clamped to the eligible one at evaluation time.
type AggregationPolicy struct {
	Mode AggMode `json:"mode"`
	// DegradedMin is the availability threshold: at least this many good members means the
	// service was serving.
	DegradedMin int `json:"degraded_min,omitempty"`
	// HealthyMin only splits GOOD into HEALTHY and DEGRADED. Since DegradedMin <=
	// HealthyMin, availability has exactly ONE good branch.
	HealthyMin int `json:"healthy_min,omitempty"`
}

// RegionAggregationPolicy combines per-region results.
type RegionAggregationPolicy struct {
	Mode               RegionMode `json:"mode"`
	DegradedMinRegions int        `json:"degraded_min_regions,omitempty"`
	HealthyMinRegions  int        `json:"healthy_min_regions,omitempty"`
}

// ValidServiceSlug reports whether s is a well-formed service slug, and SlugPattern is that
// rule spelled for an error message.
//
// A service slug and a monitor slug are the same rule — both get typed into a bundle by hand
// and both become a URL segment — so this delegates instead of restating it. Two spellings of
// one rule is exactly how two surfaces drift apart.
func ValidServiceSlug(s string) bool { return ValidMonitorSlug(s) }

// FreshnessPolicy resolves how long a member's last observation stays effective. It is
// applied once, when the evaluation epoch snapshots the member, so a recompute of an old
// range uses the deadline in force then rather than today's.
type FreshnessPolicy struct {
	ActiveMultiplier int           `json:"active_multiplier,omitempty"`
	ActiveFloor      time.Duration `json:"active_floor,omitempty"`
}

// The file surface spells this field `90s` and a bare time.Duration would spell the same
// value `90000000000` over JSON. One policy field with two spellings is the same defect as
// two validators: an operator reading the API and an operator reading the bundle would
// disagree about what they had declared. So JSON uses the duration string too.
//
// Reads accept BOTH: rows written before this codec existed hold the integer.
func (f FreshnessPolicy) MarshalJSON() ([]byte, error) {
	out := struct {
		ActiveMultiplier int    `json:"active_multiplier,omitempty"`
		ActiveFloor      string `json:"active_floor,omitempty"`
	}{ActiveMultiplier: f.ActiveMultiplier}
	if f.ActiveFloor != 0 {
		out.ActiveFloor = f.ActiveFloor.String()
	}
	return json.Marshal(out)
}

func (f *FreshnessPolicy) UnmarshalJSON(b []byte) error {
	var raw struct {
		ActiveMultiplier int             `json:"active_multiplier"`
		ActiveFloor      json.RawMessage `json:"active_floor"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	f.ActiveMultiplier = raw.ActiveMultiplier
	f.ActiveFloor = 0
	if len(raw.ActiveFloor) == 0 || string(raw.ActiveFloor) == "null" {
		return nil
	}
	if raw.ActiveFloor[0] == '"' {
		var s string
		if err := json.Unmarshal(raw.ActiveFloor, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("freshness.active_floor: %w", err)
		}
		f.ActiveFloor = d
		return nil
	}
	var ns int64
	if err := json.Unmarshal(raw.ActiveFloor, &ns); err != nil {
		return fmt.Errorf("freshness.active_floor must be a duration string: %w", err)
	}
	f.ActiveFloor = time.Duration(ns)
	return nil
}

// ServicePolicies is the part of a definition revision the evaluator reads. It is stored
// as JSON on the revision and validated by ONE validator, shared by the API and the file
// provider — a second validator is a second answer to the same question.
type ServicePolicies struct {
	Aggregation AggregationPolicy       `json:"aggregation"`
	Region      RegionAggregationPolicy `json:"region"`
	MissingData MissingDataPolicy       `json:"missing_data"`
	// Maintenance mirrors today's rule that maintenance heartbeats leave both the numerator
	// and the denominator. `exclude` is the only value in phase 1, and the default.
	Maintenance MaintenancePolicy `json:"maintenance"`
	Freshness   FreshnessPolicy   `json:"freshness"`
}

// DefaultFreshnessPolicy is what an omitted freshness block resolves to.
func DefaultFreshnessPolicy() FreshnessPolicy {
	return FreshnessPolicy{ActiveMultiplier: 3, ActiveFloor: 90 * time.Second}
}

// ResolveStaleAfter returns how long a member's last observation stays effective.
//
// For `push` this deliberately mirrors the product's own dead-man cutoff — the stale-push
// sweep uses interval_seconds + grace_seconds — so a service and the monitor it is built
// from cannot disagree about when a missing ping became a failure.
func ResolveStaleAfter(f FreshnessPolicy, typ MonitorType, interval, grace time.Duration) time.Duration {
	if typ == MonitorPush {
		return interval + grace
	}
	d := time.Duration(f.ActiveMultiplier) * interval
	if d < f.ActiveFloor {
		d = f.ActiveFloor
	}
	return d
}

// ApplyServicePolicyDefaults fills the fields a declaration may omit and normalizes the
// region-mode sugar, so the evaluator only ever sees per_region thresholds.
//
// declaredPerRegion is the declared SLI member count per region; expectedRegions is the
// size of the expected region set.
func ApplyServicePolicyDefaults(p ServicePolicies, declaredPerRegion map[string]int, expectedRegions int) ServicePolicies {
	if p.Aggregation.Mode == "" {
		// `all` is the conservative reading of "these count": every declared reliability
		// input must be good.
		p.Aggregation.Mode = AggAll
	}
	if p.MissingData == "" {
		p.MissingData = MissingUnknown
	}
	if p.Maintenance == "" {
		p.Maintenance = MaintenanceExclude
	}
	if p.Freshness.ActiveMultiplier == 0 && p.Freshness.ActiveFloor == 0 {
		p.Freshness = DefaultFreshnessPolicy()
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
		p.Region = RegionAggregationPolicy{Mode: RegionPer, DegradedMinRegions: 1, HealthyMinRegions: 1}
	case RegionAll:
		p.Region = RegionAggregationPolicy{Mode: RegionPer, DegradedMinRegions: expectedRegions, HealthyMinRegions: expectedRegions}
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

// ValidateServicePolicies enforces the write-time thresholds against the DECLARED
// cardinality.
//
// A policy momentarily unsatisfiable because members are EXCLUDED is not an error — that
// is the evaluation-time clamp — and conflating the two is what makes a planned
// maintenance window look like a definition error.
func ValidateServicePolicies(p ServicePolicies, declaredPerRegion map[string]int, expectedRegions int) error {
	if len(declaredPerRegion) == 0 {
		return fmt.Errorf("service policies: a revision with reliability inputs declares at least one member")
	}
	for region, declared := range declaredPerRegion {
		if declared < 1 {
			return fmt.Errorf("service policies: region %q declares no members", region)
		}
	}
	switch p.Aggregation.Mode {
	case AggAll, AggAny:
	case AggQuorum:
		a := p.Aggregation
		if a.DegradedMin < 1 {
			return fmt.Errorf("service policies: degraded_min must be at least 1, got %d", a.DegradedMin)
		}
		if a.HealthyMin < a.DegradedMin {
			return fmt.Errorf("service policies: healthy_min %d is below degraded_min %d", a.HealthyMin, a.DegradedMin)
		}
		for region, declared := range declaredPerRegion {
			if a.HealthyMin > declared {
				return fmt.Errorf("service policies: healthy_min %d exceeds the %d members declared in region %q", a.HealthyMin, declared, region)
			}
		}
	default:
		return fmt.Errorf("service policies: unknown aggregation mode %q", p.Aggregation.Mode)
	}
	r := p.Region
	if r.Mode != RegionPer {
		return fmt.Errorf("service policies: region mode %q must be normalized before validation", r.Mode)
	}
	if r.DegradedMinRegions < 1 {
		return fmt.Errorf("service policies: degraded_min_regions must be at least 1, got %d", r.DegradedMinRegions)
	}
	if r.HealthyMinRegions < r.DegradedMinRegions {
		return fmt.Errorf("service policies: healthy_min_regions %d is below degraded_min_regions %d", r.HealthyMinRegions, r.DegradedMinRegions)
	}
	if r.HealthyMinRegions > expectedRegions {
		return fmt.Errorf("service policies: healthy_min_regions %d exceeds the %d expected regions", r.HealthyMinRegions, expectedRegions)
	}
	switch p.MissingData {
	case MissingUnknown, MissingBad, MissingIgnore:
	default:
		return fmt.Errorf("service policies: unknown missing_data policy %q", p.MissingData)
	}
	switch p.Maintenance {
	case MaintenanceExclude:
	default:
		return fmt.Errorf("service policies: unknown maintenance policy %q", p.Maintenance)
	}
	return nil
}

// RevisionState distinguishes the row that governs from the row that lost a same-boundary
// race. The loser is retained for audit, is never referenced by a fact, and contributes no
// validity interval.
type RevisionState string

const (
	RevisionEffective RevisionState = "effective"
	// RevisionSuperseded is a row that never took effect: a later write on the SAME
	// boundary displaced it before it ever governed a bucket.
	RevisionSuperseded RevisionState = "superseded_before_effect"
)

// DefinitionRevision is axis one: what a human declared availability to mean.
type DefinitionRevision struct {
	ID        string `json:"id"`
	ServiceID string `json:"service_id"`
	ProjectID string `json:"project_id"`
	Revision  int64  `json:"revision"`
	// CreatedAt is when the write happened; EffectiveAt is when the row starts to GOVERN
	// buckets. They are separate because conflating them leaves the boundary undefined.
	CreatedAt   time.Time     `json:"created_at"`
	EffectiveAt time.Time     `json:"effective_at"`
	State       RevisionState `json:"state"`

	Monitors  []string        `json:"monitors"`
	SLI       []string        `json:"sli"`
	Policies  ServicePolicies `json:"policies"`
	CreatedBy string          `json:"created_by,omitempty"`
}

// EpochMember is one declared SLI member as the epoch snapshotted it: the
// evaluation-semantics projection plus the resolved staleness deadline. Nothing here is
// re-read from the live monitor row at evaluation time — that is what makes a recompute of
// an old range reproduce the state in force then.
type EpochMember struct {
	MonitorID  string              `json:"monitor_id"`
	Semantics  EvaluationSemantics `json:"semantics"`
	StaleAfter time.Duration       `json:"stale_after"`
	// ArmedAt is where a push member's dead-man starts counting when it has no observation
	// yet, mirroring the product's own COALESCE(GREATEST(push_armed_at, last_result_ts),
	// created_at).
	ArmedAt time.Time `json:"armed_at,omitempty"`
}

// EvaluationEpoch is axis two: what the system was measuring.
type EvaluationEpoch struct {
	ID           string        `json:"id"`
	ServiceID    string        `json:"service_id"`
	ProjectID    string        `json:"project_id"`
	Seq          int64         `json:"seq"`
	RevisionID   string        `json:"revision_id"`
	CreatedAt    time.Time     `json:"created_at"`
	EffectiveAt  time.Time     `json:"effective_at"`
	State        RevisionState `json:"state"`
	Members      []EpochMember `json:"members"`
	SnapshotHash string        `json:"snapshot_hash"`
}

// CanonicalBucket is the fixed width every fact is keyed by (phase 1).
const CanonicalBucket = time.Minute

// CeilToBucket rounds an instant UP to a canonical bucket boundary, which is what an
// ordinary prospective write uses for its EffectiveAt.
//
// A write landing exactly on a boundary uses that boundary. The equality case is stated
// because it is where two implementations diverge, and because ceiling and flooring
// disagree at every other instant: a write at 12:00:30 governs from 12:01:00.
func CeilToBucket(t time.Time) time.Time {
	floored := t.Truncate(CanonicalBucket)
	if floored.Equal(t) {
		return t
	}
	return floored.Add(CanonicalBucket)
}

// FloorToBucket rounds down. Only the one retroactive case — a first-adoption backfill —
// uses it, and what it produces is a declared reconstruction rather than a measurement.
func FloorToBucket(t time.Time) time.Time { return t.Truncate(CanonicalBucket) }
