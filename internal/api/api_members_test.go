package api_test

import (
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestAddMemberByEmail(t *testing.T) {
	h := newHandler(seededStore())
	// By email of an existing user (u1@x → u1).
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"email":"u1@x","role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add by email = %d, want 201", rec.Code)
	}
	// Unknown email → 400 (must sign in first).
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"email":"ghost@x","role":"viewer"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("add unknown email = %d, want 400", rec.Code)
	}
	// user_id still works.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"user_id":"u1","role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add by user_id = %d, want 201", rec.Code)
	}
	// A non-admin still can't add members.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/organizations/o1/members", `{"email":"u1@x","role":"viewer"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer add = %d, want 403", rec.Code)
	}
}

func seededWithMember() *fakeStore {
	fs := seededStore()
	fs.members["o1"] = []domain.Membership{{ID: "m1", OrgID: "o1", UserID: "u1", Role: domain.RoleViewer}}
	return fs
}

func TestUpdateMemberAuthz(t *testing.T) {
	h := newHandler(seededWithMember())
	if rec := do(h, o1Viewer, http.MethodPatch, "/api/v1/organizations/o1/members/m1", `{"role":"editor"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer update = %d, want 403", rec.Code)
	}
	if rec := do(h, outsider, http.MethodPatch, "/api/v1/organizations/o1/members/m1", `{"role":"editor"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider update = %d, want 404", rec.Code)
	}
	// Membership lives in o1, not o2 → hidden.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o2/members/m1", `{"role":"editor"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org update = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/m9", `{"role":"editor"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown member = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/m1", `{"role":"boss"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid role = %d, want 400", rec.Code)
	}
	// A project role is not valid at org scope.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/m1", `{"role":"project_admin"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("project role at org scope = %d, want 400", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/m1", `{"role":"editor"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin update = %d, want 200", rec.Code)
	}
}

func TestRemoveMemberAuthz(t *testing.T) {
	h := newHandler(seededWithMember())
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/organizations/o1/members/m1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/organizations/o1/members/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/organizations/o1/members/m1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again = %d, want 404", rec.Code)
	}
}

func TestLastOrgAdminGuard(t *testing.T) {
	fs := seededStore()
	fs.members["o1"] = append(fs.members["o1"], domain.Membership{ID: "adm1", OrgID: "o1", UserID: "oa", Role: domain.RoleOrgAdmin})
	h := newHandler(fs)
	// The sole org-level admin can't be demoted or removed.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/adm1", `{"role":"viewer"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("demote last admin = %d, want 400", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/organizations/o1/members/adm1", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("remove last admin = %d, want 400", rec.Code)
	}
	// With a second admin present, demotion is allowed.
	fs.members["o1"] = append(fs.members["o1"], domain.Membership{ID: "adm2", OrgID: "o1", UserID: "u1", Role: domain.RoleOrgAdmin})
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/adm1", `{"role":"viewer"}`); rec.Code != http.StatusOK {
		t.Fatalf("demote with two admins = %d, want 200", rec.Code)
	}
}

func TestLastOrgAdminGuardSkippedForGlobalAdmin(t *testing.T) {
	// A global admin may demote or remove an org's sole org_admin — the org is
	// never locked out because the global admin can appoint a replacement
	// (consistent with deleting the account from the Users page).
	fs := seededStore()
	fs.members["o1"] = append(fs.members["o1"], domain.Membership{ID: "adm1", OrgID: "o1", UserID: "oa", Role: domain.RoleOrgAdmin})
	h := newHandler(fs)
	if rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/organizations/o1/members/adm1", `{"role":"viewer"}`); rec.Code != http.StatusOK {
		t.Fatalf("global-admin demote last org admin = %d, want 200", rec.Code)
	}
	if rec := do(h, globalAdmin, http.MethodDelete, "/api/v1/organizations/o1/members/adm1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("global-admin remove last org admin = %d, want 204", rec.Code)
	}
}
