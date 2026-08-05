package domain

import "testing"

func TestRoleValid(t *testing.T) {
	valid := []Role{RoleOrgAdmin, RoleProjectAdmin, RoleEditor, RoleViewer}
	for _, r := range valid {
		if !r.Valid() {
			t.Errorf("role %q should be valid", r)
		}
	}
	if Role("superuser").Valid() {
		t.Error("unknown role should be invalid")
	}
}

func TestRoleValidForScope(t *testing.T) {
	cases := []struct {
		role  Role
		scope Scope
		want  bool
	}{
		{RoleOrgAdmin, ScopeOrg, true},
		{RoleOrgAdmin, ScopeProject, false},
		{RoleProjectAdmin, ScopeProject, true},
		{RoleProjectAdmin, ScopeOrg, false},
		{RoleEditor, ScopeOrg, true},
		{RoleEditor, ScopeProject, true},
		{RoleViewer, ScopeOrg, true},
		{RoleViewer, ScopeProject, true},
		{RoleViewer, Scope("bogus"), false},
	}
	for _, c := range cases {
		if got := c.role.ValidForScope(c.scope); got != c.want {
			t.Errorf("%q.ValidForScope(%q) = %v, want %v", c.role, c.scope, got, c.want)
		}
	}
}

func TestMembershipScope(t *testing.T) {
	if (Membership{}).Scope() != ScopeOrg {
		t.Error("empty ProjectID should be org scope")
	}
	if (Membership{ProjectID: "p1"}).Scope() != ScopeProject {
		t.Error("set ProjectID should be project scope")
	}
}

func TestMembershipValidate(t *testing.T) {
	ok := Membership{UserID: "u", OrgID: "o", Role: RoleOrgAdmin}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid org membership rejected: %v", err)
	}
	okProj := Membership{UserID: "u", OrgID: "o", ProjectID: "p", Role: RoleProjectAdmin}
	if err := okProj.Validate(); err != nil {
		t.Fatalf("valid project membership rejected: %v", err)
	}

	bad := []Membership{
		{OrgID: "o", Role: RoleViewer},                                // no user
		{UserID: "u", Role: RoleViewer},                               // no org
		{UserID: "u", OrgID: "o", Role: Role("x")},                    // unknown role
		{UserID: "u", OrgID: "o", Role: RoleProjectAdmin},             // project role at org scope
		{UserID: "u", OrgID: "o", ProjectID: "p", Role: RoleOrgAdmin}, // org role at project scope
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}
