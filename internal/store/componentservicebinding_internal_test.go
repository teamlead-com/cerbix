package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// A component can be CREATED bound to a service, and the binding is resolved within the page's
// organization.
//
// Creation with a service binding was unreachable through the product: `openapi.yaml` declared
// `service_id`, the SPA sent it, `domain.Component` carried it and this store already inserted it —
// the API handler was the one place that did not accept the field, and its strict decoder answered
// `400 invalid JSON body`. So the store half had never been exercised for a service, and the
// tenancy of a create had never been asked of one either.
//
// The mutation that must kill the first half: drop `ServiceID` from the create body in
// `internal/api`. The mutation for the second: resolve the binding by id alone, without the
// organization.
func TestAComponentCanBeCreatedBoundToAServiceInItsOwnOrg(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	svc, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "billing", Name: "Billing"})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	c, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "billing", ServiceID: svc.ID,
	})
	if err != nil {
		t.Fatalf("a service-bound component must be creatable: %v", err)
	}
	// The SOURCE is derived from the binding rather than taken from the caller, which is what the
	// API contract promises and what makes the component render as a service.
	if c.Source != domain.ComponentSourceService {
		t.Fatalf("source = %q, want %q", c.Source, domain.ComponentSourceService)
	}
	if c.ServiceID != svc.ID {
		t.Fatalf("service binding = %q, want %q", c.ServiceID, svc.ID)
	}
	if c.SourceProject != f.projectID {
		t.Fatalf("binding project = %q, want the service's project %q", c.SourceProject, f.projectID)
	}
}

// A binding from ANOTHER organization is refused, and refused the same way a missing one is.
//
// Before the repair the create path resolved a binding by id alone: tenancy depended entirely on
// the caller above it, and for a service there was no such caller — the API could not send one.
// Accepting `service_id` without this scope would have opened the P0 §15.0 records, a direct writer
// binding another organization's service to a component.
func TestACreateRefusesABindingOutsideThePagesOrg(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	org2, err := st.CreateOrganization(ctx, "other", "Other")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj2, err := st.CreateProject(ctx, org2.ID, "p2", "P2")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	alienSvc, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "Alien"})
	if err != nil {
		t.Fatalf("alien service: %v", err)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "alien", ServiceID: alienSvc.ID,
	}); !errors.Is(err, ErrComponentBindingNotFound) {
		t.Fatalf("foreign service binding = %v, want ErrComponentBindingNotFound", err)
	}
	// A service id that exists NOWHERE answers identically: the difference between "not yours" and
	// "not there" is a cross-tenant existence oracle, which is why `monitorInOrg` has said the same
	// since FR-021.
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "ghost",
		ServiceID: "00000000-0000-4000-8000-0000000000ff",
	}); !errors.Is(err, ErrComponentBindingNotFound) {
		t.Fatalf("absent service binding = %v, want the same ErrComponentBindingNotFound", err)
	}
	// The MONITOR half of the same resolver keeps the rule too — it had the scope only by way of
	// the API's own check, and now the store states it as well.
	alienMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj2.ID, Name: "alien-mon", Slug: "alien-mon", Type: domain.MonitorHTTP,
		Target: "https://alien.example.com", IntervalSeconds: 60, TimeoutSeconds: 5, Region: domain.DefaultRegion,
	})
	if err != nil {
		t.Fatalf("alien monitor: %v", err)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "alien-mon", MonitorID: alienMon.ID,
	}); !errors.Is(err, ErrComponentBindingNotFound) {
		t.Fatalf("foreign monitor binding = %v, want ErrComponentBindingNotFound", err)
	}
}

// A binding from the SAME organization but a project the page is not scoped to is refused in Go,
// with a reason, rather than by the deferred trigger at COMMIT (reviewer P2).
//
// The conversion path has refused this since [314] P1-4; a create read the page's project and then
// did not consult it, so the two paths disagreed about when the operator learns. The mutation that
// must kill this: drop the `assertBindingInPageScope` call from `CreateComponent`.
func TestACreateRefusesABindingOutsideThePagesProject(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	// Same org, different project than the page is scoped to.
	other, err := st.CreateService(ctx, domain.Service{ProjectID: f.otherProj, Slug: "s", Name: "S"})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	_, err = st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "out-of-scope", ServiceID: other.ID,
	})
	if !errors.Is(err, ErrComponentConversionTarget) {
		t.Fatalf("out-of-scope binding = %v, want a named refusal", err)
	}
	if !strings.Contains(err.Error(), "scoped") {
		t.Fatalf("the refusal must name the page scope, got %q", err)
	}
	// An ORG-level page is scoped to no project, so the same service is accepted there: the rule is
	// about the page's scope and not about the service.
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageOrg, Name: "org-level", ServiceID: other.ID,
	}); err != nil {
		t.Fatalf("an org-level page accepts any of its org's services: %v", err)
	}
	// And a MANUAL component still creates on a project-scoped page: it has no binding, so the
	// scope rule has nothing to compare and must not fire.
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "manual",
	}); err != nil {
		t.Fatalf("a manual component must still be creatable on a scoped page: %v", err)
	}
}

// A lookup that FAILED and a binding that is ABSENT are two different answers.
//
// `bindingProjectTx` returns `ErrComponentConversionTarget` when no row matched the organization —
// that is a caller error, and the API must answer 400. Every other error it can return is the
// database not answering, and the API must answer 500. The first version of this repair mapped
// both onto one absent-binding result, so a connection failure reached the operator as
// `400 binding not found`:
// an answer that is not merely unhelpful but false, sending them after a deleted service while the
// database was down. No DB test could see that — the store tests only ever produce the absent case.
//
// The mutation that must kill this: return one generic error for every input in
// `bindingLookupError`.
func TestALookupFailureIsNotAnAbsentBinding(t *testing.T) {
	// The absent case keeps its meaning, through a wrap, because the caller compares with errors.Is.
	absent := bindingLookupError("service", fmt.Errorf("select: %w", ErrComponentConversionTarget))
	if !errors.Is(absent, ErrComponentBindingNotFound) {
		t.Fatalf("an absent binding = %v, want ErrComponentBindingNotFound", absent)
	}

	// The infrastructure case does NOT, and it carries its cause for the log `serverError` writes.
	down := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	got := bindingLookupError("service", down)
	if errors.Is(got, ErrComponentBindingNotFound) {
		t.Fatalf("a database failure was reported as an absent binding: %v", got)
	}
	if !errors.Is(got, down) {
		t.Fatalf("the cause was dropped: %v", got)
	}
	if !strings.Contains(got.Error(), "service") {
		t.Fatalf("the message does not say which binding failed: %v", got)
	}
}

// A create that carries BOTH bindings is held to the same dormant-pair rule as a conversion.
//
// A component has ONE `source_project` column and may retain the binding it is not currently
// sourced from. Conversion has asserted since [314] P1-4 that the two can live under that one
// column, because the schema would otherwise refuse the pair as an opaque constraint error at
// COMMIT — after the operator consented to a preview that promised it would work. Create left the
// same question to the deferred trigger, so the two ways a row acquires a pair answered it
// differently: one with a sentence, one with a constraint name.
//
// The mutation that must kill this: drop the `assertRetainedBindingsSameProjectTx` call from
// `CreateComponent`.
func TestACreateWithBothBindingsRefusesAMismatchedPair(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	// The DORMANT half in another project: the active binding is the service, so the monitor is
	// what `source_project` cannot also describe.
	strayMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.otherProj, Name: "search-http", Type: domain.MonitorHTTP,
		Target: "https://search.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("stray monitor: %v", err)
	}
	_, err = st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "mismatched pair",
		ServiceID: f.serviceID, MonitorID: strayMon.ID,
	})
	if !errors.Is(err, ErrComponentConversionTarget) {
		t.Fatalf("a mismatched retained pair = %v, want a named refusal", err)
	}

	// The control: the same two kinds of binding in the SAME project create, the service wins the
	// source as `componentSourceOf` documents, and the monitor is retained rather than dropped.
	got, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "matched pair",
		ServiceID: f.serviceID, MonitorID: f.monitorID,
	})
	if err != nil {
		t.Fatalf("a matched pair must create: %v", err)
	}
	if got.Source != domain.ComponentSourceService {
		t.Fatalf("source = %q, want service", got.Source)
	}
	if got.MonitorID != f.monitorID || got.ServiceID != f.serviceID {
		t.Fatalf("the pair did not survive: monitor=%q service=%q", got.MonitorID, got.ServiceID)
	}
}
