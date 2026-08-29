package authz

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestGlobalAdminCanDoAnything(t *testing.T) {
	p := Principal{UserID: "u", IsGlobalAdmin: true}
	for _, a := range []Action{ActionGlobalManage, ActionOrgManage, ActionProjectManage, ActionProjectWrite, ActionOrgRead} {
		if !p.Can(a, "org-x", "proj-y") {
			t.Errorf("global admin should be allowed %q", a)
		}
	}
	// Global admin sees orgs even without membership.
	if !p.VisibleOrg("any-org") {
		t.Error("global admin should see any org")
	}
}

func TestGlobalManageRequiresGlobalAdmin(t *testing.T) {
	p := Principal{UserID: "u", Memberships: []domain.Membership{
		{OrgID: "o1", Role: domain.RoleOrgAdmin},
	}}
	if p.Can(ActionGlobalManage, "o1", "") {
		t.Error("org admin must not have global manage")
	}
}

func TestOrgAdminScope(t *testing.T) {
	p := Principal{UserID: "u", Memberships: []domain.Membership{
		{OrgID: "o1", Role: domain.RoleOrgAdmin},
	}}
	// Full control within o1, including all its projects.
	if !p.Can(ActionOrgManage, "o1", "") {
		t.Error("org admin should manage o1")
	}
	if !p.Can(ActionProjectManage, "o1", "any-project") {
		t.Error("org admin should manage any project in o1")
	}
	// No reach into another org.
	if p.Can(ActionOrgRead, "o2", "") {
		t.Error("org admin of o1 must not read o2")
	}
	if p.VisibleProject("o2", "p2") {
		t.Error("org admin of o1 must not see o2 projects")
	}
}

func TestProjectAdminScope(t *testing.T) {
	p := Principal{UserID: "u", Memberships: []domain.Membership{
		{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin},
	}}
	if !p.Can(ActionProjectManage, "o1", "p1") {
		t.Error("project admin should manage p1")
	}
	if !p.Can(ActionProjectWrite, "o1", "p1") {
		t.Error("project admin should write p1")
	}
	// Not another project in the same org.
	if p.Can(ActionProjectRead, "o1", "p2") {
		t.Error("project admin of p1 must not read p2")
	}
	// Cannot manage the org itself.
	if p.Can(ActionOrgManage, "o1", "") {
		t.Error("project admin must not manage the org")
	}
}

func TestEditorAndViewer(t *testing.T) {
	editor := Principal{UserID: "e", Memberships: []domain.Membership{
		{OrgID: "o1", Role: domain.RoleEditor},
	}}
	if !editor.Can(ActionProjectWrite, "o1", "p1") {
		t.Error("org editor should write any project in o1")
	}
	if editor.Can(ActionProjectManage, "o1", "p1") {
		t.Error("editor must not manage members/settings")
	}

	viewer := Principal{UserID: "v", Memberships: []domain.Membership{
		{OrgID: "o1", ProjectID: "p1", Role: domain.RoleViewer},
	}}
	if !viewer.Can(ActionProjectRead, "o1", "p1") {
		t.Error("viewer should read its project")
	}
	if viewer.Can(ActionProjectWrite, "o1", "p1") {
		t.Error("viewer must not write")
	}
	if viewer.Can(ActionProjectRead, "o1", "p2") {
		t.Error("project viewer must not read another project")
	}
}

func TestInOrgAndVisibleOrg(t *testing.T) {
	p := Principal{Memberships: []domain.Membership{
		{OrgID: "o1", ProjectID: "p1", Role: domain.RoleViewer},
	}}
	if !p.InOrg("o1") || !p.VisibleOrg("o1") {
		t.Error("project-scoped member should be in/visible o1")
	}
	if p.InOrg("o2") {
		t.Error("must not be in o2")
	}
	ga := Principal{IsGlobalAdmin: true}
	if !ga.InOrg("any") || !ga.VisibleOrg("any") {
		t.Error("global admin is in/visible any org")
	}
}

func TestNoMembershipNoAccess(t *testing.T) {
	p := Principal{UserID: "u"}
	if p.Can(ActionOrgRead, "o1", "") || p.Can(ActionProjectRead, "o1", "p1") {
		t.Error("user without membership must have no access")
	}
	if p.Can(ActionOrgRead, "", "") {
		t.Error("empty org target must be denied")
	}
}

func TestAuditUserID(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want string
	}{
		{"human uuid", Principal{UserID: "user-uuid"}, "user-uuid"},
		{"oidc client credentials keeps jit user", Principal{UserID: "cc-user-uuid", ViaToken: true}, "cc-user-uuid"},
		{"cerbix api token is null actor", Principal{UserID: SyntheticTokenActorPrefix + "token-id", ViaToken: true}, ""},
		{"prefix alone is not trusted", Principal{UserID: SyntheticTokenActorPrefix + "not-a-token", ViaToken: false}, SyntheticTokenActorPrefix + "not-a-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.AuditUserID(); got != tc.want {
				t.Fatalf("AuditUserID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// FR-024 D12: the three gate actions map onto the existing project roles — viewer+ evaluates
// and reads, editor+ writes the policy, project_admin+ overrides — and a global admin holds
// all of them as the existing rule implies. Every role × action cell is asserted, in both
// directions, so a grant added or dropped by accident fails by name.
func TestGateActionsByRole(t *testing.T) {
	member := func(role domain.Role) Principal {
		return Principal{UserID: "u", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: role}}}
	}
	cases := []struct {
		name string
		p    Principal
		want map[Action]bool
	}{
		{"viewer", member(domain.RoleViewer), map[Action]bool{
			ActionGateEvaluate: true, ActionGatePolicyWrite: false, ActionGateOverride: false}},
		{"editor", member(domain.RoleEditor), map[Action]bool{
			ActionGateEvaluate: true, ActionGatePolicyWrite: true, ActionGateOverride: false}},
		{"project_admin", member(domain.RoleProjectAdmin), map[Action]bool{
			ActionGateEvaluate: true, ActionGatePolicyWrite: true, ActionGateOverride: true}},
		{"org_admin", Principal{UserID: "u", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}}, map[Action]bool{
			ActionGateEvaluate: true, ActionGatePolicyWrite: true, ActionGateOverride: true}},
		{"global_admin", Principal{UserID: "u", IsGlobalAdmin: true}, map[Action]bool{
			ActionGateEvaluate: true, ActionGatePolicyWrite: true, ActionGateOverride: true}},
		{"no membership", Principal{UserID: "u"}, map[Action]bool{
			ActionGateEvaluate: false, ActionGatePolicyWrite: false, ActionGateOverride: false}},
	}
	for _, tc := range cases {
		for action, want := range tc.want {
			if got := tc.p.Can(action, "o1", "p1"); got != want {
				t.Errorf("%s Can(%s) = %v, want %v", tc.name, action, got, want)
			}
		}
	}
	// A project-scoped grant never reaches a sibling project, whatever the action.
	admin := member(domain.RoleProjectAdmin)
	for _, a := range []Action{ActionGateEvaluate, ActionGatePolicyWrite, ActionGateOverride} {
		if admin.Can(a, "o1", "p2") {
			t.Errorf("project admin of p1 must not hold %s on p2", a)
		}
	}
}

// The gate actions are distinct from the project actions they are mapped beside: holding
// project:write does not by itself name gate:override, so a future re-mapping has to be made
// here, in the grants table, and nowhere else.
func TestGateActionsAreNotAliases(t *testing.T) {
	editor := Principal{UserID: "e", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleEditor}}}
	if !editor.Can(ActionProjectWrite, "o1", "p1") || editor.Can(ActionGateOverride, "o1", "p1") {
		t.Fatal("an editor writes the project but does not override the gate")
	}
	if ActionGateEvaluate != "gate:evaluate" || ActionGatePolicyWrite != "gate:policy:write" || ActionGateOverride != "gate:override" {
		t.Fatal("the gate action names are the spec's (D12)")
	}
}
