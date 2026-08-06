package domain

import (
	"fmt"
	"strings"
	"time"
)

// Webhook is an outbound subscription: cerbix POSTs a signed JSON payload to URL
// on every incident lifecycle event within its scope (a project, or org-wide when
// ProjectID is empty). Secret is used to HMAC-sign deliveries.
type Webhook struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	URL       string `json:"url"`
	Secret    string `json:"secret,omitempty"` // returned only at creation
	Enabled   bool   `json:"enabled"`
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedByEmail resolves the issuer for display (list endpoint only).
	CreatedByEmail string    `json:"created_by_email,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validate enforces webhook invariants.
func (h Webhook) Validate() error {
	if h.OrgID == "" {
		return fmt.Errorf("webhook: org_id is required")
	}
	u := strings.TrimSpace(h.URL)
	if u == "" {
		return fmt.Errorf("webhook: url is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("webhook: url must be http(s)")
	}
	return nil
}

// Incident lifecycle event types carried on a webhook delivery.
const (
	EventIncidentOpened   = "incident.opened"
	EventIncidentUpdated  = "incident.updated"
	EventIncidentResolved = "incident.resolved"
)

// IncidentEvent is a lifecycle change delivered to webhook subscribers. Update is
// set for update/resolved events (the timeline entry that caused the change).
type IncidentEvent struct {
	Type     string          `json:"event"`
	Incident Incident        `json:"incident"`
	Update   *IncidentUpdate `json:"update,omitempty"`
}
