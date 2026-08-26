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
	// Seq is this incident's lifecycle sequence, and with `Incident.ID` it is the pair a RECEIVER
	// dedupes and orders on: unique per event, monotonic per incident, and stable across the retries
	// at-least-once delivery guarantees will happen.
	//
	// cerbix orders these events in DISPATCH — the outbox will not release one while an earlier event
	// of the same incident is undelivered — and stops there, because it has to (D-0177). A request
	// whose worker lost its lease mid-flight can still land, and nothing on this side reaches into a
	// receiver's queue to reorder it.
	//
	// Two different things can be built on this pair, and they are not interchangeable. Keeping the
	// highest seq applied per incident gives CURRENT STATE that cannot regress — a retry is
	// idempotent and a late lower seq is discarded — but the discarded event is gone, so that
	// receiver is always right about now and may never show a step it skipped. Tracking the NEXT
	// expected seq and buffering what runs ahead of it reconstructs the exact history, at the cost of
	// needing a bounded wait: an event can be dead-lettered here and then never arrives at all.
	//
	// Absent (0) on payloads written before the fence existed. Those predate the contract.
	Seq int64 `json:"seq,omitempty"`
}
