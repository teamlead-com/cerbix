package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// D-0180 — one axis for "which projects does this page report", asked in both directions.
//
// Three surfaces answered this differently and each was wrong in its own way: the render forgot the
// page's own project, the feed walked `components → monitors` so a Service component brought
// nothing, and the subscriber fan-out was that same monitor JOIN. The visible consequence is a page
// showing an incident and emailing nobody about it.
//
// The cases below are the two the old spellings each lost, and they are deliberately NOT masked by a
// monitor component of the same project.
func TestAPageProjectSetIsOneAxisInBothDirections(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	own, err := st.CreateProject(ctx, org.ID, "own", "Own")
	if err != nil {
		t.Fatalf("own project: %v", err)
	}
	other, err := st.CreateProject(ctx, org.ID, "other", "Other")
	if err != nil {
		t.Fatalf("other project: %v", err)
	}

	// A project-scoped page whose only component is MANUAL: no monitor and no service anywhere.
	manualPage, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, ProjectID: own.ID, Slug: "manual", Title: "Manual",
		Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("manual page: %v", err)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: manualPage.ID, Name: "Typed by hand",
	}); err != nil {
		t.Fatalf("manual component: %v", err)
	}

	got, err := st.StatusPageProjectIDs(ctx, manualPage.ID)
	if err != nil {
		t.Fatalf("page projects: %v", err)
	}
	if len(got) != 1 || got[0] != own.ID {
		t.Fatalf("a project-scoped page with only manual components reports %v, want its OWN "+
			"project — the render dropped it and the feed kept it, so the page and its mail "+
			"disagreed about what the page is about", got)
	}

	// An ORG-level page whose only component is backed by a SERVICE in another project. Nothing here
	// is monitor-backed, which is exactly what the old `components → monitors` JOIN could not see.
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: other.ID, Slug: "checkout", Name: "Checkout"})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svcPage, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "svc", Title: "Service only", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("service page: %v", err)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: svcPage.ID, Name: "Checkout", Source: "service",
		ServiceID: svc.ID, SourceProject: other.ID,
	}); err != nil {
		t.Fatalf("service component: %v", err)
	}

	got, err = st.StatusPageProjectIDs(ctx, svcPage.ID)
	if err != nil {
		t.Fatalf("page projects: %v", err)
	}
	if len(got) != 1 || got[0] != other.ID {
		t.Fatalf("a Service-only org page reports %v, want the service's project %s", got, other.ID)
	}

	// The INVERSE has to agree, or a reader sees an incident on the page and gets no mail about it.
	sub, err := st.CreateSubscriber(ctx, domain.Subscriber{
		StatusPageID: svcPage.ID, Email: "reader@x.com", ConfirmToken: "tok-svc",
	})
	if err != nil {
		t.Fatalf("subscriber: %v", err)
	}
	if _, err := st.ConfirmSubscriber(ctx, sub.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	emails, err := st.ConfirmedSubscriberEmailsForProject(ctx, other.ID)
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	if len(emails) != 1 || emails[0] != "reader@x.com" {
		t.Fatalf("fan-out for a Service-only page = %v, want the page's subscriber: the incident "+
			"renders on that page and the mail about it went to nobody", emails)
	}

	// The page's OWN project is the other half of the inverse.
	ownSub, err := st.CreateSubscriber(ctx, domain.Subscriber{
		StatusPageID: manualPage.ID, Email: "owner@x.com", ConfirmToken: "tok-own",
	})
	if err != nil {
		t.Fatalf("own subscriber: %v", err)
	}
	if _, err := st.ConfirmSubscriber(ctx, ownSub.ConfirmToken); err != nil {
		t.Fatalf("confirm own: %v", err)
	}
	emails, err = st.ConfirmedSubscriberEmailsForProject(ctx, own.ID)
	if err != nil {
		t.Fatalf("own fanout: %v", err)
	}
	if len(emails) != 1 || emails[0] != "owner@x.com" {
		t.Fatalf("fan-out for the page's own project = %v, want its subscriber", emails)
	}

	// And it stays a SET, not a cross-join: the other project's subscriber is not on this list.
	for _, e := range emails {
		if e == "reader@x.com" {
			t.Fatal("a subscriber of a different page received another project's mail")
		}
	}
}

// A DORMANT binding keeps its `source_project`, and the renderer resolves such a component to that
// project, so the axis must not filter by `source`. Filtering would narrow the mail relative to what
// the page shows — the same disagreement, in the other direction.
func TestADormantBindingKeepsThePageReportingItsProject(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	page, _ := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "s", Title: "S", Visibility: domain.VisibilityPublic,
	})
	comp, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: page.ID, Name: "API", MonitorID: mon.ID, Source: "monitor", SourceProject: proj.ID,
	})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	// Converted to manual, binding kept dormant — what `buildConversionPlan` leaves behind.
	if _, err := st.pool.Exec(ctx,
		`UPDATE components SET source = 'manual' WHERE id = $1`, comp.ID); err != nil {
		t.Fatalf("convert: %v", err)
	}

	got, err := st.StatusPageProjectIDs(ctx, page.ID)
	if err != nil {
		t.Fatalf("page projects: %v", err)
	}
	if len(got) != 1 || got[0] != proj.ID {
		t.Fatalf("a dormant binding stopped the page reporting %s: got %v — the renderer still "+
			"resolves that component to the project, so the mail would narrow below the page",
			proj.ID, got)
	}
}
