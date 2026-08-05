package domain

import "time"

// AgentToken is a database-managed pull-agent bearer token, authorizing one region.
// The secret itself is never stored (only its hash) or returned after creation.
type AgentToken struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Region    string     `json:"region"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}
