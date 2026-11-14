package store_test

import (
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// S14 (sec-hardening-2 H-14): a page made `internal` mails no subscriber. The fan-out serves the
// pages a subscriber can still open — `public` and `unlisted` — and stops the moment either is
// made `internal`; making the page visible again resumes it (the confirmed rows are kept, not
// deleted, so the owner's subscriber list survives a temporary hide).
func TestSubscribersOfAPageMadeInternalReceiveNoIncidentMail(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	for _, from := range []domain.Visibility{domain.VisibilityPublic, domain.VisibilityUnlisted} {
		page, err := st.CreateStatusPage(ctx, domain.StatusPage{
			OrgID: org.ID, ProjectID: proj.ID, Slug: "page-" + string(from), Title: "P", Visibility: from,
			UnlistedToken: map[bool]string{true: "tok-" + string(from)}[from == domain.VisibilityUnlisted],
		})
		if err != nil {
			t.Fatalf("%s page: %v", from, err)
		}
		email := string(from) + "@reader.test"
		sub, err := st.CreateSubscriber(ctx, domain.Subscriber{StatusPageID: page.ID, Email: email, ConfirmToken: "c-" + string(from)})
		if err != nil {
			t.Fatalf("subscriber: %v", err)
		}
		if _, err := st.ConfirmSubscriber(ctx, sub.ConfirmToken); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !fannedOut(ctx, t, st, proj.ID, email) {
			t.Fatalf("%s page: its confirmed subscriber is not in the fan-out", from)
		}

		page.Visibility, page.UnlistedToken = domain.VisibilityInternal, ""
		if _, err := st.UpdateStatusPage(ctx, page); err != nil {
			t.Fatalf("make internal: %v", err)
		}
		if fannedOut(ctx, t, st, proj.ID, email) {
			t.Errorf("%s → internal: the subscriber still receives the project's incident mail", from)
		}

		page.Visibility = from
		if from == domain.VisibilityUnlisted {
			page.UnlistedToken = "tok-again"
		}
		if _, err := st.UpdateStatusPage(ctx, page); err != nil {
			t.Fatalf("make %s again: %v", from, err)
		}
		if !fannedOut(ctx, t, st, proj.ID, email) {
			t.Errorf("internal → %s: the confirmed subscriber did not come back", from)
		}
	}
}

// fannedOut reports whether the project's incident fan-out reaches email.
func fannedOut(ctx context.Context, t *testing.T, st *store.Store, projectID, email string) bool {
	t.Helper()
	emails, err := st.ConfirmedSubscriberEmailsForProject(ctx, projectID)
	if err != nil {
		t.Fatalf("fan-out: %v", err)
	}
	for _, e := range emails {
		if e == email {
			return true
		}
	}
	return false
}
