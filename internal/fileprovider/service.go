package fileprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Services in bundle format 2 (spec func-service-reliability §15.2; D-0159).
//
// `func-monitoring-as-code` §3 admits new top-level resource maps only in a LATER bundle
// format, so services arrive with `format: 2` and format 1 stays valid and unchanged — a
// format-1 bundle simply declares no services and is never rewritten or upgraded implicitly.
//
// The reference key is the MONITOR SLUG, not the provider source UID. A provider UID is
// provider-local ownership identity, and a UI-owned service may name a file-owned monitor
// (and the reverse), so the cross-owner reference has to be something both sides can see.

// rawService is the strict wire shape of one service. Unknown keys are rejected by the
// decoder's KnownFields(true) — there is no field here for them to land in.
type rawService struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Owner       *rawServiceOwner `yaml:"owner"`
	Monitors    []string         `yaml:"monitors"`
	SLI         []string         `yaml:"sli"`
	Aggregation *rawAggregation  `yaml:"aggregation"`
	Region      *rawRegionPolicy `yaml:"region"`
	MissingData string           `yaml:"missing_data"`
	Maintenance string           `yaml:"maintenance"`
	Freshness   *rawFreshness    `yaml:"freshness"`
	// Alerting is the §16.6a paging declaration. It is a POINTER because absent and empty are
	// different statements here (see decodeServiceAlerting).
	Alerting *rawServiceAlerting `yaml:"alerting"`
}

// rawServiceAlerting is the strict wire shape of the §16.6a paging declaration — the four
// DECLARED fields and nothing server-owned. `alert_config_generation`, the latches and the
// leases have no field here at all, so a bundle that names one is refused by KnownFields(true)
// rather than silently ignored: they are the database's, and a hash that moved because an alert
// FIRED would make the bundle reapply forever.
//
// Every scalar is a pointer so "omitted" is distinguishable from a meaningful zero:
// `confirm_evaluations: 0` must be REFUSED by the shared validator (bounds are 1..10), while an
// omitted one takes the documented default of 2. Reusing the zero value for both would silently
// accept the one spelling the spec forbids.
type rawServiceAlerting struct {
	OwnsPaging *bool `yaml:"owns_paging"`
	// PageOn is a plain slice, not a pointer, because yaml.v3 already distinguishes what
	// matters: an omitted key decodes to nil (→ the default `{down}`), and `page_on: []`
	// decodes to a non-nil empty slice (→ §16.6a's "explicitly page for no state", which
	// dis-arms LIVE). An explicit `page_on:` null reads as omitted, exactly as every other
	// null does in this package.
	PageOn             []string `yaml:"page_on"`
	PageOnUnknown      *bool    `yaml:"page_on_unknown"`
	ConfirmEvaluations *int     `yaml:"confirm_evaluations"`
	// RenotifySeconds is the repeat cadence for the service's own escalation ladder (D-0185). A
	// pointer for the same reason as the field above: `renotify_seconds: 0` is a MEANINGFUL value —
	// it says "never repeat" — and it must be distinguishable from an omitted key, which takes the
	// same default. They agree today, and encoding them identically would make a future change of
	// default silently rewrite what every bundle meant.
	RenotifySeconds *int `yaml:"renotify_seconds"`
}

type rawServiceOwner struct {
	EscalationPolicy string `yaml:"escalation_policy"`
	OncallSchedule   string `yaml:"oncall_schedule"`
}

type rawAggregation struct {
	Mode        string `yaml:"mode"`
	DegradedMin *int   `yaml:"degraded_min"`
	HealthyMin  *int   `yaml:"healthy_min"`
}

type rawRegionPolicy struct {
	Mode               string `yaml:"mode"`
	DegradedMinRegions *int   `yaml:"degraded_min_regions"`
	HealthyMinRegions  *int   `yaml:"healthy_min_regions"`
}

type rawFreshness struct {
	ActiveMultiplier *int   `yaml:"active_multiplier"`
	ActiveFloor      string `yaml:"active_floor"`
}

// DesiredService is one parsed, validated service declaration.
//
// Monitors and SLI hold monitor SLUGS as written in the bundle; resolving them to ids is the
// apply path's job, because only the database knows which slug is which row.
type DesiredService struct {
	Slug        string
	Name        string
	Description string

	EscalationPolicy string
	OncallSchedule   string

	Monitors []string
	SLI      []string
	Policies domain.ServicePolicies

	// Alerting is the §16.6a paging declaration, canonical and already validated. It is NEVER nil
	// for a parsed format-2 service: an absent block declares the DEFAULT policy, because a file
	// that describes the desired state cannot let history decide what a service pages for. NIL when
	// the bundle declares nothing about paging at all.
	//
	// Nil is not "the default policy". A service that has ownership ON and whose declaration
	// loses its `alerting:` block must not be disowned by that silence, and a service that
	// never had ownership must not gain it — so the apply writes the declaration's columns only when
	// this is non-nil, exactly as §15.2's format gate treats a format-1 bundle's silence about
	// `services` as no statement rather than as "delete them all".
	Alerting *domain.ServiceAlertPolicy

	// Hash is the canonical semantic hash. An unchanged hash MUST NOT create a definition
	// revision — the direct analogue of §7's "MUST NOT call the semantic monitor update
	// path".
	Hash string
}

const (
	maxServiceNameLen        = 120
	maxServiceDescriptionLen = 500
	maxServiceMembers        = 200
)

// decodeServices normalizes and validates the `services` map.
//
// It checks SHAPE only. A service may legitimately reference a monitor this bundle does not
// own — a file-managed service naming a UI-managed monitor is explicitly allowed — so an
// unresolvable slug is not a parse error; it is resolved, or refused, at apply time against
// the project, which is the only place that knows what exists.
func decodeServices(raw map[string]rawService) (map[string]DesiredService, error) {
	if len(raw) == 0 {
		return map[string]DesiredService{}, nil
	}
	out := make(map[string]DesiredService, len(raw))
	for slug, rs := range raw {
		svc, err := decodeService(slug, rs)
		if err != nil {
			return nil, err
		}
		out[slug] = svc
	}
	return out, nil
}

func decodeService(slug string, rs rawService) (DesiredService, error) {
	if !domain.ValidServiceSlug(slug) {
		return DesiredService{}, rejectf(ReasonDomainInvalid, slug,
			"service key %q must match %s", slug, domain.MonitorSlugPattern())
	}
	name := strings.TrimSpace(rs.Name)
	if name == "" {
		name = slug
	}
	if len(name) > maxServiceNameLen {
		return DesiredService{}, rejectf(ReasonDomainInvalid, slug, "service `name` exceeds %d characters", maxServiceNameLen)
	}
	if len(rs.Description) > maxServiceDescriptionLen {
		return DesiredService{}, rejectf(ReasonDomainInvalid, slug, "service `description` exceeds %d characters", maxServiceDescriptionLen)
	}

	monitors, err := normalizeMemberList(slug, "monitors", rs.Monitors)
	if err != nil {
		return DesiredService{}, err
	}
	sli, err := normalizeMemberList(slug, "sli", rs.SLI)
	if err != nil {
		return DesiredService{}, err
	}
	inContext := map[string]bool{}
	for _, m := range monitors {
		inContext[m] = true
	}
	for _, m := range sli {
		if !inContext[m] {
			// The two lists are declared INDEPENDENTLY, but an SLI member outside the
			// operational context would be a number with no visible source.
			return DesiredService{}, rejectf(ReasonDomainInvalid, slug,
				"sli member %q is not in `monitors`; every reliability input must also be operational context", m)
		}
	}

	policies, err := decodeServicePolicies(slug, rs)
	if err != nil {
		return DesiredService{}, err
	}

	alerting, err := decodeServiceAlerting(slug, rs.Alerting)
	if err != nil {
		return DesiredService{}, err
	}

	svc := DesiredService{
		Slug: slug, Name: name, Description: rs.Description,
		Monitors: monitors, SLI: sli, Policies: policies, Alerting: alerting,
	}
	if rs.Owner != nil {
		svc.EscalationPolicy = strings.TrimSpace(rs.Owner.EscalationPolicy)
		svc.OncallSchedule = strings.TrimSpace(rs.Owner.OncallSchedule)
	}
	svc.Hash = canonicalServiceHash(svc)
	return svc, nil
}

// normalizeMemberList sorts and deduplicates a member list.
//
// Both lists are SET-like: order carries nothing, so it is normalized away before hashing —
// otherwise reordering two lines in a YAML file would look like a redefinition of what
// availability means.
func normalizeMemberList(slug, field string, in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		m := strings.TrimSpace(raw)
		if m == "" {
			return nil, rejectf(ReasonDomainInvalid, slug, "service `%s` contains an empty entry", field)
		}
		if !domain.ValidMonitorSlug(m) {
			return nil, rejectf(ReasonDomainInvalid, slug,
				"service `%s` entry %q must be a monitor slug matching %s", field, m, domain.MonitorSlugPattern())
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) > maxServiceMembers {
		return nil, rejectf(ReasonDomainInvalid, slug, "service `%s` exceeds %d members", field, maxServiceMembers)
	}
	sort.Strings(out)
	return out, nil
}

func decodeServicePolicies(slug string, rs rawService) (domain.ServicePolicies, error) {
	var p domain.ServicePolicies

	if rs.Aggregation != nil {
		p.Aggregation.Mode = domain.AggMode(strings.TrimSpace(rs.Aggregation.Mode))
		if rs.Aggregation.DegradedMin != nil {
			p.Aggregation.DegradedMin = *rs.Aggregation.DegradedMin
		}
		if rs.Aggregation.HealthyMin != nil {
			p.Aggregation.HealthyMin = *rs.Aggregation.HealthyMin
		}
	}
	if rs.Region != nil {
		p.Region.Mode = domain.RegionMode(strings.TrimSpace(rs.Region.Mode))
		if rs.Region.DegradedMinRegions != nil {
			p.Region.DegradedMinRegions = *rs.Region.DegradedMinRegions
		}
		if rs.Region.HealthyMinRegions != nil {
			p.Region.HealthyMinRegions = *rs.Region.HealthyMinRegions
		}
	}
	if md := strings.TrimSpace(rs.MissingData); md != "" {
		p.MissingData = domain.MissingDataPolicy(md)
		switch p.MissingData {
		case domain.MissingUnknown, domain.MissingBad, domain.MissingIgnore:
		default:
			return domain.ServicePolicies{}, rejectf(ReasonDomainInvalid, slug, "unknown `missing_data` policy %q", md)
		}
	}
	if mt := strings.TrimSpace(rs.Maintenance); mt != "" {
		p.Maintenance = domain.MaintenancePolicy(mt)
		if p.Maintenance != domain.MaintenanceExclude {
			return domain.ServicePolicies{}, rejectf(ReasonDomainInvalid, slug, "unknown `maintenance` policy %q", mt)
		}
	}
	if rs.Freshness != nil {
		if rs.Freshness.ActiveMultiplier != nil {
			p.Freshness.ActiveMultiplier = *rs.Freshness.ActiveMultiplier
		}
		if f := strings.TrimSpace(rs.Freshness.ActiveFloor); f != "" {
			d, err := time.ParseDuration(f)
			if err != nil || d <= 0 {
				return domain.ServicePolicies{}, rejectf(ReasonDomainInvalid, slug,
					"service `freshness.active_floor` must be a positive duration, got %q", f)
			}
			p.Freshness.ActiveFloor = d
		}
	}
	switch p.Aggregation.Mode {
	case "", domain.AggAll, domain.AggAny, domain.AggQuorum:
	default:
		return domain.ServicePolicies{}, rejectf(ReasonDomainInvalid, slug,
			"unknown `aggregation.mode` %q", p.Aggregation.Mode)
	}
	switch p.Region.Mode {
	case "", domain.RegionPer, domain.RegionAny, domain.RegionAll:
	default:
		return domain.ServicePolicies{}, rejectf(ReasonDomainInvalid, slug,
			"unknown `region.mode` %q", p.Region.Mode)
	}
	return p, nil
}

// decodeServiceAlerting maps the `alerting:` block to the canonical, validated paging policy
// (spec §16.6a), or to nil when the block is absent.
//
// Two things happen here that deliberately do NOT happen at apply time:
//
//   - Canonicalization. `page_on` is sorted and deduplicated by the domain's own Canonical(),
//     so re-ordering two entries in a YAML file is not a change: it must not move the hash,
//     must not reapply, and must not bump `alert_config_generation` — which would dis-arm
//     delegation and page the members for an edit nobody made.
//   - Validation, through the ONE validator §16.6a gives the API and the MaC apply. Doing it at
//     PARSE time is what makes a bad bundle a file-named rejection that keeps its
//     last-known-good, instead of a half-applied transaction discovering the bounds after the
//     services before it in the same bundle were already written.
//
// The order is canonical THEN validate, matching the store's UI path exactly, so a declaration
// can never be accepted in one spelling and refused in another.
func decodeServiceAlerting(slug string, ra *rawServiceAlerting) (*domain.ServiceAlertPolicy, error) {
	// An ABSENT block is the DEFAULT policy, not silence — the file is the desired state, and a
	// desired state that depends on what the file used to say is not declarative. An earlier
	// revision returned nil here and the apply skipped the columns, which meant a bundle that once
	// declared `owns_paging: true` and then dropped the block left the service still owning paging:
	// the same file converged to two different databases depending on history, and for a
	// file-managed service the UI cannot even correct it (§16.6a refuses those edits with a 409).
	// Converging to the default is also safe for existing bundles, because a service that never
	// declared alerting already holds exactly these values.
	//
	// Inside a PRESENT block the same defaults apply per field, so the two readings agree.
	p := domain.DefaultServiceAlertPolicy()
	if ra == nil {
		return &p, nil
	}
	if ra.OwnsPaging != nil {
		p.OwnsPaging = *ra.OwnsPaging
	}
	if ra.PageOn != nil {
		states := make([]domain.ServiceAlertState, 0, len(ra.PageOn))
		for _, s := range ra.PageOn {
			states = append(states, domain.ServiceAlertState(strings.TrimSpace(s)))
		}
		p.PageOn = states
	}
	if ra.PageOnUnknown != nil {
		p.PageOnUnknown = *ra.PageOnUnknown
	}
	if ra.ConfirmEvaluations != nil {
		p.ConfirmEvaluations = *ra.ConfirmEvaluations
	}
	if ra.RenotifySeconds != nil {
		p.RenotifySeconds = *ra.RenotifySeconds
	}
	p = p.Canonical()
	if err := p.Validate(); err != nil {
		return nil, rejectf(ReasonDomainInvalid, slug, "%s", err.Error())
	}
	return &p, nil
}

// canonicalService is the stable projection hashed for the create/update/no-op decision.
//
// It covers the DECLARATION only. Server-owned values — revision numbers, epoch ids,
// generations, sealed state — are absent by construction, so applying the same file twice is
// a no-op even after the service has produced facts.
type canonicalService struct {
	Slug             string                 `json:"slug"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	EscalationPolicy string                 `json:"escalation_policy"`
	OncallSchedule   string                 `json:"oncall_schedule"`
	Monitors         []string               `json:"monitors"`
	SLI              []string               `json:"sli"`
	Policies         domain.ServicePolicies `json:"policies"`
	// The paging declaration is DELIBERATELY ABSENT from this hash, and that is a correctness
	// decision rather than an omission.
	//
	// This hash decides create/update/NO-OP, and the update branch calls
	// `putServiceDeclarationTx`, which creates a definition revision AND its evaluation epoch
	// unconditionally. §16.6a is explicit that paging fields must bump neither: they change who
	// is paged, not what is measured, and an epoch bump would re-segment reliability history for
	// an alerting edit. Putting `alerting:` in here would have done exactly that on every
	// `owns_paging` toggle.
	//
	// The apply therefore reconciles the paging declaration on EVERY branch — create, update and
	// no-op alike — against the row itself, which is idempotent by construction and needs no hash
	// to notice a change. `alert_config_generation`, the latches, the leases and `firing` inside
	// a burn rule are absent for the ordinary reason: they are server-owned, and a hash that
	// moved because an alert fired would make the bundle reapply forever.
}

// canonicalServiceHash computes the semantic hash of a normalized service.
//
// The member lists are already sorted and deduplicated, and json.Marshal emits map keys in sorted
// order — so the encoding is
// deterministic across parses of semantically-equal YAML: comments, key order, indentation, the
// order two `page_on` states were typed in, and the file's path and mtime cannot move it.
func canonicalServiceHash(svc DesiredService) string {
	c := canonicalService{
		Slug: svc.Slug, Name: svc.Name, Description: svc.Description,
		EscalationPolicy: svc.EscalationPolicy, OncallSchedule: svc.OncallSchedule,
		Monitors: svc.Monitors, SLI: svc.SLI, Policies: svc.Policies,
	}
	if c.Monitors == nil {
		c.Monitors = []string{}
	}
	if c.SLI == nil {
		c.SLI = []string{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		// canonicalService holds only strings, slices and plain structs; Marshal cannot fail.
		panic(fmt.Sprintf("fileprovider: canonical service marshal: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
