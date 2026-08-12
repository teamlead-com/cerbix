package fileprovider

import (
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
)

func orgScope() config.ProviderScopeConfig {
	return config.ProviderScopeConfig{Type: config.ProviderScopeOrganization, Organization: "acme"}
}

// TestDecodeBindsTenantOnMonitorError: a monitor-level error after the tenant resolves is
// attributed to that project (§9.1), so it can be frozen per-project, not provider-wide.
func TestDecodeBindsTenantOnMonitorError(t *testing.T) {
	y := "format: 1\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: bogus\n    target: t\n"
	_, err := Decode([]byte(y), orgScope())
	var be *BundleError
	if !errors.As(err, &be) || be.Reason != ReasonUnsupportedType {
		t.Fatalf("want unsupported_type BundleError, got %v", err)
	}
	if be.Org != "acme" || be.Project != "payments" {
		t.Fatalf("monitor error must carry the resolved tenant, got %q/%q", be.Org, be.Project)
	}
}

// TestDecodeBindsTenantOnStrictDecodeError is the reviewer's requested regression: a strict
// typed-decode failure (unknown monitor field) fires BEFORE resolveTenant, but the header
// tenant is unambiguous, so the rejection must still bind to that project.
func TestDecodeBindsTenantOnStrictDecodeError(t *testing.T) {
	y := "format: 1\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://x\n    bogus_field: y\n"
	_, err := Decode([]byte(y), orgScope())
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want *BundleError, got %v", err)
	}
	if be.Org != "acme" || be.Project != "payments" {
		t.Fatalf("strict-decode error with a valid header must bind the tenant, got %q/%q (reason %s)", be.Org, be.Project, be.Reason)
	}
}

// TestDecodeProjectScopeAlwaysBindable: project scope has a static tenant, so even a
// strict-decode failure binds to the scope's project.
func TestDecodeProjectScopeAlwaysBindable(t *testing.T) {
	y := "format: 1\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://x\n    bogus_field: y\n"
	scope := config.ProviderScopeConfig{Type: config.ProviderScopeProject, Organization: "acme", Project: "payments"}
	_, err := Decode([]byte(y), scope)
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want *BundleError, got %v", err)
	}
	if be.Org != "acme" || be.Project != "payments" {
		t.Fatalf("project scope must bind to the static tenant, got %q/%q", be.Org, be.Project)
	}
}

// TestDecodeUnboundOnAmbiguousHeader: a duplicate root tenant key is ambiguous, so the
// rejection stays UNBOUND (provider-wide orphan suspension) — the safe side.
func TestDecodeUnboundOnAmbiguousHeader(t *testing.T) {
	y := "format: 1\norganization: acme\norganization: beta\nproject: payments\nmonitors: {}\n"
	_, err := Decode([]byte(y), instanceScope())
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want *BundleError, got %v", err)
	}
	if be.Org != "" || be.Project != "" {
		t.Fatalf("ambiguous/duplicate tenant header must stay unbound, got %q/%q", be.Org, be.Project)
	}
}

// TestDecodeUnboundOnMissingInstanceHeader: instance scope with no org/project header cannot
// determine a tenant → unbound.
func TestDecodeUnboundOnMissingInstanceHeader(t *testing.T) {
	y := "format: 1\nmonitors:\n  api:\n    name: A\n    type: bogus\n    target: t\n"
	_, err := Decode([]byte(y), instanceScope())
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want *BundleError, got %v", err)
	}
	if be.Org != "" || be.Project != "" {
		t.Fatalf("missing instance tenant header must stay unbound, got %q/%q", be.Org, be.Project)
	}
}
