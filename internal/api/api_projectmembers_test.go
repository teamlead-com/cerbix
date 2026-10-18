package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0206 (FR-040, func-project-members.md): a project admin manages the members of their own
// project through /api/v1/projects/{projectID}/members.

var (
	p1Admin = authz.Principal{UserID: "pa", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
	p2Admin = authz.Principal{UserID: "pb", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p2", Role: domain.RoleProjectAdmin}}}
	// A cbx_ API token scoped to p1 as project_admin, with no allow-list ("role decides").
	p1Token = authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "t1", ViaToken: true,
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
	// The same token with an allow-list that names project:manage.
	p1TokenAllowed = authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "t2", ViaToken: true,
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}},
		Actions:     []authz.Action{authz.ActionProjectManage, authz.ActionProjectRead}}
	// An OIDC client-credentials principal: a real user id, ViaToken set by the auth layer.
	p1JWT = authz.Principal{UserID: "svc", ViaToken: true,
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
)

// projectMembersStore: p1 has a project member (m-p1), p2 has one (m-p2), o1 has an org-level
// member (m-org), and o2's project p3 has one (m-p3).
func projectMembersStore() *fakeStore {
	fs := seededStore()
	fs.users["u2"] = domain.User{ID: "u2", Email: "u2@x", DisplayName: "Two"}
	fs.users["u3"] = domain.User{ID: "u3", Email: "u3@x", DisplayName: "Three"}
	fs.users["u4"] = domain.User{ID: "u4", Email: "u4@x", DisplayName: "Four"}
	fs.members["o1"] = []domain.Membership{
		{ID: "m-org", OrgID: "o1", UserID: "u1", Role: domain.RoleEditor},
		{ID: "m-p1", OrgID: "o1", ProjectID: "p1", UserID: "u2", Role: domain.RoleViewer},
		{ID: "m-p2", OrgID: "o1", ProjectID: "p2", UserID: "u3", Role: domain.RoleViewer},
	}
	fs.members["o2"] = []domain.Membership{
		{ID: "m-p3", OrgID: "o2", ProjectID: "p3", UserID: "u4", Role: domain.RoleViewer},
	}
	return fs
}

func TestProjectAdminManagesTheMembersOfTheirProject(t *testing.T) {
	fs := projectMembersStore()
	h := newHandler(fs)

	rec := do(h, p1Admin, http.MethodGet, "/api/v1/projects/p1/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Members   []map[string]any `json:"members"`
		Inherited []map[string]any `json:"inherited"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Members) != 1 || got.Members[0]["id"] != "m-p1" {
		t.Fatalf("members = %+v, want only m-p1", got.Members)
	}
	if len(got.Inherited) != 1 || got.Inherited[0]["email"] != "u1@x" || got.Inherited[0]["role"] != "editor" {
		t.Fatalf("inherited = %+v, want o1's org-level u1 as editor", got.Inherited)
	}
	// The inherited shape carries no membership id, user id or last activity.
	for _, k := range []string{"id", "user_id", "last_active_at", "org_id", "project_id"} {
		if _, ok := got.Inherited[0][k]; ok {
			t.Errorf("inherited row exposes %q", k)
		}
	}

	if rec := do(h, p1Admin, http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"editor"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1/members/m-p1", `{"role":"project_admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("re-role = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Admin, http.MethodDelete, "/api/v1/projects/p1/members/m-p1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body.String())
	}

	// Audit: one row per write, the target names the project.
	var targets []string
	for _, e := range fs.audit {
		if strings.HasPrefix(e.Action, "member.") {
			targets = append(targets, e.Action+" "+e.Target)
		}
	}
	want := []string{
		"member.remove user u2 · project p1",
		"member.role_change project_admin · user u2 · project p1",
		"member.add editor · user u3 · project p1",
	}
	if strings.Join(targets, "|") != strings.Join(want, "|") {
		t.Fatalf("audit = %q, want %q", targets, want)
	}
}

func TestProjectMemberEndpointsStayInsideTheProject(t *testing.T) {
	h := newHandler(projectMembersStore())
	// Memberships that are not project rows of p1 are not addressable through p1: 404.
	for _, id := range []string{"m-org", "m-p2", "m-p3", "m-none"} {
		if rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1/members/"+id, `{"role":"viewer"}`); rec.Code != http.StatusNotFound {
			t.Errorf("patch %s via p1 = %d, want 404", id, rec.Code)
		}
		if rec := do(h, p1Admin, http.MethodDelete, "/api/v1/projects/p1/members/"+id, ""); rec.Code != http.StatusNotFound {
			t.Errorf("delete %s via p1 = %d, want 404", id, rec.Code)
		}
	}
	// A project of another org, and an unknown project: 404.
	for _, path := range []string{"/api/v1/projects/p3/members", "/api/v1/projects/nope/members"} {
		if rec := do(h, p1Admin, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	// Another project of the same org: p1's admin does not see p2 → 404.
	if rec := do(h, p1Admin, http.MethodGet, "/api/v1/projects/p2/members", ""); rec.Code != http.StatusNotFound {
		t.Errorf("p1 admin on p2 = %d, want 404", rec.Code)
	}
	// An org-wide viewer SEES p2 but cannot manage it → 403; so does p2's own admin succeed.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p2/members", ""); rec.Code != http.StatusForbidden {
		t.Errorf("org viewer on p2 = %d, want 403", rec.Code)
	}
	if rec := do(h, p2Admin, http.MethodGet, "/api/v1/projects/p2/members", ""); rec.Code != http.StatusOK {
		t.Errorf("p2 admin on p2 = %d, want 200", rec.Code)
	}
}

func TestProjectMemberEndpointsRefuseBeforeLoadingAMembership(t *testing.T) {
	h := newHandler(projectMembersStore())
	// An editor of p1 gets the same 403 for an existing and an unknown membership id: the project
	// gate runs before any membership is loaded, so ids are not probeable.
	for _, id := range []string{"m-p1", "m-none"} {
		if rec := do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/members/"+id, `{"role":"viewer"}`); rec.Code != http.StatusForbidden {
			t.Errorf("editor patch %s = %d, want 403", id, rec.Code)
		}
		if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/members/"+id, ""); rec.Code != http.StatusForbidden {
			t.Errorf("editor delete %s = %d, want 403", id, rec.Code)
		}
	}
	for _, p := range []authz.Principal{p1Editor, p1Viewer} {
		if rec := do(h, p, http.MethodGet, "/api/v1/projects/p1/members", ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s list = %d, want 403", p.UserID, rec.Code)
		}
		if rec := do(h, p, http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"viewer"}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s add = %d, want 403", p.UserID, rec.Code)
		}
	}
}

func TestProjectMemberEndpointsRefuseMachinePrincipals(t *testing.T) {
	h := newHandler(projectMembersStore())
	for name, p := range map[string]authz.Principal{"token": p1Token, "token with project:manage": p1TokenAllowed, "client-credentials": p1JWT} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/projects/p1/members", ""},
			{http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"viewer"}`},
			{http.MethodPatch, "/api/v1/projects/p1/members/m-p1", `{"role":"editor"}`},
			{http.MethodDelete, "/api/v1/projects/p1/members/m-p1", ""},
			// The same answer for a project the token cannot see: nothing is disclosed.
			{http.MethodGet, "/api/v1/projects/p3/members", ""},
		} {
			if rec := do(h, p, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403", name, c.method, c.path, rec.Code)
			}
		}
	}
}

func TestProjectMemberWritesValidateTheirBody(t *testing.T) {
	h := newHandler(projectMembersStore())
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"org_admin"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"boss"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"viewer","project_id":"p2"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"viewer","org_id":"o2"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/p1/members", `{"email":"ghost@x","role":"viewer"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/p1/members", `{"user_id":"ghost","role":"viewer"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/projects/p1/members", `{"user_id":"u2","role":"editor"}`, http.StatusConflict},
		{http.MethodPatch, "/api/v1/projects/p1/members/m-p1", `{"role":"org_admin"}`, http.StatusBadRequest},
		{http.MethodPatch, "/api/v1/projects/p1/members/m-p1", `{"role":"boss"}`, http.StatusBadRequest},
	} {
		if rec := do(h, p1Admin, c.method, c.path, c.body); rec.Code != c.want {
			t.Errorf("%s %s %s = %d, want %d (%s)", c.method, c.path, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
}

func TestOrgAdminAndGlobalAdminManageProjectMembersToo(t *testing.T) {
	for _, p := range []authz.Principal{o1Admin, globalAdmin} {
		h := newHandler(projectMembersStore())
		if rec := do(h, p, http.MethodGet, "/api/v1/projects/p1/members", ""); rec.Code != http.StatusOK {
			t.Errorf("%s list = %d", p.UserID, rec.Code)
		}
		if rec := do(h, p, http.MethodPatch, "/api/v1/projects/p1/members/m-p1", `{"role":"editor"}`); rec.Code != http.StatusOK {
			t.Errorf("%s patch = %d", p.UserID, rec.Code)
		}
	}
}

func TestProjectAdminStillCannotUseTheOrgMemberEndpoints(t *testing.T) {
	h := newHandler(projectMembersStore())
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/organizations/o1/members", ""},
		{http.MethodPost, "/api/v1/organizations/o1/members", `{"email":"u3@x","role":"viewer"}`},
		{http.MethodPatch, "/api/v1/organizations/o1/members/m-p1", `{"role":"editor"}`},
		{http.MethodDelete, "/api/v1/organizations/o1/members/m-p1", ""},
	} {
		if rec := do(h, p1Admin, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("project admin %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

// A membership removed between the gate and the write is a 404 — on the new endpoints and,
// fixed in this iteration, on the org endpoints that used to answer 500.
func TestAVanishedMembershipIsNotFoundNotAServerError(t *testing.T) {
	fs := projectMembersStore()
	fs.membershipVanishesOnWrite = true
	h := newHandler(fs)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/projects/p1/members/m-p1", `{"role":"editor"}`},
		{http.MethodDelete, "/api/v1/projects/p1/members/m-p1", ""},
		{http.MethodPatch, "/api/v1/organizations/o1/members/m-org", `{"role":"viewer"}`},
		{http.MethodDelete, "/api/v1/organizations/o1/members/m-org", ""},
	} {
		if rec := do(h, o1Admin, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	// The project vanished between the gate and the INSERT.
	fs.membershipVanishesOnWrite = false
	fs.projectVanishesOnCreate = true
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/members", `{"email":"u3@x","role":"viewer"}`); rec.Code != http.StatusNotFound {
		t.Errorf("create on a vanished project = %d, want 404", rec.Code)
	}
}

// The org path writes the same audit target shape for a project-scoped membership.
func TestOrgMemberAuditNamesTheProject(t *testing.T) {
	fs := projectMembersStore()
	h := newHandler(fs)
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"user_id":"u3","project_id":"p1","role":"editor"}`); rec.Code != http.StatusCreated {
		t.Fatalf("org add with project = %d %s", rec.Code, rec.Body.String())
	}
	if got := fs.audit[0].Target; got != "editor · user u3 · project p1" {
		t.Fatalf("org add target = %q", got)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/organizations/o1/members/m-p2", `{"role":"editor"}`); rec.Code != http.StatusOK {
		t.Fatalf("org patch = %d", rec.Code)
	}
	if got := fs.audit[0].Target; got != "editor · user u3 · project p2" {
		t.Fatalf("org role_change target = %q", got)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/organizations/o1/members/m-p2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("org delete = %d", rec.Code)
	}
	if got := fs.audit[0].Target; got != "user u3 · project p2" {
		t.Fatalf("org remove target = %q", got)
	}
}
