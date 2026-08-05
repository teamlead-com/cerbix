package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestAdvanceEscalations covers the on-call ladder lifecycle: step 0 fires promptly,
// later steps fire once their offset elapses, on-call schedule targets resolve to the
// current participant, the ladder latches (no repeats), and acknowledgement stops it.
func TestAdvanceEscalations(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	mkChannel := func(name string) domain.NotificationChannel {
		ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
			ProjectID: proj.ID, Type: domain.ChannelWebhook, Name: name,
			Config: map[string]string{"url": "https://example.com/" + name}, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create channel %s: %v", name, err)
		}
		return ch
	}
	primary := mkChannel("primary")
	oncallCh := mkChannel("oncall")

	sched, err := st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: proj.ID, Name: "rotation", ShiftSeconds: 604800,
		AnchorAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), Participants: []string{oncallCh.ID},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	policy, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: proj.ID, Name: "p", Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: primary.ID}}},
			{AfterSeconds: 600, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetSchedule, ID: sched.ID}}},
		},
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "payments", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, AutoIncident: true,
		EscalationPolicyID: policy.ID,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, MonitorID: mon.ID, Title: "down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "auto opened", "system")
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	count := func() int { return st.countOutbox(ctx, t, domain.TopicEscalationStep, "pending") }

	// Step 0 (after=0) fires on the first pass.
	if n, err := st.AdvanceEscalations(ctx); err != nil || n != 1 {
		t.Fatalf("step0: fired=%d err=%v (want 1)", n, err)
	}
	if count() != 1 {
		t.Fatalf("after step0 outbox=%d, want 1", count())
	}

	// Step 1 (after=600s) is not due yet → nothing new (latched).
	if n, err := st.AdvanceEscalations(ctx); err != nil || n != 0 {
		t.Fatalf("not-due: fired=%d err=%v (want 0)", n, err)
	}
	if count() != 1 {
		t.Fatalf("no new alert expected, outbox=%d", count())
	}

	// Backdate the incident start so step 1's offset has elapsed → it fires (resolving
	// the on-call schedule to the current participant).
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET started_at = now() - interval '20 minutes' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n != 1 {
		t.Fatalf("step1: fired=%d err=%v (want 1)", n, err)
	}
	if count() != 2 {
		t.Fatalf("after step1 outbox=%d, want 2", count())
	}

	// All steps done, no repeat → nothing further.
	if n, err := st.AdvanceEscalations(ctx); err != nil || n != 0 {
		t.Fatalf("exhausted: fired=%d err=%v (want 0)", n, err)
	}

	// Acknowledgement stops escalation even if we re-open the ladder by backdating.
	if _, err := st.AcknowledgeIncident(ctx, inc.ID, "alice"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET escalation_step = 0, started_at = now() - interval '1 hour' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("reset step: %v", err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n != 0 {
		t.Fatalf("acked incident must not escalate: fired=%d err=%v", n, err)
	}
	if count() != 2 {
		t.Fatalf("acked → no new alerts, outbox=%d", count())
	}
}

// TestOnCallOverrideStore covers override round-trip and that GetOnCallSchedule loads
// overrides so OnCall(now) returns the override channel during its window.
func TestOnCallOverrideStore(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mk := func(name string) domain.NotificationChannel {
		ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
			ProjectID: proj.ID, Type: domain.ChannelWebhook, Name: name,
			Config: map[string]string{"url": "https://example.com/" + name}, Enabled: true,
		})
		if err != nil {
			t.Fatalf("channel: %v", err)
		}
		return ch
	}
	primary, cover := mk("primary"), mk("cover")
	sc, err := st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: proj.ID, Name: "rot", ShiftSeconds: 604800,
		AnchorAt: time.Now().Add(-24 * time.Hour), Participants: []string{primary.ID},
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	// No override yet → rotation resolves to the sole participant.
	got, _ := st.GetOnCallSchedule(ctx, sc.ID)
	if got.OnCall(time.Now()) != primary.ID {
		t.Fatalf("no override on-call = %q, want primary", got.OnCall(time.Now()))
	}
	// Add an override covering now → cover is on call.
	ov, err := st.AddOnCallOverride(ctx, domain.OnCallOverride{
		ScheduleID: sc.ID, ChannelID: cover.ID,
		StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("add override: %v", err)
	}
	got, _ = st.GetOnCallSchedule(ctx, sc.ID)
	if got.OnCall(time.Now()) != cover.ID {
		t.Fatalf("override on-call = %q, want cover", got.OnCall(time.Now()))
	}
	if list, _ := st.ListOnCallOverrides(ctx, sc.ID); len(list) != 1 {
		t.Fatalf("list overrides = %d, want 1", len(list))
	}
	// Delete → back to rotation.
	if err := st.DeleteOnCallOverride(ctx, ov.ID); err != nil {
		t.Fatalf("delete override: %v", err)
	}
	got, _ = st.GetOnCallSchedule(ctx, sc.ID)
	if got.OnCall(time.Now()) != primary.ID {
		t.Fatalf("after delete on-call = %q, want primary", got.OnCall(time.Now()))
	}
}

// TestAdvanceEscalationsRepeatLast covers the optional repeat of the final step on the
// monitor's renotify cadence until acknowledged.
func TestAdvanceEscalationsRepeatLast(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelWebhook, Name: "c",
		Config: map[string]string{"url": "https://example.com/c"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	policy, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: proj.ID, Name: "p", RepeatLast: true, Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: ch.ID}}},
		},
	})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "m", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, AutoIncident: true,
		RenotifySeconds: 300, EscalationPolicyID: policy.ID,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, MonitorID: mon.ID, Title: "down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "auto", "system")
	if err != nil {
		t.Fatalf("incident: %v", err)
	}
	count := func() int { return st.countOutbox(ctx, t, domain.TopicEscalationStep, "pending") }

	if n, _ := st.AdvanceEscalations(ctx); n != 1 { // step0 fires
		t.Fatalf("step0 fired=%d, want 1", n)
	}
	if n, _ := st.AdvanceEscalations(ctx); n != 0 { // within renotify window → no repeat
		t.Fatalf("within window fired=%d, want 0", n)
	}
	// Backdate last_escalated_at past the renotify cadence → repeat fires once.
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET last_escalated_at = now() - interval '10 minutes' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if n, _ := st.AdvanceEscalations(ctx); n != 1 {
		t.Fatalf("repeat fired=%d, want 1", n)
	}
	if count() != 2 {
		t.Fatalf("outbox=%d, want 2", count())
	}
}
