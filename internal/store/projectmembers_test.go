package store_test

import (
	"errors"
	"sort"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// iter-0206 (FR-040, func-project-members.md): the project-scoped member queries are the guard
// that keeps a project admin inside their project. The API test fake does not filter by project,
// so THESE tests — on a real database, with two organizations of two projects each — are what
// hold the SQL scope.
type projectMembersWorld struct {
	acme, globex           domain.Organization
	api, web, gFiles, gOps domain.Project
	alice, bob, carol, dan domain.User
	// memberships
	aliceOrgAdmin, bobAPIEditor, carolWebViewer, danGlobexAPI domain.Membership
}

func newProjectMembersWorld(t *testing.T, st *store.Store) projectMembersWorld {
	t.Helper()
	ctx := t.Context()
	var w projectMembersWorld
	var err error
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatalf("setup: %v", e)
		}
	}
	w.acme, err = st.CreateOrganization(ctx, "acme", "Acme")
	must(err)
	w.globex, err = st.CreateOrganization(ctx, "globex", "Globex")
	must(err)
	w.api, err = st.CreateProject(ctx, w.acme.ID, "api", "API")
	must(err)
	w.web, err = st.CreateProject(ctx, w.acme.ID, "web", "Web")
	must(err)
	w.gFiles, err = st.CreateProject(ctx, w.globex.ID, "files", "Files")
	must(err)
	w.gOps, err = st.CreateProject(ctx, w.globex.ID, "ops", "Ops")
	must(err)
	w.alice, err = st.CreateLocalUser(ctx, "alice@example.com", "Alice", "h", false)
	must(err)
	w.bob, err = st.CreateLocalUser(ctx, "bob@example.com", "Bob", "h", false)
	must(err)
	w.carol, err = st.CreateLocalUser(ctx, "carol@example.com", "Carol", "h", false)
	must(err)
	w.dan, err = st.CreateLocalUser(ctx, "dan@example.com", "Dan", "h", false)
	must(err)
	w.aliceOrgAdmin, err = st.CreateMembership(ctx, domain.Membership{UserID: w.alice.ID, OrgID: w.acme.ID, Role: domain.RoleOrgAdmin})
	must(err)
	w.bobAPIEditor, err = st.CreateMembership(ctx, domain.Membership{UserID: w.bob.ID, OrgID: w.acme.ID, ProjectID: w.api.ID, Role: domain.RoleEditor})
	must(err)
	w.carolWebViewer, err = st.CreateMembership(ctx, domain.Membership{UserID: w.carol.ID, OrgID: w.acme.ID, ProjectID: w.web.ID, Role: domain.RoleViewer})
	must(err)
	w.danGlobexAPI, err = st.CreateMembership(ctx, domain.Membership{UserID: w.dan.ID, OrgID: w.globex.ID, ProjectID: w.gFiles.ID, Role: domain.RoleProjectAdmin})
	must(err)
	// An org-level membership in the OTHER org, which must never show up as inherited for acme.
	_, err = st.CreateMembership(ctx, domain.Membership{UserID: w.carol.ID, OrgID: w.globex.ID, Role: domain.RoleEditor})
	must(err)
	return w
}

func TestProjectMembersListsOnlyThisProjectsRows(t *testing.T) {
	st, ctx := testStore(t)
	w := newProjectMembersWorld(t, st)

	members, err := st.ListProjectMembers(ctx, w.api.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 1 || members[0].ID != w.bobAPIEditor.ID || members[0].Email != "bob@example.com" ||
		members[0].ProjectID != w.api.ID {
		t.Fatalf("api members = %+v, want only bob's project membership", members)
	}

	inherited, err := st.ListProjectInheritedMembers(ctx, w.api.ID)
	if err != nil {
		t.Fatalf("inherited: %v", err)
	}
	if len(inherited) != 1 || inherited[0].Email != "alice@example.com" || inherited[0].Role != domain.RoleOrgAdmin {
		t.Fatalf("api inherited = %+v, want only acme's org-level alice", inherited)
	}

	// Globex's project sees its own project member and globex's org-level carol, nothing of acme.
	gm, _ := st.ListProjectMembers(ctx, w.gFiles.ID)
	gi, _ := st.ListProjectInheritedMembers(ctx, w.gFiles.ID)
	if len(gm) != 1 || gm[0].ID != w.danGlobexAPI.ID {
		t.Fatalf("globex files members = %+v", gm)
	}
	if len(gi) != 1 || gi[0].Email != "carol@example.com" || gi[0].Role != domain.RoleEditor {
		t.Fatalf("globex files inherited = %+v", gi)
	}
}

func TestProjectMembershipWritesAreScopedToTheProject(t *testing.T) {
	st, ctx := testStore(t)
	w := newProjectMembersWorld(t, st)

	// Through the api project, none of these can be touched: an org-level row, another project's
	// row in the same org, another org's row, an unknown id.
	for name, id := range map[string]string{
		"org-level":     w.aliceOrgAdmin.ID,
		"other project": w.carolWebViewer.ID,
		"other org":     w.danGlobexAPI.ID,
		"unknown":       "00000000-0000-0000-0000-000000000000",
	} {
		if _, err := st.UpdateProjectMembershipRole(ctx, w.api.ID, id, domain.RoleViewer); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("update %s through api: err = %v, want ErrNotFound", name, err)
		}
		if _, err := st.DeleteProjectMembership(ctx, w.api.ID, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete %s through api: err = %v, want ErrNotFound", name, err)
		}
	}
	// None of them changed.
	all, _ := st.ListMembershipsByOrg(ctx, w.acme.ID)
	gall, _ := st.ListMembershipsByOrg(ctx, w.globex.ID)
	roles := map[string]domain.Role{}
	for _, m := range append(all, gall...) {
		roles[m.ID] = m.Role
	}
	if roles[w.aliceOrgAdmin.ID] != domain.RoleOrgAdmin || roles[w.carolWebViewer.ID] != domain.RoleViewer ||
		roles[w.danGlobexAPI.ID] != domain.RoleProjectAdmin {
		t.Fatalf("a scoped write touched a row outside the project: %v", roles)
	}

	// Not a project role, even on its own row.
	if _, err := st.UpdateProjectMembershipRole(ctx, w.api.ID, w.bobAPIEditor.ID, domain.RoleOrgAdmin); err == nil {
		t.Fatalf("org_admin on a project row was accepted")
	}
	// Its own row: update and delete work, and return the row for the audit.
	up, err := st.UpdateProjectMembershipRole(ctx, w.api.ID, w.bobAPIEditor.ID, domain.RoleProjectAdmin)
	if err != nil || up.Role != domain.RoleProjectAdmin || up.UserID != w.bob.ID {
		t.Fatalf("update own row = %+v, %v", up, err)
	}
	gone, err := st.DeleteProjectMembership(ctx, w.api.ID, w.bobAPIEditor.ID)
	if err != nil || gone.UserID != w.bob.ID {
		t.Fatalf("delete own row = %+v, %v", gone, err)
	}
	if _, err := st.DeleteProjectMembership(ctx, w.api.ID, w.bobAPIEditor.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete again: err = %v, want ErrNotFound", err)
	}
}

func TestCreateProjectMembershipTellsItsFailuresApart(t *testing.T) {
	st, ctx := testStore(t)
	w := newProjectMembersWorld(t, st)

	m, err := st.CreateProjectMembership(ctx, w.api.ID, w.carol.ID, domain.RoleViewer)
	if err != nil || m.OrgID != w.acme.ID || m.ProjectID != w.api.ID || m.Role != domain.RoleViewer {
		t.Fatalf("create = %+v, %v; want carol viewer of acme/api", m, err)
	}
	// Already a member of this project.
	if _, err := st.CreateProjectMembership(ctx, w.api.ID, w.bob.ID, domain.RoleViewer); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: err = %v, want ErrConflict", err)
	}
	// A user with an ORG-level membership may still get a project role (two different rows).
	if _, err := st.CreateProjectMembership(ctx, w.api.ID, w.alice.ID, domain.RoleEditor); err != nil {
		t.Fatalf("org-level user gets a project row: %v", err)
	}
	// Unknown user.
	if _, err := st.CreateProjectMembership(ctx, w.api.ID, "00000000-0000-0000-0000-000000000000", domain.RoleViewer); !errors.Is(err, store.ErrUnknownUser) {
		t.Fatalf("unknown user: err = %v, want ErrUnknownUser", err)
	}
	// The project disappeared.
	if err := st.DeleteProject(ctx, w.acme.ID, w.web.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := st.CreateProjectMembership(ctx, w.web.ID, w.dan.ID, domain.RoleViewer); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("vanished project: err = %v, want ErrNotFound", err)
	}
	// Not a project role.
	if _, err := st.CreateProjectMembership(ctx, w.api.ID, w.dan.ID, domain.RoleOrgAdmin); err == nil {
		t.Fatalf("org_admin on a project was accepted")
	}

	members, _ := st.ListProjectMembers(ctx, w.api.ID)
	var emails []string
	for _, m := range members {
		emails = append(emails, m.Email)
	}
	sort.Strings(emails)
	if len(emails) != 3 || emails[0] != "alice@example.com" || emails[1] != "bob@example.com" || emails[2] != "carol@example.com" {
		t.Fatalf("api members after creates = %v", emails)
	}
}
