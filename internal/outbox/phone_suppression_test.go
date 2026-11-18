package outbox

import (
	"errors"
	"fmt"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// P2 (FR-041a): a phone delivery is subject to exactly the suppression the same alert gets on a Slack
// channel — silence, maintenance, dependency ancestors, service delegation, escalation flat-down and
// the staleness gates. Every case runs one event to a Slack channel and an sms channel and asserts the
// two get the same verdict, and that it is the verdict the case expects.
func TestPhoneSuppressionParity(t *testing.T) {
	down := func(seq int64) []byte {
		return payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: seq})
	}
	up := payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusDown, Cur: domain.StatusUp, Seq: 7})
	burn := payload(t, domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", WindowSeconds: 3600, BurnRate: 14.4, Firing: true})
	step := payload(t, domain.EscalationStepAlert{IncidentID: "inc1", MonitorID: "m1", SubjectName: "payments-api",
		Step: 0, ChannelIDs: []string{rigSlack, rigSMS}})
	onset := payload(t, domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown, Seq: 1,
		Recipients: []string{rigSlack, rigSMS}})

	silence := func(r *phoneRig) { r.w.WithSilence(func() bool { return true }) }
	maintenance := func(r *phoneRig) { r.fs.inMaintenance = map[string]bool{"m1": true} }
	ancestorDown := func(r *phoneRig) {
		m := r.fs.monitors["m1"]
		m.DependsOn = []string{"m0"}
		r.fs.monitors["m1"] = m
		r.fs.downAncestors = []store.DownAncestor{{ID: "m0", Name: "database"}}
	}
	ladder := func(r *phoneRig) {
		m := r.fs.monitors["m1"]
		m.EscalationPolicyID, m.AutoIncident = "pol1", true
		r.fs.monitors["m1"] = m
	}
	cases := []struct {
		name      string
		topic     string
		payload   []byte
		setup     func(r *phoneRig)
		delivered bool
	}{
		{"control: DOWN", domain.TopicMonitorTransition, down(7), nil, true},
		{"control: burn firing", domain.TopicSLOBurnAlert, burn, nil, true},
		{"control: escalation step", domain.TopicEscalationStep, step, nil, true},
		{"control: service onset", domain.TopicServiceAlert, onset, nil, true},

		{"silence: DOWN", domain.TopicMonitorTransition, down(7), silence, false},
		{"silence: recovery", domain.TopicMonitorTransition, up, silence, false},
		{"silence: burn firing", domain.TopicSLOBurnAlert, burn, silence, false},
		{"silence: escalation step", domain.TopicEscalationStep, step, silence, false},
		{"silence: service onset", domain.TopicServiceAlert, onset, silence, false},

		{"maintenance: DOWN", domain.TopicMonitorTransition, down(7), maintenance, false},
		{"maintenance: recovery is never suppressed", domain.TopicMonitorTransition, up, maintenance, true},

		{"dependency: DOWN", domain.TopicMonitorTransition, down(7), ancestorDown, false},
		{"dependency: burn firing", domain.TopicSLOBurnAlert, burn, ancestorDown, false},
		{"dependency: recovery is never suppressed", domain.TopicMonitorTransition, up, ancestorDown, true},
		{"dependency lookup fails open", domain.TopicMonitorTransition, down(7), func(r *phoneRig) {
			ancestorDown(r)
			r.fs.downAncErr = errors.New("db down")
		}, true},

		{"delegation: DOWN", domain.TopicMonitorTransition, down(7), func(r *phoneRig) {
			r.fs.delegation = armedFor("m1", store.DelegationLive)
		}, false},
		{"delegation: burn firing", domain.TopicSLOBurnAlert, burn, func(r *phoneRig) {
			r.fs.delegation = armedFor("m1", store.DelegationBurn)
		}, false},
		{"delegation: a monitor's escalation step", domain.TopicEscalationStep, step, func(r *phoneRig) {
			r.fs.delegation = armedFor("m1", store.DelegationLive)
		}, false},
		{"delegation: recovery is never suppressed", domain.TopicMonitorTransition, up, func(r *phoneRig) {
			r.fs.delegation = armedFor("m1", store.DelegationLive)
		}, true},
		{"delegation lookup fails open", domain.TopicMonitorTransition, down(7), func(r *phoneRig) {
			r.fs.delegationErr = errors.New("db down")
		}, true},

		{"escalation flat-down: DOWN of a monitor with a ladder", domain.TopicMonitorTransition, down(7), ladder, false},
		{"escalation flat-down: its recovery", domain.TopicMonitorTransition, up, ladder, true},

		{"superseded DOWN", domain.TopicMonitorTransition, down(5), nil, false},
		{"superseded service onset", domain.TopicServiceAlert, onset, func(r *phoneRig) {
			r.fs.alertSeq = map[string]int64{"s1|health||": 4}
		}, false},
		{"deleted monitor", domain.TopicMonitorTransition, down(7), func(r *phoneRig) { delete(r.fs.monitors, "m1") }, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newPhoneRig(t)
			if c.setup != nil {
				c.setup(r)
			}
			slack, sms, _ := r.run(domain.OutboxEvent{ID: fmt.Sprintf("e%d", i), Topic: c.topic, Payload: c.payload})
			if (slack > 0) != (sms > 0) {
				t.Fatalf("Slack posts %d, SMS %d: the two channel types got different verdicts", slack, sms)
			}
			if (sms > 0) != c.delivered {
				t.Fatalf("Slack posts %d, SMS %d; want delivered=%v", slack, sms, c.delivered)
			}
		})
	}
}
