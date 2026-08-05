package domain

// SearchHit is one result of a global search. The handler returns only hits the
// caller may see; OrgID + ProjectID carry the scope used for that visibility
// check (for a project hit, ProjectID is the project's own id).
type SearchHit struct {
	Type      string `json:"type"` // monitor | project | incident
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id"`
	Label     string `json:"label"`         // monitor/project name, or incident title
	Sub       string `json:"sub,omitempty"` // monitor type, project slug, or incident status
}
