package authz

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D12 (invariants 16, 17): `change:record` is a central action granted editor and above;
// a token's `actions` allow-list is INTERSECTED with the role inside the ONE predicate — Can and
// its query-scope mirror VisibleScope — and nowhere else; a nil list leaves every existing
// principal's authority unchanged.

func TestChangeRecordIsEditorAndAbove(t *testing.T) {
	for _, tc := range []struct {
		role domain.Role
		want bool
	}{
		{domain.RoleOrgAdmin, true},
		{domain.RoleProjectAdmin, true},
		{domain.RoleEditor, true},
		{domain.RoleViewer, false},
	} {
		p := Principal{UserID: "u", Memberships: []domain.Membership{{OrgID: "o1", Role: tc.role}}}
		if got := p.Can(ActionChangeRecord, "o1", "p1"); got != tc.want {
			t.Errorf("%s Can(change:record) = %v, want %v", tc.role, got, tc.want)
		}
	}
	if !ValidAction(ActionChangeRecord) || ValidAction("change:read") || ValidAction("not:an:action") {
		t.Fatal("the catalogue names change:record and no change:read (reads are project:read)")
	}
	if len(Actions()) != 10 {
		t.Fatalf("the catalogue has %d actions; the six core, three gate and change:record make ten", len(Actions()))
	}
}

// The CI token of D12: role editor, actions [gate:evaluate, change:record]. It can ask the gate
// and record changes and can do NOTHING else — not even project:read — while its role alone
// would grant project:read, project:write and gate:policy:write.
func TestTokenAllowListIntersectsTheRoleInsideCan(t *testing.T) {
	ci := Principal{
		UserID:      SyntheticTokenActorPrefix + "tk1",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
		ViaToken:    true,
		Actions:     []Action{ActionGateEvaluate, ActionChangeRecord},
	}
	for _, tc := range []struct {
		action Action
		want   bool
	}{
		{ActionGateEvaluate, true},
		{ActionChangeRecord, true},
		{ActionProjectRead, false},
		{ActionProjectWrite, false},
		{ActionGatePolicyWrite, false},
		{ActionOrgRead, false},
	} {
		if got := ci.Can(tc.action, "o1", "p1"); got != tc.want {
			t.Errorf("CI token Can(%s) = %v, want %v", tc.action, got, tc.want)
		}
	}
	// The list never widens the role: a viewer token listing change:record still cannot record.
	viewer := Principal{Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleViewer}}, ViaToken: true,
		Actions: []Action{ActionChangeRecord}}
	if viewer.Can(ActionChangeRecord, "o1", "p1") {
		t.Fatal("an allow-list widened a viewer to change:record; it must only narrow")
	}
	// The list bounds even a global admin principal: the intersection is unconditional.
	admin := Principal{IsGlobalAdmin: true, Actions: []Action{ActionGateEvaluate}}
	if admin.Can(ActionProjectRead, "o1", "p1") || !admin.Can(ActionGateEvaluate, "o1", "p1") {
		t.Fatal("the allow-list must bound every principal that carries one")
	}
	// An EMPTY list is a list: it grants nothing.
	none := Principal{Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleEditor}}, Actions: []Action{}}
	if none.Can(ActionProjectRead, "o1", "p1") {
		t.Fatal("an empty allow-list must grant nothing (nil, not empty, means unrestricted)")
	}
	// VisibleScope mirrors Can: the CI token sees no project under project:read, every project
	// of its grant under gate:evaluate.
	if all, orgs, projects := ci.VisibleScope(ActionProjectRead); all || len(orgs) != 0 || len(projects) != 0 {
		t.Fatalf("CI token VisibleScope(project:read) = %v %v %v, want nothing", all, orgs, projects)
	}
	if _, _, projects := ci.VisibleScope(ActionGateEvaluate); len(projects) != 1 || projects[0] != "p1" {
		t.Fatalf("CI token VisibleScope(gate:evaluate) projects = %v, want [p1]", projects)
	}
	// 404-vs-403 visibility is membership, not action: the project is still visible through
	// InOrg, so a refused read is a 403 and not a 404.
	if !ci.InOrg("o1") {
		t.Fatal("membership visibility must not depend on the allow-list")
	}
}

// A NULL list leaves every existing principal exactly as it was: the same editor token without
// a list keeps the whole editor grant, and VisibleScope is unchanged.
func TestNilAllowListLeavesAuthorityUnchanged(t *testing.T) {
	editor := Principal{Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}, ViaToken: true}
	for _, a := range []Action{ActionProjectRead, ActionProjectWrite, ActionGateEvaluate, ActionGatePolicyWrite, ActionChangeRecord, ActionOrgRead} {
		if !editor.Can(a, "o1", "p1") {
			t.Errorf("editor without a list lost %s", a)
		}
	}
	for _, a := range []Action{ActionProjectManage, ActionGateOverride, ActionGlobalManage} {
		if editor.Can(a, "o1", "p1") {
			t.Errorf("editor without a list gained %s", a)
		}
	}
	if _, _, projects := editor.VisibleScope(ActionProjectRead); len(projects) != 1 {
		t.Fatalf("VisibleScope(project:read) = %v, want [p1]", projects)
	}
}
