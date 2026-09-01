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
	// Services arrives with format 2. It is declared here rather than in a separate struct
	// because KnownFields(true) rejects anything absent from the type — so the format gate
	// below is what keeps a format-1 bundle from quietly gaining a resource map.
	Services map[string]rawService `yaml:"services"`
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
	// Slug is the project-unique, immutable reference key a service names this monitor by.
	// Format 2 declares it explicitly; omitted, it defaults to the map key, which IS the
	// provider source uid — so the same Git-tracked bundle resolves to the same slug on
	// every installation.
	Slug string `yaml:"slug"`
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
	// Recover a BINDABLE tenant identity from the parsed header up front, from the node tree
	// (not the typed struct), so that even a strict typed-decode failure below (unknown monitor
	// field, wrong field type, a nested duplicate key) — OR a structural-policy rejection
	// (custom tag / depth / node-count) in a bundle whose header is unambiguous — can be
	// attributed to a project. §9.1 draws the line at "cannot be associated with a tenant", not
	// "the decode/policy check failed". Extracting from the already-parsed `doc` node is safe
	// even if the policy check below rejects the document. A bindable error freezes just that
	// project; only a genuinely unbindable one (ambiguous/duplicate/malformed header, or a scope
	// that leaves the tenant undetermined) suspends orphaning provider-wide. bind() is a no-op
	// when the tenant is unbindable, so those errors stay unbound.
	boundOrg, boundProject, tenantOK := bindableTenant(scope, &doc)
	bind := func(err error) error {
		if tenantOK {
			return bindTenant(err, boundOrg, boundProject)
		}
		return err
	}

	// Structural policy (spec §14): bound nesting depth + total node count and reject
	// custom/non-core YAML tags, BEFORE binding to the typed struct. A policy rejection in a
	// header-unambiguous bundle is bound to its project (see above) so it freezes per-project
	// instead of suspending orphaning provider-wide.
	if be := checkYAMLPolicy(&doc, 0, new(int)); be != nil {
		return nil, bind(be)
	}

	var raw rawBundle
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // unknown root/monitor fields (incl. server-owned) reject
	if err := dec.Decode(&raw); err != nil {
		return nil, bind(decodeError(err))
	}
	// Exactly one document per file.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, bind(rejectf(ReasonInvalidFormat, "", "a bundle file must contain exactly one YAML document"))
	}

	if raw.Format == nil {
		return nil, bind(rejectf(ReasonInvalidFormat, "", "root `format` is required"))
	}
	switch *raw.Format {
	case 1, 2:
	default:
		return nil, bind(rejectf(ReasonInvalidFormat, "", "unsupported bundle format %d (want 1 or 2)", *raw.Format))
	}
	if *raw.Format < 2 {
		// Format 1 stays exactly what it was. A resource map or a field that arrived with a
		// later format is refused rather than silently ignored: a bundle whose services were
		// quietly dropped would look applied and change nothing.
		if raw.Services != nil {
			return nil, bind(rejectf(ReasonInvalidFormat, "", "`services` requires bundle format 2"))
		}
		for uid, m := range raw.Monitors {
			if m.Slug != "" {
				return nil, bind(rejectf(ReasonInvalidFormat, uid, "monitor `slug` requires bundle format 2"))
			}
		}
	}

	// Authoritative scope-contract resolution from the typed header. On success it equals the
	// header-derived bindable tenant; on failure (a scope violation) the error is still bound
	// when the tenant is determinable, else provider-wide.
	org, project, err := resolveTenant(scope, raw.Organization, raw.Project)
	if err != nil {
		return nil, bind(err)
	}
	// The authoritative resolution now wins over the up-front node-scan for any POST-resolve
	// error (a monitor-level/dependency/empty-bundle rejection below): rebind to it, so a header
	// expressed through decoder-resolved forms the raw node-scan cannot see (YAML merge
	// keys/anchors) is still attributed to its project. EXCEPT when a tenant header is directly
	// declared with a non-string value (organization: 123): yaml coerces it to a string for the
	// typed struct, but a numeric/bool value is not a slug (D3) — adopting it would bind a
	// rejection to a bogus tenant. A header absent from the direct node content (resolved only
	// through a merge-key/anchor) is NOT poisoned, so that legitimate case still rebinds.
	if !tenantHeaderPoisoned(&doc) {
		boundOrg, boundProject, tenantOK = org, project, true
	}
	if raw.Monitors == nil {
		return nil, bind(rejectf(ReasonEmptyBundle, "", "root `monitors` map is required (empty map is allowed, but the key must be present)"))
	}

	dp := &DesiredProject{Format: *raw.Format, Organization: org, Project: project, Monitors: make(map[string]DesiredMonitor, len(raw.Monitors))}
	for uid, rm := range raw.Monitors {
		dm, err := buildMonitor(uid, rm)
		if err != nil {
			return nil, bind(err)
		}
		dp.Monitors[uid] = dm
	}
	if err := checkDependencyDAG(dp); err != nil {
		return nil, bind(err)
	}

	// Services (format 2). The map is always present, never nil, so callers need no version
	// branch: a format-1 bundle simply declares none.
	slugs := make(map[string]bool, len(dp.Monitors))
	for uid, dm := range dp.Monitors {
		slug := raw.Monitors[uid].Slug
		if slug == "" {
			// An omitted slug defaults to the map key, which IS the provider source uid —
			// so the same Git-tracked bundle resolves to the same slug everywhere.
			slug = uid
		}
		if !domain.ValidMonitorSlug(slug) {
			return nil, bind(rejectf(ReasonDomainInvalid, uid,
				"monitor slug %q must match %s", slug, domain.MonitorSlugPattern()))
		}
		dm.Monitor.Slug = slug
		dp.Monitors[uid] = dm
		if slugs[slug] {
			return nil, bind(rejectf(ReasonDomainInvalid, uid, "duplicate monitor slug %q in this bundle", slug))
		}
		slugs[slug] = true
	}
	services, err := decodeServices(raw.Services)
	if err != nil {
		return nil, bind(err)
	}
	dp.Services = services
	return dp, nil
}

// bindableTenant recovers the (org, project) a bundle can be attributed to, from the parsed
// header node, per the scope→tenant matrix (§5) but WITHOUT the scope-contract validation
// resolveTenant enforces — the point is orphan-safety attribution (§9.1), not acceptance:
//   - project scope: the tenant is static (from config), always bindable;
//   - organization scope: bindable when the header has exactly one unambiguous scalar `project`;
//   - instance scope: bindable when the header has unique scalar `organization` AND `project`.
//
// A missing, repeated, or non-scalar tenant key leaves the tenant undetermined (ok=false), so a
// decode failure there stays provider-wide.
func bindableTenant(scope config.ProviderScopeConfig, doc *yaml.Node) (org, project string, ok bool) {
	hdrOrg, hdrProject := tenantHeaderFromNode(doc)
	switch scope.Type {
	case config.ProviderScopeProject:
		return scope.Organization, scope.Project, true
	case config.ProviderScopeOrganization:
		if hdrProject != "" {
			return scope.Organization, hdrProject, true
		}
	case config.ProviderScopeInstance:
		if hdrOrg != "" && hdrProject != "" {
			return hdrOrg, hdrProject, true
		}
	}
	return "", "", false
}

// tenantHeaderFromNode reads the root `organization`/`project` scalars from the parsed document
// for tenant binding. It returns a value ONLY for an unambiguous header: a key present exactly
// once with a scalar value. A key that is absent, repeated (duplicate root key), or non-scalar
// (a map/seq) yields "" so the tenant is treated as undetermined (§9.1).
func tenantHeaderFromNode(doc *yaml.Node) (org, project string) {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return "", ""
	}
	return uniqueScalarValue(root, "organization"), uniqueScalarValue(root, "project")
}

// tenantHeaderPoisoned reports whether the root header DIRECTLY declares `organization` or
// `project` with a value that is present but NOT a plain string (a numeric/bool/other-tagged
// scalar, or a map/seq). Such a value is not a slug (D3), so the authoritative rebind must not
// adopt the decoder's coerced form. A key resolved only through a merge-key/anchor is ABSENT
// from the direct content and therefore NOT poisoned, so that legitimate rebind still proceeds.
func tenantHeaderPoisoned(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return false
	}
	poisoned := func(key string) bool {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != key {
				continue
			}
			if v := root.Content[i+1]; v.Kind != yaml.ScalarNode || (v.Tag != "" && v.Tag != "!!str") {
				return true
			}
		}
		return false
	}
	return poisoned("organization") || poisoned("project")
}

// uniqueScalarValue returns the scalar value of key in a mapping node only when the key appears
// exactly once with a scalar value; otherwise "".
func uniqueScalarValue(m *yaml.Node, key string) string {
	var val string
	seen := 0
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		seen++
		// A tenant slug is a plain string: accept only a scalar carrying the core string tag
		// (untagged or !!str). A numeric/bool/other-tagged value (organization: 123, project:
		// true, a !!binary, …) is NOT a slug → treat the tenant as undetermined/unbound.
		if v := m.Content[i+1]; v.Kind == yaml.ScalarNode && (v.Tag == "" || v.Tag == "!!str") {
			val = v.Value
		} else {
			return "" // non-scalar / non-string-tagged tenant header → undetermined
		}
	}
	if seen != 1 {
		return "" // absent or duplicated → not unambiguous
	}
	return val
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
	// A URL-style target must not carry credentials, in its userinfo OR its query string:
	// either is an inline secret, stored/echoed in cleartext and usable directly. All guards
	// are checked before type support so a secret is never echoed through a different reason,
	// and no message ever includes the raw target or a query value:
	//   1. A well-formed URL whose userinfo is populated (https://user:pass@host, a
	//      postgres://user:pass@host DSN, password-only https://:pass@host) is rejected.
	//      url.Parse only populates User when the target has a real URL authority, so bare
	//      host:port, ICMP/SSH hostnames and other non-URL targets never trip this.
	//   2. A well-formed URL whose query carries a known secret-bearing key (token, api_key,
	//      password, …) is rejected, reusing the same finite secretSettingKeys set that
	//      classifies inline settings secrets — https://h/?token=… is an inline secret just
	//      like https://user:token@h. A query that cannot be cleanly decoded is rejected
	//      conservatively, since we then cannot prove it carries no such key.
	//   3. A URL-SHAPED target (carries a "://" scheme separator) that FAILS to parse is
	//      also rejected: a parse failure (e.g. an invalid percent-escape like
	//      https://u:pw@h/%zz, or a control character in the URL) means we cannot prove it is
	//      free of embedded credentials, and domain validation only checks the target is
	//      non-empty — so a malformed-but-credentialed target would otherwise be persisted
	//      verbatim (the P1 bypass).
	// See D-0152 / spec func-monitoring-as-code §14.
	target := strings.TrimSpace(rm.Target)
	if u, err := url.Parse(target); err == nil {
		if u.User != nil {
			return DesiredMonitor{}, rejectf(ReasonInlineSecret, uid, "target carries credentials in its URL userinfo; inline secrets are forbidden")
		}
		if u.RawQuery != "" {
			vals, qerr := url.ParseQuery(u.RawQuery)
			if qerr != nil {
				return DesiredMonitor{}, rejectf(ReasonInlineSecret, uid, "target query string cannot be decoded and cannot be verified free of inline credentials; move any credentials to a secret_ref")
			}
			for k := range vals {
				if secretSettingKeys[strings.ToLower(strings.TrimSpace(k))] {
					return DesiredMonitor{}, rejectf(ReasonInlineSecret, uid, "target query key %q carries a secret; inline secrets are forbidden", k)
				}
			}
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
	// ONE entry point for every typed `settings` object: the credential registry for a
	// credentialed type, the plain schema for a type that has one (`promql`), and an error
	// naming the key for a type with neither. The branch this replaced read "settings IF AND
	// ONLY IF credentialed", which was never the rule — §3.1 asks for a strict non-secret
	// schema, and a credential is one way to have one (D-0145 addendum, 2026-09-01).
	settings, serr := domain.PrepareTypedSettings(typ, rm.Settings, domain.SurfaceFile)
	if serr != nil {
		reason := ReasonDomainInvalid
		if strings.Contains(serr.Error(), "has no setting") {
			reason = ReasonUnsupportedField
		}
		return DesiredMonitor{}, rejectf(reason, uid, "%s", serr.Error())
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
		Config:       settings,
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
