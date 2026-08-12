package fileprovider

import (
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// Format-1 contract defaults (spec §6.3). These are contract constants, NOT the mutable
// instance/UI monitor defaults — a later format is required to change them.
const (
	fmtDefaultMethod           = "GET"
	fmtDefaultIntervalSeconds  = 60
	fmtDefaultTimeoutSeconds   = 10
	fmtDefaultRetries          = 0
	fmtDefaultFailureThreshold = 1
	fmtDefaultRegion           = "core"
)

// rawBundle is the strict wire shape of one ProjectBundle file.
type rawBundle struct {
	Format       *int                  `yaml:"format"`
	Organization string                `yaml:"organization"`
	Project      string                `yaml:"project"`
	Monitors     map[string]rawMonitor `yaml:"monitors"`
}

// rawMonitor is the strict wire shape of one monitor. Pointers/strings distinguish "absent"
// from a meaningful zero so contract defaults apply only when a field is truly omitted.
// Unknown keys (including any server-owned field like id/status/execution_revision) are
// rejected by the decoder's KnownFields(true) — no field for them exists here.
type rawMonitor struct {
	Name             string            `yaml:"name"`
	Type             string            `yaml:"type"`
	Target           string            `yaml:"target"`
	Method           string            `yaml:"method"`
	Interval         string            `yaml:"interval"`
	Timeout          string            `yaml:"timeout"`
	Retries          *int              `yaml:"retries"`
	FailureThreshold *int              `yaml:"failure_threshold"`
	ConfirmInterval  string            `yaml:"confirm_interval"`
	Renotify         string            `yaml:"renotify"`
	Grace            string            `yaml:"grace"`
	Conditions       []string          `yaml:"conditions"`
	Tags             []string          `yaml:"tags"`
	Region           string            `yaml:"region"`
	Enabled          *bool             `yaml:"enabled"`
	AutoIncident     *bool             `yaml:"auto_incident"`
	DependsOn        []string          `yaml:"depends_on"`
	Settings         map[string]string `yaml:"settings"`
}

// Decode strict-parses one bundle's bytes under a resolved provider scope, producing a
// normalized, domain-validated DesiredProject with per-monitor canonical hashes and an
// in-bundle dependency DAG check. It performs no I/O and touches no database. Any violation
// returns a *BundleError with a bounded reason.
func Decode(data []byte, scope config.ProviderScopeConfig) (*DesiredProject, error) {
	// Structural policy first (spec §14): bound nesting depth + total node count and reject
	// custom/non-core YAML tags, BEFORE binding to the typed struct. yaml.v3 already bounds
	// alias expansion; this adds the explicit Cerbix bound + tag allowlist.
	var doc yaml.Node
	if err := yaml.NewDecoder(strings.NewReader(string(data))).Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, rejectf(ReasonInvalidFormat, "", "empty bundle")
		}
		return nil, decodeError(err)
	}
	if be := checkYAMLPolicy(&doc, 0, new(int)); be != nil {
		return nil, be
	}

	var raw rawBundle
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // unknown root/monitor fields (incl. server-owned) reject
	if err := dec.Decode(&raw); err != nil {
		return nil, decodeError(err)
	}
	// Exactly one document per file.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, rejectf(ReasonInvalidFormat, "", "a bundle file must contain exactly one YAML document")
	}

	if raw.Format == nil {
		return nil, rejectf(ReasonInvalidFormat, "", "root `format` is required")
	}
	if *raw.Format != 1 {
		return nil, rejectf(ReasonInvalidFormat, "", "unsupported bundle format %d (want 1)", *raw.Format)
	}

	org, project, err := resolveTenant(scope, raw.Organization, raw.Project)
	if err != nil {
		return nil, err
	}
	if raw.Monitors == nil {
		return nil, rejectf(ReasonEmptyBundle, "", "root `monitors` map is required (empty map is allowed, but the key must be present)")
	}

	dp := &DesiredProject{Organization: org, Project: project, Monitors: make(map[string]DesiredMonitor, len(raw.Monitors))}
	for uid, rm := range raw.Monitors {
		dm, err := buildMonitor(uid, rm)
		if err != nil {
			return nil, err
		}
		dp.Monitors[uid] = dm
	}
	if err := checkDependencyDAG(dp); err != nil {
		return nil, err
	}
	return dp, nil
}

// YAML structural bounds (spec §14): defense against deeply-nested / oversized / custom-tagged
// documents beyond yaml.v3's built-in alias-expansion protection.
const (
	maxYAMLDepth = 32
	maxYAMLNodes = 10000
)

// allowedYAMLTags is the core-schema tag allowlist. Any other tag — a custom `!Foo`, or a
// non-core `!!binary`/`!!python/...` — rejects the bundle (custom_tag, spec §14).
var allowedYAMLTags = map[string]bool{
	"": true, "!!str": true, "!!int": true, "!!float": true, "!!bool": true,
	"!!null": true, "!!map": true, "!!seq": true, "!!merge": true, "!!timestamp": true,
}

// checkYAMLPolicy walks the parsed node tree enforcing max depth, max total node count, and
// the tag allowlist. count is threaded by pointer so the whole document shares one budget.
func checkYAMLPolicy(n *yaml.Node, depth int, count *int) *BundleError {
	if n == nil {
		return nil
	}
	if depth > maxYAMLDepth {
		return rejectf(ReasonInvalidFormat, "", "bundle nesting exceeds the maximum depth %d", maxYAMLDepth)
	}
	*count++
	if *count > maxYAMLNodes {
		return rejectf(ReasonInvalidFormat, "", "bundle exceeds the maximum node count %d", maxYAMLNodes)
	}
	// An alias references an anchored subtree. Follow it and count that subtree against the SAME
	// budget on EVERY use, so a billion-laughs alias bomb (aliases of aliases) blows maxYAMLNodes
	// / maxYAMLDepth and is rejected rather than expanding on decode (spec §14). Alias nodes
	// carry no user tag, so they skip the tag allowlist.
	if n.Kind == yaml.AliasNode {
		if n.Alias != nil {
			return checkYAMLPolicy(n.Alias, depth+1, count)
		}
		return nil
	}
	if !allowedYAMLTags[n.Tag] {
		return rejectf(ReasonInvalidFormat, "", "unsupported or custom YAML tag %q", n.Tag)
	}
	for _, c := range n.Content {
		if be := checkYAMLPolicy(c, depth+1, count); be != nil {
			return be
		}
	}
	return nil
}

// resolveTenant applies the scope→tenant-fields matrix (spec §5) and returns the resolved
// (organization, project) slug pair used as the tenant key everywhere downstream.
func resolveTenant(scope config.ProviderScopeConfig, bundleOrg, bundleProject string) (org, project string, err error) {
	switch scope.Type {
	case config.ProviderScopeProject:
		if bundleOrg != "" || bundleProject != "" {
			return "", "", rejectf(ReasonScopeMismatch, "", "project-scoped provider forbids bundle organization/project fields")
		}
		return scope.Organization, scope.Project, nil
	case config.ProviderScopeOrganization:
		if bundleOrg != "" {
			return "", "", rejectf(ReasonScopeMismatch, "", "organization-scoped provider forbids the bundle `organization` field")
		}
		if bundleProject == "" {
			return "", "", rejectf(ReasonScopeMismatch, "", "organization-scoped bundle requires a `project`")
		}
		return scope.Organization, bundleProject, nil
	case config.ProviderScopeInstance:
		if bundleOrg == "" || bundleProject == "" {
			return "", "", rejectf(ReasonScopeMismatch, "", "instance-scoped bundle requires both `organization` and `project`")
		}
		return bundleOrg, bundleProject, nil
	default:
		return "", "", rejectf(ReasonScopeMismatch, "", "provider scope type %q is not resolvable", scope.Type)
	}
}

// buildMonitor maps one raw monitor to a normalized, validated domain.Monitor. It applies
// the format-1 defaults, rejects inline secrets and unsupported types/fields, converts
// durations to whole seconds, runs domain Normalize+Validate, and computes the canonical
// hash.
func buildMonitor(uid string, rm rawMonitor) (DesiredMonitor, error) {
	if !uidRe.MatchString(uid) {
		return DesiredMonitor{}, rejectf(ReasonInvalidUID, uid, "monitor UID must match %s", uidRe.String())
	}
	// Inline secrets are forbidden anywhere, checked before type support so a secret is
	// never echoed through a different reason.
	for k := range rm.Settings {
		if secretSettingKeys[strings.ToLower(strings.TrimSpace(k))] {
			return DesiredMonitor{}, rejectf(ReasonInlineSecret, uid, "settings key %q carries a secret; inline secrets are forbidden", k)
		}
	}
	// A URL-style target must not carry credentials in its userinfo (e.g.
	// https://user:pass@host or a postgres://user:pass@host DSN): that is an inline secret,
	// stored/echoed in cleartext and usable as Basic Auth. Two guards, both checked before
	// type support so a secret is never echoed through a different reason, and neither
	// message ever includes the raw target:
	//   1. A well-formed URL whose userinfo is populated is rejected. url.Parse only
	//      populates User when the target has a real URL authority, so bare host:port,
	//      ICMP/SSH hostnames and other non-URL targets never trip this.
	//   2. A URL-SHAPED target (carries a "://" scheme separator) that FAILS to parse is
	//      also rejected: a parse failure means we cannot prove it is free of embedded
	//      credentials (e.g. https://u:pw@h/%zz or a control char after the authority), and
	//      domain validation only checks the target is non-empty — so a malformed-but-
	//      credentialed target would otherwise be persisted verbatim (the P1 bypass).
	// Secret-like query parameters remain out of scope (see D-0152 / spec func-mac §14):
	// arbitrary ?k=v cannot be reliably classified as secret vs. legitimate in v1.
	target := strings.TrimSpace(rm.Target)
	if u, err := url.Parse(target); err == nil {
		if u.User != nil {
			return DesiredMonitor{}, rejectf(ReasonInlineSecret, uid, "target carries credentials in its URL userinfo; inline secrets are forbidden")
		}
	} else if strings.Contains(target, "://") {
		return DesiredMonitor{}, rejectf(ReasonInlineSecret, uid, "target is a malformed URL and cannot be verified free of inline credentials; fix the URL or move any credentials to a secret_ref")
	}

	typ := domain.MonitorType(rm.Type)
	if rm.Type == "" || !typ.Valid() {
		return DesiredMonitor{}, rejectf(ReasonUnsupportedType, uid, "unknown monitor type %q", rm.Type)
	}
	if !fileSupportedTypes[typ] {
		return DesiredMonitor{}, rejectf(ReasonUnsupportedType, uid, "monitor type %q is not yet available via the file provider (needs a strict non-secret settings/secret_ref contract)", rm.Type)
	}
	// The supported v1 types have no typed `settings` object; any settings key is unsupported.
	for k := range rm.Settings {
		return DesiredMonitor{}, rejectf(ReasonUnsupportedField, uid, "type %q has no file-provider setting %q", rm.Type, k)
	}

	m := domain.Monitor{
		Name:         strings.TrimSpace(rm.Name),
		Type:         typ,
		Target:       strings.TrimSpace(rm.Target),
		Method:       orDefault(rm.Method, fmtDefaultMethod),
		Conditions:   rm.Conditions,
		Tags:         rm.Tags,
		Region:       orDefault(rm.Region, fmtDefaultRegion),
		Enabled:      boolOr(rm.Enabled, true),
		AutoIncident: boolOr(rm.AutoIncident, true),
		DependsOn:    normStringSet(rm.DependsOn),
	}
	if m.Name == "" {
		return DesiredMonitor{}, rejectf(ReasonDomainInvalid, uid, "monitor `name` is required")
	}

	var derr error
	if m.IntervalSeconds, derr = durSeconds(uid, "interval", rm.Interval, fmtDefaultIntervalSeconds); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.TimeoutSeconds, derr = durSeconds(uid, "timeout", rm.Timeout, fmtDefaultTimeoutSeconds); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.ConfirmIntervalSeconds, derr = durSeconds(uid, "confirm_interval", rm.ConfirmInterval, 0); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.RenotifySeconds, derr = durSeconds(uid, "renotify", rm.Renotify, 0); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.GraceSeconds, derr = durSeconds(uid, "grace", rm.Grace, 0); derr != nil {
		return DesiredMonitor{}, derr
	}
	m.Retries = intOr(rm.Retries, fmtDefaultRetries)
	m.FailureThreshold = intOr(rm.FailureThreshold, fmtDefaultFailureThreshold)

	// Domain normalization + validation is the single source of business rules; the provider
	// never re-implements them. A sentinel ProjectID satisfies the domain's tenant check —
	// the real project_id is bound in the apply transaction.
	m.Normalize()
	vm := m
	vm.ProjectID = "file-validate"
	if err := vm.Validate(); err != nil {
		return DesiredMonitor{}, rejectf(ReasonDomainInvalid, uid, "%s", err.Error())
	}

	return DesiredMonitor{UID: uid, Monitor: m, DependsOn: m.DependsOn, Hash: canonicalHash(uid, m)}, nil
}

// durSeconds parses a duration string to whole seconds; empty → def. Fractional-second and
// negative values reject (spec §6.3).
func durSeconds(uid, field, s string, def int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, rejectf(ReasonInvalidDuration, uid, "%s: invalid duration %q", field, s)
	}
	if d < 0 {
		return 0, rejectf(ReasonInvalidDuration, uid, "%s: duration must not be negative", field)
	}
	if d%time.Second != 0 {
		return 0, rejectf(ReasonInvalidDuration, uid, "%s: duration must be a whole number of seconds, got %s", field, s)
	}
	return int(d / time.Second), nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}
