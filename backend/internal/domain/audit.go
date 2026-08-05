package domain

import "time"

// AuditEntry records an access-relevant action within an organization. ActorEmail
// and ActorName are populated on read (joined from users); a nil/deleted actor
// reads back empty.
type AuditEntry struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"org_id"`
	ActorUserID string    `json:"actor_user_id,omitempty"`
	ActorEmail  string    `json:"actor_email,omitempty"`
	ActorName   string    `json:"actor_name,omitempty"`
	ViaToken    bool      `json:"via_token"`
	Action      string    `json:"action"`
	Target      string    `json:"target,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}
