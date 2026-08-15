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

	svc := DesiredService{
		Slug: slug, Name: name, Description: rs.Description,
		Monitors: monitors, SLI: sli, Policies: policies,
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
}

// canonicalServiceHash computes the semantic hash of a normalized service.
//
// The member lists are already sorted and deduplicated, and json.Marshal emits map keys in
// sorted order, so the encoding is deterministic across parses of semantically-equal YAML:
// comments, key order, indentation and the file's path and mtime cannot move it.
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
