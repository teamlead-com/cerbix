package api_test

import (
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// The API contract says `Omit to keep the current status (a plain comment)`, and the handler used to
// break it by filling the field from the incident it had just read — in a DIFFERENT transaction from
// the write, so a transition committed in between was silently reverted. The store now resolves the
// intent against a locked row, but only if the intent survives the handler. This is the regression
// the store-level test cannot give, because that one calls the store directly.
func TestAPlainCommentReachesTheStoreAsAKeepCurrentIntent(t *testing.T) {
	fs := seededStore()
	fs.incidents["inc1"] = domain.Incident{
		ID: "inc1", ProjectID: "p1", Title: "api degraded",
		Status: domain.IncidentIdentified, Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}
	h := newHandler(fs)
	editor := authz.Principal{UserID: "ed", Memberships: []domain.Membership{
		{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor},
	}}

	rec := do(h, editor, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"body":"adding a note"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plain comment: %d %s", rec.Code, rec.Body.String())
	}
	if fs.lastUpdateStatus != "" {
		t.Fatalf("the handler passed status %q for a comment that carried none — echoing the status "+
			"it read is exactly what reverts a transition committed since", fs.lastUpdateStatus)
	}

	// An explicit status still travels untouched.
	rec = do(h, editor, http.MethodPost, "/api/v1/incidents/inc1/updates",
		`{"status":"monitoring","body":"fix applied"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("explicit status: %d %s", rec.Code, rec.Body.String())
	}
	if fs.lastUpdateStatus != string(domain.IncidentMonitoring) {
		t.Fatalf("the handler passed %q, want monitoring", fs.lastUpdateStatus)
	}
}
