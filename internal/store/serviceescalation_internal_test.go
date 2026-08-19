package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-023: a service escalates its OWN auto-opened incident. These tests are about the PREDICATES —
// when a step may advance and what happens when the answer cannot be determined — because that is
// where everything dangerous about this requirement lives (spec §1).

type ladderFixture struct {
	armFixture
	policyID  string
	channelID string
}

// escalatingService builds a service that is armed, owns paging, has a policy whose first step fires
// immediately, and holds an OPEN auto-incident. It does NOT make the service firing: each test says
// so itself, because "is it still firing" is the predicate under test.
func escalatingService(t *testing.T, st *Store, ctx context.Context) (ladderFixture, domain.Incident) {
	t.Helper()
	f := armedService(t, st, ctx)
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: f.projectID, Type: domain.ChannelWebhook, Name: "ops-ladder",
		Config: map[string]string{"url": "https://hook.example/ladder"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	policy, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: f.projectID, Name: "service-ladder", Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: ch.ID}}},
		},
	})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET escalation_policy_id = $2 WHERE id = $1`, f.serviceID, policy.ID); err != nil {
		t.Fatalf("attach policy: %v", err)
	}
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, ServiceID: f.serviceID, Title: "Checkout — service down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "opened automatically", "system")
	if err != nil {
		t.Fatalf("service incident: %v", err)
	}
	return ladderFixture{armFixture: f, policyID: policy.ID, channelID: ch.ID}, inc
}

// setFiring drives the LIVE verdict the ladder reads: whether the service is still in a pageable
// state, and whether that answer is fresh.
func setFiring(t *testing.T, st *Store, ctx context.Context, serviceID string, firing bool, leaseFresh bool) {
	t.Helper()
	lease := "now() + interval '90 seconds'"
	if !leaseFresh {
		lease = "now() - interval '1 second'"
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_alert_state SET live_firing = $2, observed_state = $3, candidate_state = $3,
		        evaluated_at = now(), lease_until = `+lease+` WHERE service_id = $1`,
		serviceID, firing, map[bool]string{true: "down", false: "healthy"}[firing]); err != nil {
		t.Fatalf("set firing: %v", err)
	}
}

func stepPayloads(t *testing.T, st *Store, ctx context.Context) []domain.EscalationStepAlert {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT payload FROM outbox_events WHERE topic = $1 ORDER BY created_at, id`,
		domain.TopicEscalationStep)
	if err != nil {
		t.Fatalf("read steps: %v", err)
	}
	defer rows.Close()
	var out []domain.EscalationStepAlert
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan step: %v", err)
		}
		var a domain.EscalationStepAlert
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decode step: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// The happy path and the two ways it ends: a step fires for the SERVICE, names it, latches its
// progress on the incident — and acknowledgement stops the ladder, exactly as it does for a monitor,
// because FR-022 put the ladder's state on the same row (spec D2/D4).
func TestAServiceLadderFiresNamesTheServiceAndStopsOnAck(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, inc := escalatingService(t, st, ctx)
	setFiring(t, st, ctx, f.serviceID, true, true)

	n, err := st.AdvanceEscalations(ctx)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if n.ServiceSteps != 1 || n.MonitorSteps != 0 {
		t.Fatalf("fired = %d, want 1 — a service with a policy, owning paging, firing and fresh must "+
			"escalate its own incident (FR-023 invariant 1), and it must be counted as a SERVICE step", n)
	}
	steps := stepPayloads(t, st, ctx)
	if len(steps) != 1 {
		t.Fatalf("%d step events, want 1", len(steps))
	}
	s := steps[0]
	switch {
	case s.ServiceID != f.serviceID:
		t.Errorf("step names service %q, want %q — the worker branches on this to skip delegation", s.ServiceID, f.serviceID)
	case s.MonitorID != "":
		t.Errorf("a SERVICE step carries monitor id %q: the anchors are exclusive", s.MonitorID)
	case s.SubjectName != "Checkout":
		t.Errorf("subject_name = %q, want the service's name", s.SubjectName)
	case s.MonitorName != "Checkout":
		t.Errorf("legacy monitor_name = %q, want the SUBJECT's name so a pre-FR-023 worker renders a "+
			"sentence instead of \" is DOWN\" (spec D6)", s.MonitorName)
	case len(s.ChannelIDs) != 1 || s.ChannelIDs[0] != f.channelID:
		t.Errorf("channels = %v, want the policy's own channel", s.ChannelIDs)
	}
	// What a PRE-FR-023 worker renders, asserted through the legacy field ALONE (invariant 9).
	legacy := domain.EscalationStepAlert{MonitorName: s.MonitorName, Step: s.Step}
	if got := legacy.Message(); got == "" || !strings.Contains(got, "Checkout is DOWN") {
		t.Errorf("a pre-FR-023 worker would render %q — the legacy field must carry the subject", got)
	}

	// Progress is latched on the INCIDENT, so a second pass fires nothing.
	after, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if after.EscalationStep != 1 || after.LastEscalatedAt == nil {
		t.Fatalf("progress = step %d / last %v, want the step latched on the incident",
			after.EscalationStep, after.LastEscalatedAt)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
		t.Fatalf("second pass fired=%d err=%v, want 0 — the ladder is a latch, not a repeat", n, err)
	}

	// Acknowledgement ends it: the incident leaves the candidate set through the same predicate a
	// monitor's does.
	if _, err := st.AcknowledgeIncident(ctx, inc.ID, "u1"); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET escalation_step = 0 WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("rewind progress: %v", err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
		t.Fatalf("acknowledged incident fired=%+v err=%v, want 0 even with its progress rewound", n, err)
	}

	// RESOLUTION ends it too, and by a different predicate than the acknowledgement — worth its own
	// assertion because an incident can be resolved WITHOUT ever being acknowledged, which is the
	// ordinary case for an outage a machine opened and a machine closed.
	if _, err := st.pool.Exec(ctx,
		`UPDATE incidents SET acknowledged_at = NULL, acknowledged_by = NULL, status = 'resolved',
		        resolved_at = now(), escalation_step = 0 WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
		t.Fatalf("resolved incident fired=%+v err=%v, want 0", n, err)
	}
}

// Invariant 12: the policy is tenant-scoped, and the guarantee is the SCHEMA's rather than the
// query's. Proven by DIRECT SQL, because a test that goes through the store would only prove the
// store's own check — and the composite FK (00069) exists precisely because that check is one bug
// away from being bypassed.
func TestAServiceCannotBorrowAnotherProjectsEscalationPolicy(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, _ := escalatingService(t, st, ctx)

	// A slug nobody else uses: the package's tests share one database, and "globex" is already taken
	// by fileapply_services_internal_test.go — a collision there fails THAT test, not this one, which
	// is the worst way to learn about it.
	otherOrg, err := st.CreateOrganization(ctx, "globex-ladder", "Globex Ladder")
	if err != nil {
		t.Fatalf("other org: %v", err)
	}
	otherProj, err := st.CreateProject(ctx, otherOrg.ID, "other", "Other")
	if err != nil {
		t.Fatalf("other project: %v", err)
	}
	alien, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: otherProj.ID, Name: "theirs", Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: f.channelID}}},
		},
	})
	if err != nil {
		t.Fatalf("alien policy: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET escalation_policy_id = $2 WHERE id = $1`, f.serviceID, alien.ID); err == nil {
		t.Fatal("a service took another project's escalation policy — routing operational response " +
			"across a tenant boundary is the one place in this feature where being wrong pages the " +
			"wrong humans (migration 00069's composite FK)")
	}
}

// D3, the fail-closed rule, in all three shapes ambiguity takes. Each sub-case rewinds nothing and
// asserts NOTHING fires — and the first sub-case proves the fixture can fire at all, so a silent
// break in the setup cannot make the rest of this test pass for the wrong reason.
func TestAServiceLadderFailsClosedOnAnythingItCannotConfirm(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, _ := escalatingService(t, st, ctx)

	// premise: firing and fresh DOES fire
	setFiring(t, st, ctx, f.serviceID, true, true)
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 1 {
		t.Fatalf("premise: fired=%d err=%v, want 1 — the rest of this test asserts absence and needs "+
			"a fixture that is capable of presence", n, err)
	}
	reset := func() {
		t.Helper()
		if _, err := st.pool.Exec(ctx,
			`UPDATE incidents SET escalation_step = 0, last_escalated_at = NULL WHERE service_id = $1`,
			f.serviceID); err != nil {
			t.Fatalf("rewind: %v", err)
		}
	}

	t.Run("the verdict is STALE, and RESUMES when it is refreshed", func(t *testing.T) {
		reset()
		setFiring(t, st, ctx, f.serviceID, true, false) // still firing, lease expired
		if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
			t.Fatalf("fired=%+v err=%v — a ladder must not page more people on a verdict nobody is "+
				"refreshing; ambiguity here would MULTIPLY a page (spec D3)", n, err)
		}
		// Fail-closed is a PAUSE, not a termination: an evaluator that catches up says the service is
		// still in trouble, and the ladder must pick up where it stopped. A rule that silently ended
		// the ladder on one stale tick would lose the rest of the escalation for that outage.
		setFiring(t, st, ctx, f.serviceID, true, true)
		if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 1 {
			t.Fatalf("fired=%+v err=%v after the verdict was refreshed, want 1 — fail-closed must pause "+
				"the ladder, not end it", n, err)
		}
	})

	t.Run("the verdict cannot be READ at all", func(t *testing.T) {
		reset()
		setFiring(t, st, ctx, f.serviceID, true, true)
		// Direct-SQL fault injection: the table the predicate reads is gone for the duration of one
		// pass. The pass must ERROR and fire nothing — "no step, and no progress written" is the claim,
		// and an error that fired half a ladder first would satisfy neither half of it.
		if _, err := st.pool.Exec(ctx, `ALTER TABLE service_alert_state RENAME TO service_alert_state_hidden`); err != nil {
			t.Fatalf("hide the table: %v", err)
		}
		n, err := st.AdvanceEscalations(ctx)
		if _, rerr := st.pool.Exec(ctx, `ALTER TABLE service_alert_state_hidden RENAME TO service_alert_state`); rerr != nil {
			t.Fatalf("restore the table: %v", rerr)
		}
		if err == nil {
			t.Fatalf("a pass that could not read the verdict reported success (%+v) — a ladder must not "+
				"treat an unreadable state as permission to page", n)
		}
		if n.Total() != 0 {
			t.Fatalf("a failed pass still fired %+v steps", n)
		}
		var step int
		if qerr := st.pool.QueryRow(ctx,
			`SELECT escalation_step FROM incidents WHERE service_id = $1`, f.serviceID).Scan(&step); qerr != nil {
			t.Fatalf("read progress: %v", qerr)
		}
		if step != 0 {
			t.Fatalf("progress advanced to %d inside a pass that failed — the whole pass is one "+
				"transaction precisely so that cannot happen", step)
		}
	})

	t.Run("the service stopped OWNING paging", func(t *testing.T) {
		reset()
		setFiring(t, st, ctx, f.serviceID, true, true)
		if _, err := st.pool.Exec(ctx, `UPDATE services SET owns_paging = false WHERE id = $1`, f.serviceID); err != nil {
			t.Fatalf("disown: %v", err)
		}
		if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
			t.Fatalf("fired=%d err=%v — a service that is not the thing that pages escalates nothing", n, err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE services SET owns_paging = true WHERE id = $1`, f.serviceID); err != nil {
			t.Fatalf("restore: %v", err)
		}
	})

	t.Run("there is NO verdict at all", func(t *testing.T) {
		reset()
		if _, err := st.pool.Exec(ctx, `DELETE FROM service_alert_state WHERE service_id = $1`, f.serviceID); err != nil {
			t.Fatalf("delete state: %v", err)
		}
		if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
			t.Fatalf("fired=%d err=%v — no evaluation has ever concluded, so nothing may page on its behalf", n, err)
		}
	})

	t.Run("the service is no longer FIRING", func(t *testing.T) {
		reset()
		armLive(t, st, ctx, f.armFixture) // recreates the row, healthy
		setFiring(t, st, ctx, f.serviceID, false, true)
		if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
			t.Fatalf("fired=%d err=%v — a recovered service escalates nothing", n, err)
		}
	})
}

// Invariants 1 and 2 from the other side: what must NEVER escalate. A human's incident and a service
// with no policy both look exactly like a candidate except for one predicate each.
func TestAServiceLadderIgnoresAHumansIncidentAndAPolicylessService(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, inc := escalatingService(t, st, ctx)
	setFiring(t, st, ctx, f.serviceID, true, true)

	// (1) a HUMAN opened it: source = 'manual'
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET source = 'manual' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("make manual: %v", err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
		t.Fatalf("fired=%d err=%v — a machine does not escalate a conclusion a person opened "+
			"(FR-023 invariant 1, the rule FR-022's D1b established)", n, err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET source = 'auto' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("restore source: %v", err)
	}

	// (2) no policy attached
	if _, err := st.pool.Exec(ctx, `UPDATE services SET escalation_policy_id = NULL WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("detach policy: %v", err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
		t.Fatalf("fired=%d err=%v — a service with no policy escalates nothing", n, err)
	}

	// (3) the SERVICE IS DELETED mid-outage (invariant 13). FR-022 keeps the incident and clears its
	// anchor, so the row is still open and unacknowledged — exactly the shape that would escalate if
	// anything escalated on a NULL anchor. Nothing may: there is no service to name, no policy to
	// read and no verdict to confirm.
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET escalation_policy_id = $2 WHERE id = $1`, f.serviceID, f.policyID); err != nil {
		t.Fatalf("reattach policy: %v", err)
	}
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	orphan, err := st.GetIncident(ctx, inc.ID)
	if err != nil || orphan.ServiceID != "" || orphan.Status == domain.IncidentResolved {
		t.Fatalf("premise: incident after the delete = %+v (err %v), want it OPEN with a cleared anchor",
			orphan, err)
	}
	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 0 {
		t.Fatalf("fired=%d err=%v — an incident whose subject is gone must not escalate on a NULL "+
			"anchor (FR-023 invariant 13)", n, err)
	}
}

// Invariant 7: the SERVICE GRAPH does not pause the ladder. This is a NEGATIVE invariant chosen
// against the obvious symmetry with the monitor dependency pause, because §14 says the impact graph
// "annotates and links; never suppresses, merges or hides" — so a graph sold as advisory must not
// become a suppression mechanism because this feature found it convenient (spec D5).
func TestTheServiceGraphDoesNotPauseAServiceLadder(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, _ := escalatingService(t, st, ctx)
	setFiring(t, st, ctx, f.serviceID, true, true)

	// An UPSTREAM service with its own open auto-incident: the monitor ladder's analogue of this
	// (a transitive parent down) is exactly what pauses a monitor.
	upstream, err := st.CreateService(ctx, domain.Service{
		ProjectID: f.projectID, Slug: "payments", Name: "Payments",
	})
	if err != nil {
		t.Fatalf("upstream service: %v", err)
	}
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.serviceID,
		[]string{upstream.ID}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("edge: %v", err)
	}
	if _, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, ServiceID: upstream.ID, Title: "Payments — service down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "opened automatically", "system"); err != nil {
		t.Fatalf("upstream incident: %v", err)
	}

	if n, err := st.AdvanceEscalations(ctx); err != nil || n.ServiceSteps != 1 {
		t.Fatalf("fired=%d err=%v, want 1 — an upstream service's incident must change NOTHING about "+
			"this ladder (FR-023 invariant 7). If a pause is wanted, it changes what §14 IS.", n, err)
	}
}
