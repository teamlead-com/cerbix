// Package authz is the authorization decision layer. It is pure (no I/O): given
// a Principal (an authenticated user plus their memberships) it decides whether
// an Action is allowed against a target org/project.
//
// This is the single source of truth for role→permission rules. Tenant isolation
// is enforced here too: a Principal can only act within orgs/projects they hold a
// membership in (global admins excepted).
package authz

import "github.com/teamlead-com/cerbix/internal/domain"

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
)

// roleGrants maps each role to the actions it grants where it applies. Scope
// applicability (org-wide vs a single project) is handled in Principal.Can.
var roleGrants = map[domain.Role]map[Action]bool{
	domain.RoleOrgAdmin: {
		ActionOrgRead:       true,
		ActionOrgManage:     true,
		ActionProjectRead:   true,
		ActionProjectManage: true,
		ActionProjectWrite:  true,
	},
	domain.RoleProjectAdmin: {
		ActionProjectRead:   true,
		ActionProjectManage: true,
		ActionProjectWrite:  true,
	},
	domain.RoleEditor: {
		ActionOrgRead:      true,
		ActionProjectRead:  true,
		ActionProjectWrite: true,
	},
	domain.RoleViewer: {
		ActionOrgRead:     true,
		ActionProjectRead: true,
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
}

// Can reports whether the principal may perform action on the target identified
// by orgID and (optionally) projectID.
//
//   - Global admins may do anything.
//   - ActionGlobalManage requires global admin.
//   - Otherwise a membership must apply to the target and grant the action:
//     org-scoped memberships apply to the org and all its projects; project-scoped
//     memberships apply only to their own project.
func (p Principal) Can(action Action, orgID, projectID string) bool {
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
