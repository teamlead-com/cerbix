package domain

import (
	"fmt"
	"log/slog"
)

// PhoneTarget is one phone channel as a delivery meets it at send (FR-041a D31 rule 1, P1, P4): the
// channel's rule-1 verdict, read through the same SQL the route predicates embed, and — when it has
// one — the VERIFIED number it would be sent to, opened into memory for this send only.
//
// The number never leaves memory: it is not serialized (`json:"-"`), a log record gets LogValue
// (no number), and every fmt verb gets Format (no number) — so a %v of a target in an error or a
// log line cannot quote it (P3).
type PhoneTarget struct {
	ChannelID string
	ProjectID string
	Kind      ChannelType // ChannelSMS or ChannelVoice
	Recipient string      // PhoneRecipientUser or PhoneRecipientNumber
	UserID    string      // a `user` channel's recipient; "" in `number` mode or once deleted
	// NotRouteReason is why the channel is not a route now (one of the Deliverable* reasons, R11);
	// "" when it is one.
	NotRouteReason string
	// Language is the recipient's own voice language (a `user` channel's user_phones.language); ""
	// means the instance's voice_language.
	Language string
	// Number is the destination in E.164: the user's verified number (`user` mode) or the channel's
	// verified number (`number` mode); "" when there is no verified number (P1).
	Number string `json:"-"`
	// NumberErr is why a verified number that exists could not be opened (D29: the key it was sealed
	// with is gone, or the ciphertext is damaged). Only this target is affected: the delivery fails
	// for its channel alone and the event is retried, while the event's other channels go out. Never
	// serialized; its text names the channel, never the number.
	NumberErr error `json:"-"`
}

// Route reports whether the channel is a route now (D31 rule 1) and has a verified number to call.
func (t PhoneTarget) Route() bool { return t.NotRouteReason == "" && t.Number != "" }

// Unreadable reports whether the channel is a route by configuration (D31 rule 1) whose verified
// number could not be opened: counted in Resolved, which follows rule 1 and nothing else, and failed
// with NumberErr rather than recorded as not deliverable.
func (t PhoneTarget) Unreadable() bool { return t.NotRouteReason == "" && t.NumberErr != nil }

// LogValue keeps the number out of every structured log record.
func (t PhoneTarget) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("channel_id", t.ChannelID), slog.String("kind", string(t.Kind)),
		slog.String("recipient", t.Recipient), slog.String("user_id", t.UserID),
		slog.String("not_route_reason", t.NotRouteReason))
}

// Format keeps the number out of every fmt verb (%v, %+v, %s, %#v).
func (t PhoneTarget) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "{channel:%s kind:%s recipient:%s user:%s not_route_reason:%s}",
		t.ChannelID, t.Kind, t.Recipient, t.UserID, t.NotRouteReason)
}

// PhoneEventRef is what a phone delivery records of the event it delivers (D32, D33, §4.5): the
// outbox event, which is the idempotency key with the channel; the subject; the incident when one is
// known; and the notes only the caller can know.
type PhoneEventRef struct {
	OutboxEventID string
	// IncidentID is the incident the alert carries (an escalation step's, a service alert's, a
	// recovery's link). A monitor DOWN's is looked up by the store under D32's lock instead.
	IncidentID string
	MonitorID  string
	// MonitorSeq is a monitor event's Seq. Required on every monitor DOWN: D32 attaches only the rows
	// of the outage whose seq the incident opener sees, and a 0 is stored as NULL and never attached.
	MonitorSeq int64
	ServiceID  string
	// ServiceState is the state a service's health onset announced; EscalationStep the 1-based
	// escalation step that handed the alert over (§4.5). Empty and 0 for every other alert.
	ServiceState   string
	EscalationStep int
	// Notes are the row notes the caller decides (§4.5): no_incident_ref, no_public_url. The renderer
	// adds link_omitted itself.
	Notes []string
}
