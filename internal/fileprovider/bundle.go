// Package fileprovider implements the Monitoring-as-Code file provider (spec
// func-monitoring-as-code, FR-017/NFR-014). This file holds the pure contract layer:
// strict ProjectBundle decoding, canonicalization, validation, and deterministic planning.
// It performs NO database or filesystem-watch work — those are later iterations. The
// application/store apply path and the watcher build on these pure values.
package fileprovider

import (
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Reason is a bounded, low-cardinality rejection code. It never contains raw YAML, secret
// values, or unbounded text — safe for logs, status records, and metric outcome buckets.
type Reason string

const (
	ReasonInvalidFormat     Reason = "invalid_format"
	ReasonUnknownField      Reason = "unknown_field"
	ReasonDuplicateKey      Reason = "duplicate_key"
	ReasonScopeMismatch     Reason = "scope_mismatch"
	ReasonInvalidUID        Reason = "invalid_uid"
	ReasonInvalidDuration   Reason = "invalid_duration"
	ReasonServerOwnedField  Reason = "server_owned_field"
	ReasonInlineSecret      Reason = "inline_secret_forbidden"
	ReasonUnsupportedType   Reason = "unsupported_type"
	ReasonUnsupportedField  Reason = "unsupported_field"
	ReasonDomainInvalid     Reason = "domain_invalid"
	ReasonDependencyInvalid Reason = "dependency_invalid"
	ReasonDependencyCycle   Reason = "dependency_cycle"
	ReasonSecretRefNotFound Reason = "secret_ref_not_found"
	ReasonFeatureDisabled   Reason = "feature_disabled"
	ReasonTypeChange        Reason = "type_change"
	ReasonDuplicateProject  Reason = "duplicate_project"
	ReasonEmptyBundle       Reason = "empty_bundle"
	ReasonQuotaExceeded     Reason = "max_managed_monitors"
)

// BundleError is a reconcile-facing error carrying a bounded reason and a message that is
// safe to log/surface (no secrets, no raw YAML). It anchors an optional monitor UID.
type BundleError struct {
	Reason Reason
	UID    string // optional: the offending monitor UID
	Msg    string
	// Org/Project are set when the rejection happened AFTER the bundle's tenant was resolved
	// (a monitor-level or dependency error in an otherwise-bound bundle). They let the grouping
	// layer freeze just that project instead of suspending orphaning provider-wide (§9.1): a
	// tenant-bindable invalid is per-project, not an unbound error. Empty when the tenant could
	// not be resolved (format/scope/tenant errors), which remain provider-wide.
	Org     string
	Project string
}

func (e *BundleError) Error() string {
	if e.UID != "" {
		return string(e.Reason) + " [" + e.UID + "]: " + e.Msg
	}
	return string(e.Reason) + ": " + e.Msg
}

func rejectf(r Reason, uid, format string, args ...any) *BundleError {
	return &BundleError{Reason: r, UID: uid, Msg: fmt.Sprintf(format, args...)}
}

// bindTenant tags a post-tenant-resolution rejection with the resolved (org, project) so the
// grouping layer can freeze just that project instead of suspending orphaning provider-wide
// (§9.1: a tenant-bindable invalid is per-project). Non-*BundleError values pass through.
func bindTenant(err error, org, project string) error {
	if be, ok := err.(*BundleError); ok {
		be.Org, be.Project = org, project
	}
	return err
}

// DesiredMonitor is one normalized, validated monitor from a bundle (pre-apply). ProjectID
// and server-owned fields are intentionally unset — they are resolved in the apply
// transaction. DependsOn holds source UIDs (sorted+deduped), resolved to DB IDs at apply.
type DesiredMonitor struct {
	UID       string
	Monitor   domain.Monitor
	DependsOn []string
	Hash      string // canonical semantic hash (§7)
}

// DesiredProject is a fully parsed, validated bundle for one resolved tenant.
type DesiredProject struct {
	// Format is the bundle format this project was declared in. It is carried because
	// ABSENCE means different things per format: a format-1 bundle cannot express services
	// at all, so its silence about them is not a statement, and downgrading a file must not
	// silently orphan every service it used to declare.
	Format       int
	Organization string
	Project      string
	Monitors     map[string]DesiredMonitor // by source UID
	// Services is the format-2 resource map, keyed by service slug. A format-1 bundle
	// always yields an empty map, never nil, so callers need no version branch.
	Services map[string]DesiredService
}

// fileSupportedTypes are the monitor types the v1 file provider can express through either
// common fields or a strict typed settings schema. The credentialed subset uses inventory
// references only; `promql` carries one non-secret field, `query` (D-0145 addendum,
// 2026-09-01). `composite` and `synthetic` remain rejected — children-by-UID and a
// multi-step definition are schema problems, not a missing field — as does any future
// untyped shape. This is an explicit scope boundary, not a generic config escape hatch.
var fileSupportedTypes = map[domain.MonitorType]bool{
	domain.MonitorHTTP:      true,
	domain.MonitorTCP:       true,
	domain.MonitorICMP:      true,
	domain.MonitorDNS:       true,
	domain.MonitorTLS:       true,
	domain.MonitorGRPC:      true,
	domain.MonitorWebSocket: true,
	domain.MonitorSSH:       true,
	domain.MonitorPush:      true,
	domain.MonitorPostgres:  true,
	domain.MonitorMySQL:     true,
	domain.MonitorRedis:     true,
	domain.MonitorRabbitMQ:  true,
	domain.MonitorPromQL:    true,
	// FR-029: admitted only after the nested typed schema, the secret-ref contract, the semantic
	// hash and their tests all existed (D12). It is the one type that carries a nested `workflow`
	// block rather than a flat `settings` map — which is exactly why `synthetic` is still refused:
	// a JSON string inside a bundle is an unvalidated document inside a validated one.
	domain.MonitorAsyncCanary: true,
}

// secretSettingKeys are field/settings keys that carry credentials. Their presence anywhere
// in a bundle rejects it as inline_secret_forbidden — a bundle never carries a secret.
var secretSettingKeys = map[string]bool{
	"password": true, "passphrase": true, "token": true, "bot_token": true,
	"secret": true, "client_secret": true, "smtp_password": true, "private_key": true,
	"cookie": true, "credentials": true, "api_key": true, "apikey": true, "auth": true,
}
