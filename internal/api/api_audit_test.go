package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestAuditRecordedAndListed(t *testing.T) {
	h := newHandler(seededStore())

	// Adding a member records an audit entry.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"user_id":"u1","role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add member = %d, want 201", rec.Code)
	}

	// Org admin can read the audit trail and sees the member.add action.
	rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit list = %d, want 200", rec.Code)
	}
	var entries []domain.AuditEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) == 0 || entries[0].Action != "member.add" {
		t.Fatalf("audit entries = %+v, want a member.add first", entries)
	}
}

func TestGenericAuditUsesSyntheticTokenIdentityContract(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	p := authz.Principal{
		UserID:      authz.SyntheticTokenActorPrefix + "org-admin-token",
		ViaToken:    true,
		Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}},
	}
	if rec := do(h, p, http.MethodPost, "/api/v1/organizations/o1/members", `{"user_id":"u1","role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("token add member = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	if len(fs.audit) == 0 {
		t.Fatal("token mutation wrote no audit row")
	}
	got := fs.audit[0]
	if got.Action != "member.add" || got.ActorUserID != "" || !got.ViaToken {
		t.Fatalf("generic token audit = %+v, want NULL actor + via_token", got)
	}
}

func TestAuditListAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, outsider, http.MethodGet, "/api/v1/organizations/o1/audit", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider audit = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1/audit", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer audit = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/audit", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin audit = %d, want 200", rec.Code)
	}
}
