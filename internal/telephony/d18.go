package telephony

import (
	"encoding/json"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// D18 (FR-041a): phone receives only the alerts that ask a person to act. The table is by outbox topic
// and the payload fields that split it; a row it does not list is not phoned. This file is the one
// place it is written down — the outbox worker asks it, and P15 tests it.

// PhoneKindFor returns the kind of phone alert an outbox event is, and false when D18 does not phone
// it: a reminder, a burn CLEAR, a service close for any reason other than `recovered`, every other
// topic, and a payload that does not decode. A monitor transition the notification policy does not
// deliver at all (domain.MonitorTransition.ShouldNotify) is not phoned either.
func PhoneKindFor(topic string, payload []byte) (AlertKind, bool) {
	switch topic {
	case domain.TopicMonitorTransition:
		var mt domain.MonitorTransition
		if json.Unmarshal(payload, &mt) != nil || mt.Reminder || !mt.ShouldNotify() {
			return "", false
		}
		switch mt.Cur {
		case domain.StatusDown:
			return KindMonitorDown, true
		case domain.StatusUp:
			return KindMonitorRecovered, true
		}
	case domain.TopicServiceAlert:
		var a domain.ServiceAlert
		if json.Unmarshal(payload, &a) != nil {
			return "", false
		}
		switch {
		case a.Signal == domain.ServiceSignalHealth && a.Firing:
			return KindServiceHealth, true
		case a.Signal == domain.ServiceSignalHealth && a.CloseReason == domain.CloseRecovered:
			return KindServiceRecovered, true
		case a.Signal == domain.ServiceSignalBurn && a.Firing:
			return KindServiceBurn, true
		}
	case domain.TopicSLOBurnAlert:
		var a domain.SLOBurnAlert
		if json.Unmarshal(payload, &a) == nil && a.Firing {
			return KindMonitorBurn, true
		}
	case domain.TopicEscalationStep:
		var a domain.EscalationStepAlert
		if json.Unmarshal(payload, &a) == nil {
			return KindEscalationStep, true
		}
	}
	return "", false
}

// PhoneAlertFor is D18's table as its three columns: whether the event is phoned at all, and whether
// an sms and a voice channel receive it.
func PhoneAlertFor(topic string, payload []byte) (alert, sms, voice bool) {
	kind, ok := PhoneKindFor(topic, payload)
	if !ok {
		return false, false, false
	}
	return true, true, kind.Voiced()
}

// Voiced reports whether a voice channel receives an alert of this kind. A recovery — a monitor's or
// a service's — is an SMS only: nobody is called to be told that something is fine (D18).
func (k AlertKind) Voiced() bool {
	return k != KindMonitorRecovered && k != KindServiceRecovered
}
