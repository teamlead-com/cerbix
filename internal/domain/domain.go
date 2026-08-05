// Package domain holds cerbix core entities and their invariants: the
// organization→project tenant hierarchy, users, and role-based memberships.
//
// Business rules (which roles are valid, at which scope a role may be granted,
// tenant-isolation intent) live here — not in transport or infra layers.
package domain

import (
	"fmt"
	"time"
)

// Role is a permission role granted to a user within an organization or project.
type Role string

const (
	// RoleOrgAdmin manages projects, members, and all monitors within one org.
	RoleOrgAdmin Role = "org_admin"
	// RoleProjectAdmin (a.k.a. Maintainer) manages one project and its members.
	RoleProjectAdmin Role = "project_admin"
	// RoleEditor creates/edits monitors and settings but not members.
	RoleEditor Role = "editor"
	// RoleViewer has read-only access.
	RoleViewer Role = "viewer"
)

// Scope identifies where a role is granted.
type Scope string

const (
	// ScopeOrg is an organization-level grant (membership.project_id IS NULL).
	ScopeOrg Scope = "org"
	// ScopeProject is a project-level grant (membership.project_id set).
	ScopeProject Scope = "project"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleOrgAdmin, RoleProjectAdmin, RoleEditor, RoleViewer:
		return true
	default:
		return false
	}
}

// ValidForScope reports whether role r may be granted at the given scope.
//
//   - org scope:     org_admin, editor, viewer
//   - project scope: project_admin, editor, viewer
//
// project_admin is project-only; org_admin is org-only; editor/viewer apply to both.
func (r Role) ValidForScope(s Scope) bool {
	switch s {
	case ScopeOrg:
		return r == RoleOrgAdmin || r == RoleEditor || r == RoleViewer
	case ScopeProject:
		return r == RoleProjectAdmin || r == RoleEditor || r == RoleViewer
	default:
		return false
	}
}

// Organization is the top-level tenant and the isolation boundary.
type Organization struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Project belongs to an organization and is the primary unit of permissions.
type Project struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User is a cerbix account provisioned from OIDC on first login.
type User struct {
	ID            string    `json:"id"`
	OIDCSub       string    `json:"oidc_sub"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	IsGlobalAdmin bool      `json:"is_global_admin"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Membership grants a user a role within an org, or within a specific project of
// that org. ProjectID == "" means an org-level grant.
type Membership struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	OrgID     string    `json:"org_id"`
	ProjectID string    `json:"project_id,omitempty"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Member is a membership enriched with the user's identity for the members UI.
type Member struct {
	Membership
	Email        string     `json:"email"`
	DisplayName  string     `json:"display_name"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
}

// Scope returns the scope implied by whether ProjectID is set.
func (m Membership) Scope() Scope {
	if m.ProjectID == "" {
		return ScopeOrg
	}
	return ScopeProject
}

// Validate enforces the membership invariants: a valid role at a scope
// consistent with whether a project is targeted.
func (m Membership) Validate() error {
	if m.UserID == "" {
		return fmt.Errorf("membership: user_id is required")
	}
	if m.OrgID == "" {
		return fmt.Errorf("membership: org_id is required")
	}
	if !m.Role.Valid() {
		return fmt.Errorf("membership: unknown role %q", m.Role)
	}
	if !m.Role.ValidForScope(m.Scope()) {
		return fmt.Errorf("membership: role %q not allowed at %s scope", m.Role, m.Scope())
	}
	return nil
}
