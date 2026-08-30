package authz

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D12: visibility is membership, not action. The CI token `role: editor, actions:
// [gate:evaluate, change:record]` cannot read its project (Can(project:read) is false — 403) but
// still SEES it (VisibleProject is true — never 404), while a token of another org sees nothing.
// VisibleScope keeps intersecting the list (a token whose list omits the action lists nothing).
func TestVisibilityIsMembershipNotTheAllowList(t *testing.T) {
	ci := Principal{ViaToken: true,
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
		Actions:     []Action{ActionGateEvaluate, ActionChangeRecord}}
	if ci.Can(ActionProjectRead, "o1", "p1") {
		t.Fatal("the allow-list must deny project:read to the CI token (403)")
	}
	if !ci.VisibleProject("o1", "p1") {
		t.Fatal("the CI token must still SEE its project (404 would hide what it may record on)")
	}
	if ci.VisibleProject("o1", "p2") || ci.VisibleProject("o2", "p3") {
		t.Fatal("visibility is still bounded by membership")
	}
	if all, orgs, projects := ci.VisibleScope(ActionProjectRead); all || len(orgs)+len(projects) != 0 {
		t.Fatal("VisibleScope mirrors Can: a list omitting project:read scopes to nothing")
	}
	none := ci
	none.Actions = []Action{}
	if !none.VisibleProject("o1", "p1") || none.Can(ActionChangeRecord, "o1", "p1") {
		t.Fatal("an empty list grants nothing and hides nothing")
	}
	// The caller's own Actions are untouched by the visibility check.
	_ = ci.VisibleProject("o1", "p1")
	if len(ci.Actions) != 2 {
		t.Fatal("VisibleProject must not mutate the principal")
	}
}
