package domain

import (
	"errors"
	"time"
)

// The write side of a phone delivery (FR-041a §4.5, D32, D33, §6.2): what the delivery path hands
// the store before and after a provider call. The read views of the rows are elsewhere.

// Alert kinds of a phone delivery row (`phone_deliveries.alert_kind`). The values are those of
// telephony.AlertKind (D18); they are repeated here because the store, which cannot import the
// telephony package's renderer, decides on two of them: the monitor DOWN kind (D32's attachment) and
// the two burn kinds (no subject for the storm guard, §6.2).
const (
	PhoneAlertMonitorDown      = "monitor_down"
	PhoneAlertMonitorRecovered = "monitor_recovered"
	PhoneAlertServiceHealth    = "service_health"
	PhoneAlertServiceRecovered = "service_recovered"
	PhoneAlertEscalationStep   = "escalation_step"
	PhoneAlertMonitorBurn      = "monitor_burn"
	PhoneAlertServiceBurn      = "service_burn"
)

var phoneAlertKinds = map[string]bool{
	PhoneAlertMonitorDown: true, PhoneAlertMonitorRecovered: true, PhoneAlertServiceHealth: true,
	PhoneAlertServiceRecovered: true, PhoneAlertEscalationStep: true, PhoneAlertMonitorBurn: true,
	PhoneAlertServiceBurn: true,
}

// ValidPhoneAlertKind reports whether k is one of the alert kinds above.
func ValidPhoneAlertKind(k string) bool { return phoneAlertKinds[k] }

// IsPhoneBurnAlert reports whether k is a burn-rate alert: it has no incident and no subject the
// storm guard counts per hour, only the number (§6.2).
func IsPhoneBurnAlert(k string) bool { return k == PhoneAlertMonitorBurn || k == PhoneAlertServiceBurn }

// PhoneDeliveryStaleAfter is how long a `sending` row may wait for its provider's answer, measured
// from `updated_at` (D33). A retry that meets a younger row fails with ErrPhoneDeliveryInFlight; an
// older one is closed as `outcome_unknown` by that retry or by the leader's pass.
const PhoneDeliveryStaleAfter = 60 * time.Second

// ErrPhoneDeliveryClosed: a provider's answer came back for a row that is no longer `sending` — a
// retry or the leader's pass closed it as `outcome_unknown` meanwhile (D33). That row is final; its
// late answer is not stored and is not an event failure.
var ErrPhoneDeliveryClosed = errors.New("the phone delivery was closed before its answer was stored")

// ErrPhoneDeliveryInFlight: the event's delivery to this channel is reserved and its provider call
// may still be in flight (a `sending` row younger than PhoneDeliveryStaleAfter). The outbox retries
// the event; it must not be reported as delivered (D33).
var ErrPhoneDeliveryInFlight = errors.New("a phone delivery of this event is still in flight")

// ErrPhoneChannelGone: the channel of a delivery — or its project — was deleted after the delivery's
// targets were read, so no row can be written for it (FR-041a review P3-4). The delivery to that
// channel ends there: no row, not counted, logged; the event does not fail for it.
var ErrPhoneChannelGone = errors.New("the phone channel was deleted during its delivery")

// ErrPhoneRoutesUncounted marks an error a phone delivery returned BEFORE it counted the event's phone
// routes — the delivery record could not be read, say. Its Resolved is then not a count of anything,
// and a service alert must not report the phone recipients missing (FR-041a review P2-7). Match it with
// errors.Is; PhoneRoutesUncounted marks an error without changing its text.
var ErrPhoneRoutesUncounted = errors.New("the phone routes were not counted")

// PhoneRoutesUncounted returns err marked as ErrPhoneRoutesUncounted, its text unchanged; nil for nil.
func PhoneRoutesUncounted(err error) error {
	if err == nil {
		return nil
	}
	return phoneRoutesUncounted{err}
}

type phoneRoutesUncounted struct{ error }

func (e phoneRoutesUncounted) Unwrap() error      { return e.error }
func (phoneRoutesUncounted) Is(target error) bool { return target == ErrPhoneRoutesUncounted }

// WithDefaults returns the bounds with §6.2's defaults in place of a zero (or negative) value.
func (s TelephonyStorm) WithDefaults() TelephonyStorm {
	if s.PerIncidentPerHour <= 0 {
		s.PerIncidentPerHour = defaultStormPerIncidentHour
	}
	if s.PerNumberPerHour <= 0 {
		s.PerNumberPerHour = defaultStormPerNumberHour
	}
	return s
}

// PhoneReserveInput describes one phone delivery: one alert to one phone channel.
type PhoneReserveInput struct {
	ProjectID string
	ChannelID string
	// OutboxEventID is the idempotency key with ChannelID (D33). Empty for a delivery without an
	// outbox event; such a row is never found again by a retry.
	OutboxEventID string
	Kind          ChannelType // ChannelSMS or ChannelVoice
	AlertKind     string      // one of the PhoneAlert* kinds
	// IncidentID is the incident the alert carries (an escalation step, a service alert, the link of
	// a recovery). For a monitor DOWN it is ignored: the store looks the monitor's open auto-incident
	// up under the monitor's lock (D32).
	IncidentID string
	MonitorID  string
	MonitorSeq int64 // the Seq the monitor event carries; 0 is stored as NULL
	ServiceID  string
	UserID     string // the recipient of a `user` channel
	// ServiceState is the paging state a service's health onset announced (§4.5): one of the
	// ServiceAlertState values, stored NULL when empty or outside them. EscalationStep is the 1-based
	// step that handed the alert over, stored NULL when 0.
	ServiceState   string
	EscalationStep int
	// Number is the destination in E.164, in memory only: the store keeps its keyed hash and its mask,
	// never the number. Required for a reservation; optional for an unsent row (R9).
	Number string
	// Notes known before the call (no_incident_ref, no_public_url, link_omitted, or the cause of
	// not_deliverable).
	Notes []string
	// The storm guard's bounds are not an input: the reservation reads them from instance_settings
	// inside its own transaction, under the number's lock, so they are as current as the count (§6.2,
	// P13).
}

// PhoneReservationStatus is what a reservation decided.
type PhoneReservationStatus string

const (
	// PhoneReservationProceed: the row is `sending` and committed; call the provider, then finish it.
	PhoneReservationProceed PhoneReservationStatus = "proceed"
	// PhoneReservationStormLimited: the storm guard refused the delivery; the row is final
	// (`storm_limited`) and was written by this reservation.
	PhoneReservationStormLimited PhoneReservationStatus = "storm_limited"
	// PhoneReservationClosedStale: a retry met a `sending` row older than PhoneDeliveryStaleAfter and
	// closed it as `outcome_unknown` (final). No call is made.
	PhoneReservationClosedStale PhoneReservationStatus = "closed_stale"
	// PhoneReservationExisting: the row of this event and channel was already final; nothing changed.
	PhoneReservationExisting PhoneReservationStatus = "existing"
)

// PhoneReservation is the row a reservation found or wrote.
type PhoneReservation struct {
	ID         string
	Status     PhoneReservationStatus
	Reason     string // the row's reason after the reservation
	IncidentID string // the row's incident, "" when none (for a monitor DOWN: D32's lookup)
	Attempts   int
}

// PhoneOutcome is a provider's answer as the row keeps it (D34): no response text, ever.
type PhoneOutcome struct {
	Reason     string // PhoneReasonSent, PhoneReasonProviderUnreachable or PhoneReasonProviderRejected
	HTTPStatus int    // 0 when there was no HTTP answer: stored as NULL
	Code       string // "" is stored as NULL (the column refuses '')
	Ref        string // "" is stored as NULL
	Segments   int
	Notes      []string // appended to the row's notes
}
