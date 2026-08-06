package domain

import "time"

// AdminUserMembership is one grant shown on the instance-wide Users page,
// with the org/project names resolved for display.
type AdminUserMembership struct {
	OrgID       string `json:"org_id"`
	OrgName     string `json:"org_name"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	Role        Role   `json:"role"`
}

// AdminUser is the user-keyed row of the instance-wide Users page: one row per
// user (unlike Member, which is membership-keyed), with an empty memberships
// list for users outside any organization.
type AdminUser struct {
	User
	AuthType     string                `json:"auth_type"` // local | oidc | both | none
	LastActiveAt *time.Time            `json:"last_active_at,omitempty"`
	Memberships  []AdminUserMembership `json:"memberships"`
}
