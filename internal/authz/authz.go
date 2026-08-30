// Package authz is the authorization decision layer. It is pure (no I/O): given
// a Principal (an authenticated user plus their memberships) it decides whether
// an Action is allowed against a target org/project.
//
// This is the single source of truth for role→permission rules. Tenant isolation
// is enforced here too: a Principal can only act within orgs/projects they hold a
// membership in (global admins excepted).
package authz

import (
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Action is a permission checked by handlers.
type Action string

const (
	// ActionGlobalManage: create/delete organizations, grant global admin. Global admin only.
	ActionGlobalManage Action = "global:manage"
	// ActionOrgRead: view an organization and list its projects.
	ActionOrgRead Action = "org:read"
	// ActionOrgManage: create projects and manage members within an organization.
	ActionOrgManage Action = "org:manage"
	// ActionProjectRead: view a project.
	ActionProjectRead Action = "project:read"
	// ActionProjectManage: manage a project's members and settings.
	ActionProjectManage Action = "project:manage"
	// ActionProjectWrite: create/edit monitors and settings within a project.
	ActionProjectWrite Action = "project:write"

	// The reliability gate (FR-024, D12): three actions checked where every other action is,
	// mapped in v1 onto the existing project roles. No handler compares roles itself.

	// ActionGateEvaluate: ask the gate for a decision, read a service's policy and overrides,
	// and read the decision ledger. viewer+.
	ActionGateEvaluate Action = "gate:evaluate"
	// ActionGatePolicyWrite: create, replace or delete a service's gate policy. editor+.
	ActionGatePolicyWrite Action = "gate:policy:write"
	// ActionGateOverride: create or revoke a gate override — the one act that lets a BLOCK
	// through. project_admin+.
	ActionGateOverride Action = "gate:override"

	// Change intelligence (FR-025, D12): one new action. The timeline, the comparison and the
	// incident links are reads under ActionProjectRead — no `change:read` exists.

	// ActionChangeRecord: record a change phase on a service. editor+.
	ActionChangeRecord Action = "change:record"
)

// actionCatalogue is the CLOSED list of every Action — what a token's `actions` allow-list is
// validated against on create (FR-025 D12, `action_unknown`).
var actionCatalogue = []Action{
	ActionGlobalManage,
	ActionOrgRead,
	ActionOrgManage,
	ActionProjectRead,
	ActionProjectManage,
	ActionProjectWrite,
	ActionGateEvaluate,
	ActionGatePolicyWrite,
	ActionGateOverride,
	ActionChangeRecord,
}

// Actions returns a copy of the central action catalogue.
func Actions() []Action {
	out := make([]Action, len(actionCatalogue))
	copy(out, actionCatalogue)
	return out
}

// ValidAction reports whether a names an Action of the catalogue.
func ValidAction(a Action) bool {
	for _, known := range actionCatalogue {
		if known == a {
			return true
		}
	}
	return false
}

// roleGrants maps each role to the actions it grants where it applies. Scope
// applicability (org-wide vs a single project) is handled in Principal.Can.
var roleGrants = map[domain.Role]map[Action]bool{
	domain.RoleOrgAdmin: {
		ActionOrgRead:         true,
		ActionOrgManage:       true,
		ActionProjectRead:     true,
		ActionProjectManage:   true,
		ActionProjectWrite:    true,
		ActionGateEvaluate:    true,
		ActionGatePolicyWrite: true,
		ActionGateOverride:    true,
		ActionChangeRecord:    true,
	},
	domain.RoleProjectAdmin: {
		ActionProjectRead:     true,
		ActionProjectManage:   true,
		ActionProjectWrite:    true,
		ActionGateEvaluate:    true,
		ActionGatePolicyWrite: true,
		ActionGateOverride:    true,
		ActionChangeRecord:    true,
	},
	domain.RoleEditor: {
		ActionOrgRead:         true,
		ActionProjectRead:     true,
		ActionProjectWrite:    true,
		ActionGateEvaluate:    true,
		ActionGatePolicyWrite: true,
		ActionChangeRecord:    true,
	},
	domain.RoleViewer: {
		ActionOrgRead:      true,
		ActionProjectRead:  true,
		ActionGateEvaluate: true,
	},
}

// Principal is an authenticated user together with their memberships.
//
// ViaToken marks a principal established from a service-account API token rather
// than an interactive session; handlers use it to attribute writes (e.g. an
// incident's source) without changing authorization, which stays role-driven.
type Principal struct {
	UserID        string
	IsGlobalAdmin bool
	Memberships   []domain.Membership
	ViaToken      bool
	// AuditLabel is the TRUSTED human-readable name of the acting subject, set by
	// the auth layer alongside the identity it authenticated: a user's email (or
	// display name), an API token's name, or a client-credential subject. Audit
	// rows carry it beside the typed columns so a reader sees WHO acted without
	// resolving a uuid ([288] P1-3). Never client-supplied.
	AuditLabel string
	// Actions is an API token's ALLOW-LIST (FR-025 D12): nil means the role decides — every
	// principal that predates the list, and every session principal, is nil here. A non-nil
	// list is INTERSECTED with the role grants: Can(action) = roleGrants[role] ∋ action AND
	// action ∈ Actions. It is consulted in Can (and its scoping mirror VisibleScope) and
	// nowhere else; handlers keep calling Can. Set by the auth layer from the token row.
	Actions []Action
}

// allows is the allow-list half of the D12 intersection: true when the principal carries no
// list, or when the list names the action.
func (p Principal) allows(action Action) bool {
	if p.Actions == nil {
		return true
	}
	for _, a := range p.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// AuditActorLabel returns the label to record for this principal, falling back
// to the identity when the auth layer set none — an audit row is never
// actorless, even for a path that predates the label.
func (p Principal) AuditActorLabel() string {
	if p.AuditLabel != "" {
		return p.AuditLabel
	}
	return p.UserID
}

// SyntheticTokenActorPrefix marks a Principal.UserID that is NOT a real user uuid but a
// machine identity derived from a Cerbix API token ("apitoken:<token-id>"). The auth layer
// constructs it; audit writers must map such an id to a NULL actor (via_token already
// attributes the action to the machine). OIDC client-credentials principals carry a REAL
// JIT-provisioned user uuid and keep their attribution.
const SyntheticTokenActorPrefix = "apitoken:"

// AuditUserID returns the value an audit row's actor_user_id (uuid, nullable) may carry for
// this principal: the user uuid, or "" (NULL) for a synthetic token identity.
func (p Principal) AuditUserID() string {
	if p.ViaToken && strings.HasPrefix(p.UserID, SyntheticTokenActorPrefix) {
		return ""
	}
	return p.UserID
}

// Can reports whether the principal may perform action on the target identified
// by orgID and (optionally) projectID.
//
//   - A token's allow-list (Actions, when non-nil) must name the action — the ONE place the
//     list is consulted (FR-025 D12); it bounds even a principal the role would let through.
//   - Global admins may do anything.
//   - ActionGlobalManage requires global admin.
//   - Otherwise a membership must apply to the target and grant the action:
//     org-scoped memberships apply to the org and all its projects; project-scoped
//     memberships apply only to their own project.
func (p Principal) Can(action Action, orgID, projectID string) bool {
	if !p.allows(action) {
		return false
	}
	if p.IsGlobalAdmin {
		return true
	}
	if action == ActionGlobalManage {
		return false
	}
	if orgID == "" {
		return false
	}
	for _, m := range p.Memberships {
		if m.OrgID != orgID {
			continue
		}
		if m.ProjectID != "" {
			// Project-scoped grant: only applies to its own project.
			if projectID == "" || m.ProjectID != projectID {
				continue
			}
		}
		if roleGrants[m.Role][action] {
			return true
		}
	}
	return false
}

// InOrg reports whether the principal has any membership in the organization
// (org-level or project-level), or is a global admin. Used to decide whether the
// org is visible at all — distinct from Can(ActionOrgRead), which a project-only
// member does not hold at org scope.
func (p Principal) InOrg(orgID string) bool {
	if p.IsGlobalAdmin {
		return true
	}
	for _, m := range p.Memberships {
		if m.OrgID == orgID {
			return true
		}
	}
	return false
}

// VisibleOrg reports whether the principal can see the organization at all.
func (p Principal) VisibleOrg(orgID string) bool {
	return p.InOrg(orgID)
}

// VisibleProject reports whether the principal can see the project at all.
func (p Principal) VisibleProject(orgID, projectID string) bool {
	return p.Can(ActionProjectRead, orgID, projectID)
}

// VisibleScope returns the set of orgs/projects on which the principal holds
// action, for pushing tenant scoping DOWN into a query (WHERE org_id = ANY(orgIDs)
// OR project_id = ANY(projectIDs)) instead of filtering rows after a global LIMIT.
// allOrgs=true means no restriction (global admin). orgIDs are org-level grants (all
// projects in them); projectIDs are project-scoped grants. It mirrors Can exactly — the
// allow-list included: a token whose list omits the action sees nothing.
func (p Principal) VisibleScope(action Action) (allOrgs bool, orgIDs, projectIDs []string) {
	if !p.allows(action) {
		return false, nil, nil
	}
	if p.IsGlobalAdmin {
		return true, nil, nil
	}
	for _, m := range p.Memberships {
		if !roleGrants[m.Role][action] {
			continue
		}
		if m.ProjectID == "" {
			orgIDs = append(orgIDs, m.OrgID)
		} else {
			projectIDs = append(projectIDs, m.ProjectID)
		}
	}
	return false, orgIDs, projectIDs
}
