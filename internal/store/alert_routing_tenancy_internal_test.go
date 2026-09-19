package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type alertRoutingTenantFixture struct {
	projectA  string
	projectB  string
	monitorA  domain.Monitor
	channelA  domain.NotificationChannel
	channelB  domain.NotificationChannel
	scheduleA domain.OnCallSchedule
	scheduleB domain.OnCallSchedule
	policyA   domain.EscalationPolicy
}

func seedAlertRoutingTenantFixture(t *testing.T, st *Store, ctx context.Context) alertRoutingTenantFixture {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "routing-tenant", "Routing tenant")
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	projectA, err := st.CreateProject(ctx, org.ID, "routing-a", "Routing A")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}
	projectB, err := st.CreateProject(ctx, org.ID, "routing-b", "Routing B")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}
	channelA, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: projectA.ID,
		Type:      domain.ChannelWebhook,
		Name:      "A",
		Config:    map[string]string{"url": "https://a.example.test/hook"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create channel A: %v", err)
	}
	channelB, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: projectB.ID,
		Type:      domain.ChannelWebhook,
		Name:      "B",
		Config:    map[string]string{"url": "https://b.example.test/hook"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create channel B: %v", err)
	}
	monitorA, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID:       projectA.ID,
		Name:            "routing-a",
		Type:            domain.MonitorHTTP,
		Target:          "https://a.example.test/healthz",
		IntervalSeconds: 60,
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("create monitor A: %v", err)
	}
	anchor := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	scheduleA, err := st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: projectA.ID, Name: "A", ShiftSeconds: 3600, AnchorAt: anchor,
		Participants: []string{channelA.ID},
	})
	if err != nil {
		t.Fatalf("create schedule A: %v", err)
	}
	scheduleB, err := st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: projectB.ID, Name: "B", ShiftSeconds: 3600, AnchorAt: anchor,
		Participants: []string{channelB.ID},
	})
	if err != nil {
		t.Fatalf("create schedule B: %v", err)
	}
	policyA, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: projectA.ID,
		Name:      "A",
		Steps: []domain.EscalationStep{{
			Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: channelA.ID}},
		}},
	})
	if err != nil {
		t.Fatalf("create policy A: %v", err)
	}
	return alertRoutingTenantFixture{
		projectA: projectA.ID, projectB: projectB.ID, monitorA: monitorA,
		channelA: channelA, channelB: channelB, scheduleA: scheduleA, scheduleB: scheduleB, policyA: policyA,
	}
}

func TestAlertRoutingStoreRejectsForeignProjectReferences(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	fixture := seedAlertRoutingTenantFixture(t, st, ctx)

	if err := st.LinkMonitorChannel(ctx, fixture.monitorA.ID, fixture.channelB.ID); !errors.Is(err, ErrRoutingReferenceNotInProject) {
		t.Fatalf("link foreign channel error = %v, want ErrRoutingReferenceNotInProject", err)
	}

	foreignTargets := []domain.EscalationStep{{Targets: []domain.EscalationTarget{
		{Type: domain.EscalationTargetChannel, ID: fixture.channelB.ID},
		{Type: domain.EscalationTargetSchedule, ID: fixture.scheduleB.ID},
	}}}
	if _, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: fixture.projectA, Name: "foreign", Steps: foreignTargets,
	}); !errors.Is(err, ErrRoutingReferenceNotInProject) {
		t.Fatalf("create foreign policy error = %v, want ErrRoutingReferenceNotInProject", err)
	}

	changedPolicy := fixture.policyA
	changedPolicy.Steps = foreignTargets
	if _, err := st.UpdateEscalationPolicy(ctx, changedPolicy); !errors.Is(err, ErrRoutingReferenceNotInProject) {
		t.Fatalf("update foreign policy error = %v, want ErrRoutingReferenceNotInProject", err)
	}
	storedPolicy, err := st.GetEscalationPolicy(ctx, fixture.policyA.ID)
	if err != nil {
		t.Fatalf("reload policy: %v", err)
	}
	if storedPolicy.Steps[0].Targets[0].ID != fixture.channelA.ID {
		t.Fatalf("rejected policy update changed stored target to %q", storedPolicy.Steps[0].Targets[0].ID)
	}

	changedSchedule := fixture.scheduleA
	changedSchedule.Participants = []string{fixture.channelB.ID}
	if _, err := st.UpdateOnCallSchedule(ctx, changedSchedule); !errors.Is(err, ErrRoutingReferenceNotInProject) {
		t.Fatalf("update foreign schedule error = %v, want ErrRoutingReferenceNotInProject", err)
	}
	storedSchedule, err := st.GetOnCallSchedule(ctx, fixture.scheduleA.ID)
	if err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if storedSchedule.Participants[0] != fixture.channelA.ID {
		t.Fatalf("rejected schedule update changed stored participant to %q", storedSchedule.Participants[0])
	}

	if _, err := st.AddOnCallOverride(ctx, domain.OnCallOverride{
		ScheduleID: fixture.scheduleA.ID,
		ChannelID:  fixture.channelB.ID,
		StartsAt:   time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC),
		EndsAt:     time.Date(2026, time.September, 20, 11, 0, 0, 0, time.UTC),
	}); !errors.Is(err, ErrRoutingReferenceNotInProject) {
		t.Fatalf("add foreign override error = %v, want ErrRoutingReferenceNotInProject", err)
	}
}

func TestAlertRoutingSchemaRejectsDirectForeignProjectWrites(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	fixture := seedAlertRoutingTenantFixture(t, st, ctx)

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO monitor_notifications (monitor_id, channel_id, project_id) VALUES ($1,$2,$3)`,
		fixture.monitorA.ID, fixture.channelB.ID, fixture.projectA); err == nil {
		t.Fatal("direct SQL linked a monitor to another project's channel")
	}

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO oncall_overrides (schedule_id, channel_id, project_id, starts_at, ends_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		fixture.scheduleA.ID, fixture.channelB.ID, fixture.projectA,
		time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC),
		time.Date(2026, time.September, 20, 11, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("direct SQL added an override from another project's channel")
	}

	foreignSteps, err := json.Marshal([]domain.EscalationStep{{Targets: []domain.EscalationTarget{{
		Type: domain.EscalationTargetChannel, ID: fixture.channelB.ID,
	}}}})
	if err != nil {
		t.Fatalf("marshal foreign steps: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO escalation_policies (project_id, name, repeat_last, steps) VALUES ($1,'foreign',false,$2)`,
		fixture.projectA, foreignSteps); err == nil {
		t.Fatal("direct SQL inserted an escalation target from another project")
	}

	foreignParticipants, err := json.Marshal([]string{fixture.channelB.ID})
	if err != nil {
		t.Fatalf("marshal foreign participants: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE oncall_schedules SET participants = $2 WHERE id = $1`,
		fixture.scheduleA.ID, foreignParticipants); err == nil {
		t.Fatal("direct SQL updated a schedule with another project's participant")
	}
}

func TestEscalationSnapshotRejectsForeignProjectTargets(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	fixture, incident := escalatingService(t, st, ctx)
	otherProject, err := st.CreateProject(ctx, fixture.orgID, "snapshot-other", "Snapshot other")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	foreignChannel, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: otherProject.ID,
		Type:      domain.ChannelWebhook,
		Name:      "snapshot-foreign",
		Config:    map[string]string{"url": "https://snapshot.example.test/hook"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create foreign channel: %v", err)
	}
	steps, err := json.Marshal([]domain.EscalationStep{{Targets: []domain.EscalationTarget{{
		Type: domain.EscalationTargetChannel, ID: foreignChannel.ID,
	}}}})
	if err != nil {
		t.Fatalf("marshal snapshot steps: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE incident_escalation_snapshots SET steps = $2 WHERE incident_id = $1`,
		incident.ID, steps); err == nil {
		t.Fatal("direct SQL moved a frozen escalation snapshot to another project's channel")
	}
}
