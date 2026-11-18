package domain

import "time"

// PhoneDeliveryView is one row of a phone delivery as the operator reads it (FR-041a §4.5, R13): the
// reason and the facts of the attempt, the channel's name, and the number as its mask only. It
// never carries a number, a hash or a provider's answer text (P3, D34).
//
// IncidentID and IncidentRef are filled only by the channel's list, where the incident is not
// implied; a delivery with no incident leaves them out.
type PhoneDeliveryView struct {
	ID             string    `json:"id"`
	ChannelID      string    `json:"channel_id"`
	ChannelName    string    `json:"channel_name"`
	IncidentID     string    `json:"incident_id,omitempty"`
	IncidentRef    string    `json:"incident_ref,omitempty"`
	Kind           string    `json:"kind"`
	AlertKind      string    `json:"alert_kind"`
	NumberMasked   *string   `json:"number_masked"`
	Reason         string    `json:"reason"`
	Notes          []string  `json:"notes"`
	ProviderStatus *int      `json:"provider_status"`
	ProviderCode   *string   `json:"provider_code"`
	ProviderRef    *string   `json:"provider_ref"`
	Segments       int       `json:"segments"`
	Attempts       int       `json:"attempts"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// What the delivery was about (§4.5), so a list reads without the incident beside it. Each is
	// null when there is none: Subject when the row names neither a monitor nor a service or it was
	// deleted; ServiceState for anything but a service's health onset; EscalationStep (1-based) for
	// anything but an escalation step; RecipientDisplayName for a `number` channel or a deleted
	// user — a display name, never an email.
	Subject              *PhoneDeliverySubject `json:"subject"`
	ServiceState         *string               `json:"service_state"`
	EscalationStep       *int                  `json:"escalation_step"`
	RecipientDisplayName *string               `json:"recipient_display_name"`
}

// Kinds of a delivery's subject.
const (
	PhoneDeliverySubjectMonitor = "monitor"
	PhoneDeliverySubjectService = "service"
)

// PhoneDeliverySubject is the monitor or service a delivery was about, resolved at read.
type PhoneDeliverySubject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
