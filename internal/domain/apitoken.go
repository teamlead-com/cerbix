package domain

import (
	"fmt"
	"strings"
	"time"
)

// ApiToken is a service-account credential. It grants a role within an
// organization (or one of its projects) to a machine caller presenting the token
// as a bearer credential. Only a hash of the secret is stored; the plaintext is
// shown once at creation.
type ApiToken struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	Name      string `json:"name"`
	Role      Role   `json:"role"`
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedByEmail resolves the issuer for display (list endpoint only).
	CreatedByEmail string     `json:"created_by_email,omitempty"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Scope returns the scope implied by whether ProjectID is set.
func (t ApiToken) Scope() Scope {
	if t.ProjectID == "" {
		return ScopeOrg
	}
	return ScopeProject
}

// Validate enforces token invariants: a name, an org, and a role valid at the
// token's scope (project-scoped or org-scoped).
func (t ApiToken) Validate() error {
	if t.OrgID == "" {
		return fmt.Errorf("api token: org_id is required")
	}
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("api token: name is required")
	}
	if !t.Role.Valid() {
		return fmt.Errorf("api token: unknown role %q", t.Role)
	}
	if !t.Role.ValidForScope(t.Scope()) {
		return fmt.Errorf("api token: role %q not allowed at %s scope", t.Role, t.Scope())
	}
	return nil
}
