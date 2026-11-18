package telephony

import (
	"encoding/json"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// P15 (FR-041a D18): every outbox topic, and every predicate row of D18's table, against an sms and a
// voice channel. A row the table does not list is not phoned.
func TestPhoneAlertForD18(t *testing.T) {
	type row struct {
		name             string
		topic            string
		payload          []byte
		kind             AlertKind
		phoned, sms, vox bool
	}
	mt := func(prev, cur domain.MonitorStatus, reminder bool) []byte {
		return mustJSON(t, domain.MonitorTransition{MonitorID: "m1", Prev: prev, Cur: cur, Reminder: reminder, Seq: 3})
	}
	sa := func(signal domain.ServiceAlertSignal, firing bool, reason domain.ServiceAlertCloseReason) []byte {
		a := domain.ServiceAlert{ServiceID: "s1", Signal: signal, Firing: firing, CloseReason: reason,
			Recipients: []string{"c1"}}
		if signal == domain.ServiceSignalHealth && firing {
			a.State = domain.ServiceAlertDown
		}
		return mustJSON(t, a)
	}
	rows := []row{
		{"monitor DOWN", domain.TopicMonitorTransition, mt(domain.StatusUp, domain.StatusDown, false), KindMonitorDown, true, true, true},
		{"monitor DOWN from pending", domain.TopicMonitorTransition, mt(domain.StatusPending, domain.StatusDown, false), KindMonitorDown, true, true, true},
		{"monitor recovery", domain.TopicMonitorTransition, mt(domain.StatusDown, domain.StatusUp, false), KindMonitorRecovered, true, true, false},
		{"monitor reminder", domain.TopicMonitorTransition, mt(domain.StatusDown, domain.StatusDown, true), "", false, false, false},
		{"monitor reminder (up)", domain.TopicMonitorTransition, mt(domain.StatusUp, domain.StatusUp, true), "", false, false, false},
		// Transitions the notification policy never delivers are not phoned either.
		{"monitor pending→up", domain.TopicMonitorTransition, mt(domain.StatusPending, domain.StatusUp, false), "", false, false, false},
		{"monitor down→down", domain.TopicMonitorTransition, mt(domain.StatusDown, domain.StatusDown, false), "", false, false, false},
		{"monitor up→pending", domain.TopicMonitorTransition, mt(domain.StatusUp, domain.StatusPending, false), "", false, false, false},

		{"service health firing", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, true, ""), KindServiceHealth, true, true, true},
		{"service health recovered", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseRecovered), KindServiceRecovered, true, true, false},
		{"service burn firing", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, true, ""), KindServiceBurn, true, true, true},
		{"service burn CLEAR", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, false, domain.CloseRecovered), "", false, false, false},
		{"service burn CLEAR (rule removed)", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, false, domain.CloseRuleRemoved), "", false, false, false},

		{"slo burn firing", domain.TopicSLOBurnAlert, mustJSON(t, domain.SLOBurnAlert{MonitorID: "m1", Firing: true, BurnRate: 14.4}), KindMonitorBurn, true, true, true},
		{"slo burn CLEAR", domain.TopicSLOBurnAlert, mustJSON(t, domain.SLOBurnAlert{MonitorID: "m1", Firing: false}), "", false, false, false},

		{"escalation step", domain.TopicEscalationStep, mustJSON(t, domain.EscalationStepAlert{IncidentID: "i1", Step: 0, ChannelIDs: []string{"c1"}}), KindEscalationStep, true, true, true},
		{"escalation step repeat_last", domain.TopicEscalationStep, mustJSON(t, domain.EscalationStepAlert{IncidentID: "i1", Step: 2, Repeat: true, ChannelIDs: []string{"c1"}}), KindEscalationStep, true, true, true},
		{"escalation step of a service", domain.TopicEscalationStep, mustJSON(t, domain.EscalationStepAlert{IncidentID: "i1", ServiceID: "s1", Step: 1, ChannelIDs: []string{"c1"}}), KindEscalationStep, true, true, true},

		{"sla report", domain.TopicSLAReport, mustJSON(t, domain.SLAReport{ProjectID: "p1"}), "", false, false, false},
		{"region worker alert", domain.TopicRegionWorkerAlert, mustJSON(t, domain.RegionWorkerAlert{ProjectID: "p1"}), "", false, false, false},
		{"incident event", domain.TopicIncidentEvent, mustJSON(t, domain.IncidentEvent{Type: domain.EventIncidentOpened}), "", false, false, false},
		{"subscriber confirm", domain.TopicSubscriberConfirm, mustJSON(t, domain.SubscriberConfirm{To: "a@example.com"}), "", false, false, false},
		{"incident correlation", domain.TopicIncidentCorrelation, []byte(`{"incident_id":"i1"}`), "", false, false, false},
		{"unknown topic", "not_a_topic", []byte(`{}`), "", false, false, false},
		{"undecodable payload", domain.TopicMonitorTransition, []byte(`{`), "", false, false, false},
	}
	// Every service close reason other than `recovered` is a close that did not recover: not phoned.
	for _, reason := range []domain.ServiceAlertCloseReason{
		domain.CloseVisibilityLost, domain.CloseEnteredMaintenance, domain.CloseOwnershipDisabled,
		domain.ClosePolicyChanged, domain.CloseBurnDisabled, domain.CloseRuleRemoved,
		domain.CloseServiceDeleted, domain.CloseUndelivered,
	} {
		rows = append(rows, row{"service health close " + string(reason), domain.TopicServiceAlert,
			sa(domain.ServiceSignalHealth, false, reason), "", false, false, false})
	}

	covered := map[string]bool{}
	for _, r := range rows {
		covered[r.topic] = true
		kind, ok := PhoneKindFor(r.topic, r.payload)
		if ok != r.phoned || kind != r.kind {
			t.Errorf("%s: PhoneKindFor = (%q, %v); want (%q, %v)", r.name, kind, ok, r.kind, r.phoned)
		}
		alert, sms, voice := PhoneAlertFor(r.topic, r.payload)
		if alert != r.phoned || sms != r.sms || voice != r.vox {
			t.Errorf("%s: PhoneAlertFor = (alert %v, sms %v, voice %v); want (%v, %v, %v)",
				r.name, alert, sms, voice, r.phoned, r.sms, r.vox)
		}
		if ok && kind.Voiced() != r.vox {
			t.Errorf("%s: %q.Voiced() = %v; want %v", r.name, kind, kind.Voiced(), r.vox)
		}
	}
	for _, topic := range domain.AllTopics() {
		if !covered[topic] {
			t.Errorf("topic %q has no row: P15 enumerates every topic", topic)
		}
	}
	// Every kind the renderer knows is reachable from the table, and only those.
	for _, k := range []AlertKind{KindMonitorDown, KindMonitorRecovered, KindServiceHealth, KindServiceRecovered,
		KindEscalationStep, KindMonitorBurn, KindServiceBurn} {
		want := k != KindMonitorRecovered && k != KindServiceRecovered
		if k.Voiced() != want {
			t.Errorf("%q.Voiced() = %v; want %v", k, k.Voiced(), want)
		}
	}
}
