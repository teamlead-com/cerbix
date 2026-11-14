package authz

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-2 H-3 fixes 2–3: status_page:read is held by every role, org-level and
// project-level, and is checked through ANY membership in the organization — the dedicated
// predicate CanAnyScopeInOrg — because Can(action, org, "") skips project-scoped memberships and
// would refuse the project-only members the owner ruled may read the org's pages. A token holds
// it only when its allow-list names it.
func TestStatusPageReadThroughAnyMembershipOfTheOrg(t *testing.T) {
	for _, role := range []domain.Role{domain.RoleOrgAdmin, domain.RoleEditor, domain.RoleViewer} {
		p := Principal{Memberships: []domain.Membership{{OrgID: "o1", Role: role}}}
		if !p.CanAnyScopeInOrg(ActionStatusPageRead, "o1") || !p.Can(ActionStatusPageRead, "o1", "") {
			t.Errorf("org-level %s cannot read status pages", role)
		}
	}
	for _, role := range []domain.Role{domain.RoleProjectAdmin, domain.RoleEditor, domain.RoleViewer} {
		p := Principal{Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: role}}}
		if !p.CanAnyScopeInOrg(ActionStatusPageRead, "o1") {
			t.Errorf("project-only %s cannot read the org's status pages", role)
		}
		if p.Can(ActionStatusPageRead, "o1", "") {
			t.Errorf("project-only %s: Can(…, org, \"\") now honours project memberships — the predicate exists because it does not", role)
		}
		if p.CanAnyScopeInOrg(ActionStatusPageRead, "o2") || p.CanAnyScopeInOrg(ActionStatusPageRead, "") {
			t.Errorf("project-only %s reads another org's pages", role)
		}
		// The predicate is not a back door to org-level actions the role does not grant.
		if p.CanAnyScopeInOrg(ActionOrgManage, "o1") {
			t.Errorf("project-only %s holds org:manage through the any-membership predicate", role)
		}
	}

	member := []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}
	cases := []struct {
		name    string
		actions []Action
		want    bool
	}{
		{"no list (role decides)", nil, true},
		{"list naming it", []Action{ActionStatusPageRead}, true},
		{"list without it", []Action{ActionProjectRead, ActionOrgRead}, false},
		{"empty list", []Action{}, false},
	}
	for _, c := range cases {
		tok := Principal{UserID: SyntheticTokenActorPrefix + "t", ViaToken: true, Memberships: member, Actions: c.actions}
		if got := tok.CanAnyScopeInOrg(ActionStatusPageRead, "o1"); got != c.want {
			t.Errorf("token, %s: CanAnyScopeInOrg = %v, want %v", c.name, got, c.want)
		}
		if got := tok.AllowListed(ActionStatusPageRead); got != c.want {
			t.Errorf("token, %s: AllowListed = %v, want %v", c.name, got, c.want)
		}
	}
	admin := Principal{IsGlobalAdmin: true}
	if !admin.CanAnyScopeInOrg(ActionStatusPageRead, "any") {
		t.Error("global admin cannot read an org's status pages")
	}
	if !ValidAction(ActionStatusPageRead) {
		t.Error("status_page:read is not in the catalogue — a token could not list it")
	}
}
