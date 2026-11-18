package outbox

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func payload(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// P15 (FR-041a D18) through the worker: every outbox topic and every predicate row of D18's table,
// delivered to a monitor / snapshot / step that holds an sms channel, a voice channel and a Slack
// channel. The phone channels receive exactly the table; what the Slack channel receives is today's.
func TestPhoneReceivesExactlyD18(t *testing.T) {
	all := []string{rigSlack, rigSMS, rigVoice}
	mt := func(prev, cur domain.MonitorStatus, reminder bool) []byte {
		return payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: prev, Cur: cur, Reminder: reminder, Seq: 7})
	}
	sa := func(signal domain.ServiceAlertSignal, firing bool, reason domain.ServiceAlertCloseReason) []byte {
		a := domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceName: "Checkout", ServiceSlug: "checkout",
			Signal: signal, Firing: firing, CloseReason: reason, Seq: 1, Recipients: all,
			BurnRate: 6, WindowSeconds: 21600, Window: "30d"}
		if signal == domain.ServiceSignalHealth && firing {
			a.State = domain.ServiceAlertDown
		}
		return payload(t, a)
	}
	burn := func(firing bool) []byte {
		return payload(t, domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", WindowSeconds: 3600, BurnRate: 14.4, Firing: firing})
	}
	step := func(repeat bool) []byte {
		return payload(t, domain.EscalationStepAlert{IncidentID: "inc1", MonitorID: "m1", MonitorName: "payments-api",
			SubjectName: "payments-api", Step: 1, Repeat: repeat, ChannelIDs: all})
	}
	rows := []struct {
		name       string
		topic      string
		payload    []byte
		sms, voice bool
	}{
		{"monitor DOWN", domain.TopicMonitorTransition, mt(domain.StatusUp, domain.StatusDown, false), true, true},
		{"monitor recovery", domain.TopicMonitorTransition, mt(domain.StatusDown, domain.StatusUp, false), true, false},
		{"monitor reminder", domain.TopicMonitorTransition, mt(domain.StatusDown, domain.StatusDown, true), false, false},
		{"service health firing", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, true, ""), true, true},
		{"service health recovered", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseRecovered), true, false},
		{"service health visibility_lost", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseVisibilityLost), false, false},
		{"service health entered_maintenance", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseEnteredMaintenance), false, false},
		{"service health ownership_disabled", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseOwnershipDisabled), false, false},
		{"service health policy_changed", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.ClosePolicyChanged), false, false},
		{"service health service_deleted", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseServiceDeleted), false, false},
		{"service health undelivered", domain.TopicServiceAlert, sa(domain.ServiceSignalHealth, false, domain.CloseUndelivered), false, false},
		{"service burn firing", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, true, ""), true, true},
		{"service burn CLEAR", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, false, domain.CloseRecovered), false, false},
		{"service burn rule_removed", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, false, domain.CloseRuleRemoved), false, false},
		{"service burn burn_disabled", domain.TopicServiceAlert, sa(domain.ServiceSignalBurn, false, domain.CloseBurnDisabled), false, false},
		{"slo burn firing", domain.TopicSLOBurnAlert, burn(true), true, true},
		{"slo burn CLEAR", domain.TopicSLOBurnAlert, burn(false), false, false},
		{"escalation step", domain.TopicEscalationStep, step(false), true, true},
		{"escalation step repeat_last", domain.TopicEscalationStep, step(true), true, true},
		{"sla report", domain.TopicSLAReport, payload(t, domain.SLAReport{ProjectID: "p1", ProjectName: "prod"}), false, false},
		{"region worker alert", domain.TopicRegionWorkerAlert, payload(t, domain.RegionWorkerAlert{Region: "eu", ProjectID: "p1"}), false, false},
		{"incident event", domain.TopicIncidentEvent, payload(t, domain.IncidentEvent{Type: domain.EventIncidentOpened,
			Incident: domain.Incident{ID: "inc1", ProjectID: "p1"}}), false, false},
		{"subscriber confirm", domain.TopicSubscriberConfirm, payload(t, domain.SubscriberConfirm{To: "a@example.com"}), false, false},
		{"incident correlation", domain.TopicIncidentCorrelation, []byte(`{"incident_id":"inc1"}`), false, false},
	}
	covered := map[string]bool{}
	for i, r := range rows {
		covered[r.topic] = true
		rig := newPhoneRig(t)
		_, sms, calls := rig.run(domain.OutboxEvent{ID: fmt.Sprintf("e%d", i), Topic: r.topic, Payload: r.payload})
		if (sms > 0) != r.sms || (calls > 0) != r.voice || sms > 1 || calls > 1 {
			t.Errorf("%s: %d SMS and %d calls; want sms %v, voice %v", r.name, sms, calls, r.sms, r.voice)
		}
	}
	for _, topic := range domain.AllTopics() {
		if !covered[topic] {
			t.Errorf("topic %q has no row: P15 enumerates every topic", topic)
		}
	}
}
