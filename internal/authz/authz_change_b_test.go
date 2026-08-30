package authz

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D12 at its edges (invariant 17; iter-0165 task 2, Agent B): the allow-list is an
// INTERSECTION checked by the one predicate — Can and its query-scope mirror VisibleScope — for
// every principal that carries one, the global admin included, and an EMPTY list is a list.

// An empty (non-nil) list grants nothing to anybody — not org:read, not project:read, not even
// global:manage to a global admin — while membership visibility (InOrg, VisibleOrg, and the
// 404-versus-403 predicate VisibleProject — D-0212 item 2) is untouched, so a refused read is a 403
// and not a 404; VisibleScope, which mirrors Can, sees nothing.
func TestEmptyAllowListGrantsNothingIncludingOrgReadWhileMembershipVisibilityStays(t *testing.T) {
	for _, p := range []Principal{
		{UserID: "org-admin", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}, Actions: []Action{}},
		{UserID: "project-admin", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}, Actions: []Action{}},
		{UserID: "global-admin", IsGlobalAdmin: true, Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleViewer}}, Actions: []Action{}},
	} {
		for _, a := range Actions() {
			if p.Can(a, "o1", "p1") || p.Can(a, "o1", "") || p.Can(a, "", "") {
				t.Errorf("%s with an empty list Can(%s)", p.UserID, a)
			}
			if all, orgs, projects := p.VisibleScope(a); all || orgs != nil || projects != nil {
				t.Errorf("%s with an empty list VisibleScope(%s) = %v %v %v", p.UserID, a, all, orgs, projects)
			}
		}
		if !p.VisibleProject("o1", "p1") {
			t.Errorf("%s with an empty list lost project VISIBILITY — that is membership, not the allow-list (D-0212)", p.UserID)
		}
		if !p.InOrg("o1") || !p.VisibleOrg("o1") {
			t.Errorf("%s: membership visibility depends on the allow-list", p.UserID)
		}
	}
}

// A global admin WITH a list is bounded by it before the admin shortcut: Can and VisibleScope
// answer only the listed actions (VisibleScope still says allOrgs for those), global:manage is
// refused unless listed, and a nil list leaves the admin unbounded.
func TestGlobalAdminWithAnAllowListIsBoundedByItInCanAndVisibleScope(t *testing.T) {
	admin := Principal{UserID: "root", IsGlobalAdmin: true, Actions: []Action{ActionGateEvaluate, ActionChangeRecord}}
	for _, tc := range []struct {
		action Action
		want   bool
	}{
		{ActionGateEvaluate, true}, {ActionChangeRecord, true},
		{ActionGlobalManage, false}, {ActionOrgRead, false}, {ActionProjectRead, false}, {ActionProjectWrite, false}, {ActionGateOverride, false},
	} {
		if got := admin.Can(tc.action, "o1", "p1"); got != tc.want {
			t.Errorf("admin with a list Can(%s) = %v, want %v", tc.action, got, tc.want)
		}
		if got := admin.Can(tc.action, "", ""); got != tc.want {
			t.Errorf("admin with a list Can(%s) on no target = %v, want %v", tc.action, got, tc.want)
		}
		all, orgs, projects := admin.VisibleScope(tc.action)
		if all != tc.want || orgs != nil || projects != nil {
			t.Errorf("admin with a list VisibleScope(%s) = %v %v %v, want allOrgs=%v", tc.action, all, orgs, projects, tc.want)
		}
	}
	// The list names global:manage: an admin may then manage — the intersection with "everything".
	root := Principal{IsGlobalAdmin: true, Actions: []Action{ActionGlobalManage}}
	if !root.Can(ActionGlobalManage, "", "") || root.Can(ActionProjectRead, "o1", "p1") {
		t.Fatal("a listed global:manage must pass and nothing else")
	}
	// A non-admin listing global:manage still cannot: the role half of the intersection fails.
	member := Principal{Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}, Actions: []Action{ActionGlobalManage}}
	if member.Can(ActionGlobalManage, "o1", "") {
		t.Fatal("an allow-list gave an org admin global:manage")
	}
	// nil: unbounded, as today.
	plain := Principal{IsGlobalAdmin: true}
	if !plain.Can(ActionGlobalManage, "", "") || !plain.Can(ActionProjectWrite, "o1", "p1") {
		t.Fatal("a global admin without a list lost authority")
	}
	if all, _, _ := plain.VisibleScope(ActionProjectRead); !all {
		t.Fatal("a global admin without a list lost allOrgs")
	}
}

// Can(action) = roleGrants[role] ∋ action AND action ∈ list — checked over every org-scoped role,
// several lists, and every action of the catalogue, with VisibleScope mirroring Can. The list
// can only narrow: no (role, list) pair grants an action the role lacks. A project-scoped grant
// under a list holds exactly the listed actions on its own project and nothing elsewhere.
func TestAllowListIsAnIntersectionOfRoleAndListNeverAUnion(t *testing.T) {
	lists := [][]Action{
		{ActionGateEvaluate, ActionChangeRecord},
		{ActionChangeRecord},
		{ActionGateOverride, ActionProjectRead},
		{ActionOrgManage, ActionGlobalManage},
		Actions(),
	}
	for _, role := range []domain.Role{domain.RoleOrgAdmin, domain.RoleEditor, domain.RoleViewer} {
		bare := Principal{Memberships: []domain.Membership{{OrgID: "o1", Role: role}}}
		for _, list := range lists {
			listed := Principal{Memberships: bare.Memberships, Actions: list}
			for _, a := range Actions() {
				inList := false
				for _, l := range list {
					inList = inList || l == a
				}
				want := bare.Can(a, "o1", "p1") && inList
				if got := listed.Can(a, "o1", "p1"); got != want {
					t.Errorf("%s with %v Can(%s) = %v, want %v", role, list, a, got, want)
				}
				_, orgs, projects := listed.VisibleScope(a)
				if (len(orgs) == 1 && orgs[0] == "o1") != want || projects != nil {
					t.Errorf("%s with %v VisibleScope(%s) = %v %v, want org o1 iff %v", role, list, a, orgs, projects, want)
				}
			}
		}
	}
	pa := Principal{Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}},
		Actions: []Action{ActionGateOverride, ActionProjectRead}}
	if !pa.Can(ActionGateOverride, "o1", "p1") || !pa.Can(ActionProjectRead, "o1", "p1") || pa.Can(ActionProjectWrite, "o1", "p1") ||
		pa.Can(ActionGateOverride, "o1", "p2") || pa.Can(ActionProjectRead, "o1", "") {
		t.Fatal("project-scoped intersection is wrong")
	}
	if _, orgs, projects := pa.VisibleScope(ActionGateOverride); orgs != nil || len(projects) != 1 || projects[0] != "p1" {
		t.Fatalf("project-scoped VisibleScope = %v %v", orgs, projects)
	}
}

// Duplicates and unknown entries in a list never widen authority: a duplicated action is that
// action once, an entry naming no catalogue action grants nothing, and the list bounds a
// principal with several memberships uniformly.
func TestAllowListDuplicatesAndUnknownEntriesNeverWidenAuthority(t *testing.T) {
	p := Principal{
		Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleViewer}, {OrgID: "o2", ProjectID: "p2", Role: domain.RoleEditor}},
		Actions:     []Action{ActionProjectRead, ActionProjectRead, Action("bogus:action"), Action("change:read")},
	}
	if !p.Can(ActionProjectRead, "o1", "p1") || !p.Can(ActionProjectRead, "o2", "p2") {
		t.Fatal("a duplicated listed action was refused")
	}
	for _, a := range []Action{Action("bogus:action"), Action("change:read"), ActionProjectWrite, ActionChangeRecord, ActionOrgRead} {
		if p.Can(a, "o1", "p1") || p.Can(a, "o2", "p2") {
			t.Errorf("Can(%s) through a duplicate/unknown-laden list", a)
		}
	}
	if _, orgs, projects := p.VisibleScope(ActionProjectRead); len(orgs) != 1 || orgs[0] != "o1" || len(projects) != 1 || projects[0] != "p2" {
		t.Fatalf("VisibleScope(project:read) = %v %v", orgs, projects)
	}
	if _, orgs, projects := p.VisibleScope(ActionProjectWrite); orgs != nil || projects != nil {
		t.Fatalf("VisibleScope(project:write) = %v %v, want nothing (editor grants it, the list omits it)", orgs, projects)
	}
}
