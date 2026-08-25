package store

import (
	"context"
	"encoding/json"
	"errors"
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

// FR-023's write path, at the store. The claim is not "a column can be updated": it is that a change
// to WHO GETS WOKEN is audited with what moved, inside the mutating transaction, and that every
// refusal writes nothing at all.
func TestSetServiceEscalationPolicyIsAuditedAndRefusesCarefully(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, _ := escalatingService(t, st, ctx)
	// escalatingService already attached one; start from cleared so the first write is an ATTACH.
	if _, err := st.pool.Exec(ctx, `UPDATE services SET escalation_policy_id = NULL WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	audits := func() []string {
		t.Helper()
		rows, err := st.pool.Query(ctx,
			`SELECT target FROM audit_logs WHERE action = 'service.escalation_policy' ORDER BY created_at, id`)
		if err != nil {
			t.Fatalf("read audit: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var target string
			if err := rows.Scan(&target); err != nil {
				t.Fatalf("scan audit: %v", err)
			}
			out = append(out, target)
		}
		return out
	}
	actor := AlertActor{ViaToken: true} // a machine actor: NULL user id, token attribution

	// ATTACH — audited, naming what moved.
	got, err := st.SetServiceEscalationPolicy(ctx, f.projectID, f.serviceID, f.policyID, actor)
	if err != nil || got != f.policyID {
		t.Fatalf("attach = %q err=%v", got, err)
	}
	if a := audits(); len(a) != 1 || !strings.Contains(a[0], "none→"+f.policyID) {
		t.Fatalf("audit after attach = %v, want one row naming none→the policy — an operator reading "+
			"the log after a missed page must be able to tell attach from replace from clear", a)
	}

	// A RE-SEND of the same id is not a write, and writes no audit line either.
	if _, err := st.SetServiceEscalationPolicy(ctx, f.projectID, f.serviceID, f.policyID, actor); err != nil {
		t.Fatalf("no-op: %v", err)
	}
	if a := audits(); len(a) != 1 {
		t.Fatalf("audit rows = %d after a no-op, want 1 — the trail would otherwise claim somebody "+
			"changed the paging routing when nobody did", len(a))
	}

	// CLEAR — also a change, and it says so.
	if got, err := st.SetServiceEscalationPolicy(ctx, f.projectID, f.serviceID, "", actor); err != nil || got != "" {
		t.Fatalf("clear = %q err=%v", got, err)
	}
	if a := audits(); len(a) != 2 || !strings.Contains(a[1], f.policyID+"→none") {
		t.Fatalf("audit after clear = %v, want a second row naming the policy→none", a)
	}

	// A policy from ANOTHER project: refused by name, and nothing written.
	otherOrg, err := st.CreateOrganization(ctx, "globex-esc", "Globex Esc")
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
	if _, err := st.SetServiceEscalationPolicy(ctx, f.projectID, f.serviceID, alien.ID, actor); !errors.Is(err, ErrOwnerNotInProject) {
		t.Fatalf("foreign policy err = %v, want ErrOwnerNotInProject — the FK would refuse it too, but "+
			"as a constraint violation the API could only render as a 500", err)
	}
	if a := audits(); len(a) != 2 {
		t.Fatalf("a refused write left %d audit rows, want 2", len(a))
	}
	var stored *string
	if err := st.pool.QueryRow(ctx,
		`SELECT escalation_policy_id::text FROM services WHERE id = $1`, f.serviceID).Scan(&stored); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if stored != nil {
		t.Fatalf("a refused write stored %q", *stored)
	}

	// WRONG TENANT and a non-uuid id give the same answer, so existence never leaks.
	if _, err := st.SetServiceEscalationPolicy(ctx, otherProj.ID, f.serviceID, "", actor); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
	if _, err := st.SetServiceEscalationPolicy(ctx, f.projectID, "not-a-uuid", "", actor); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id err = %v, want ErrNotFound (not a 500 from the driver)", err)
	}

	// A FILE-MANAGED service refuses: its fields are the file's desired state, and a write here
	// would be restated by the next reconcile.
	// File ownership lives in `managed_services`, not in a column on the service — the same shape the
	// graph tests use to claim a service for a provider.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO managed_services (service_id, provider_id, org_id, project_id, source_uid)
		SELECT $1, 'file:ops.yaml', p.org_id, p.id, 'checkout' FROM projects p WHERE p.id = $2`,
		f.serviceID, f.projectID); err != nil {
		t.Fatalf("mark file-managed: %v", err)
	}
	if _, err := st.SetServiceEscalationPolicy(ctx, f.projectID, f.serviceID, f.policyID, actor); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("file-managed err = %v, want ErrServiceManagedByFile", err)
	}
	if a := audits(); len(a) != 2 {
		t.Fatalf("a refused file-managed write left %d audit rows, want 2", len(a))
	}
}

// FR-023 §8 — "retroactive escalation of incidents opened before a policy was attached" is a NON-GOAL,
// and it used to be one only in prose. The ladder read `services.escalation_policy_id` live and timed
// its steps from `incidents.started_at`, so attaching a policy to a service with an hours-old open
// incident made the next pass find every delay already elapsed.
func TestAttachingAPolicyDoesNotPageAnAlreadyOpenIncident(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: f.projectID, Type: domain.ChannelWebhook, Name: "ops-late",
		Config: map[string]string{"url": "https://hook.example/late"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}

	// The incident opens while the service has NO policy.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	inc, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout down", 2,
		governingRevision(t, st, ctx, f.serviceID))
	if err != nil || !created {
		t.Fatalf("open: %v (created=%v)", err, created)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// ...and it has been open for two hours.
	if _, err := st.pool.Exec(ctx,
		`UPDATE incidents SET started_at = now() - interval '2 hours' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("age the incident: %v", err)
	}
	setFiring(t, st, ctx, f.serviceID, true, true)

	// NOW an operator attaches a ladder whose first step fires immediately.
	policy, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: f.projectID, Name: "late-ladder", Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: ch.ID}}},
			{AfterSeconds: 300, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: ch.ID}}},
		},
	})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET escalation_policy_id = $2 WHERE id = $1`, f.serviceID, policy.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if _, err := st.AdvanceEscalations(ctx); err != nil {
		t.Fatalf("advance escalations: %v", err)
	}
	if steps := stepPayloads(t, st, ctx); len(steps) != 0 {
		t.Fatalf("the ladder paged %d step(s) for an incident that opened before the policy existed: %+v",
			len(steps), steps)
	}

	// The NEXT incident does climb it: the non-goal is about retroactivity, not about the policy.
	if _, err := st.pool.Exec(ctx, `DELETE FROM incidents WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("clear incident: %v", err)
	}
	tx2, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := st.OpenServiceIncidentTx(ctx, tx2, f.serviceID, f.projectID, "checkout down again", 2,
		governingRevision(t, st, ctx, f.serviceID)); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := st.AdvanceEscalations(ctx); err != nil {
		t.Fatalf("advance escalations again: %v", err)
	}
	if steps := stepPayloads(t, st, ctx); len(steps) != 1 {
		t.Fatalf("the next incident climbed %d step(s), want its first", len(steps))
	}
}

// The same freeze, reached by the route a policy id alone would not have closed: `escalation_policies`
// keeps its ladder in a jsonb column and has no version, so editing a policy IN PLACE would otherwise
// move the ladder under an incident already climbing it.
func TestEditingAPolicyInPlaceDoesNotMoveAnOpenIncidentsLadder(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	lf, _ := escalatingService(t, st, ctx)
	setFiring(t, st, ctx, lf.serviceID, true, true)

	other, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: lf.projectID, Type: domain.ChannelWebhook, Name: "ops-swapped",
		Config: map[string]string{"url": "https://hook.example/swapped"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("second channel: %v", err)
	}
	// Rewrite the attached policy's steps to page somebody else, entirely.
	if _, err := st.UpdateEscalationPolicy(ctx, domain.EscalationPolicy{
		ID: lf.policyID, ProjectID: lf.projectID, Name: "service-ladder",
		Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: other.ID}}},
		},
	}); err != nil {
		t.Fatalf("edit policy: %v", err)
	}

	if _, err := st.AdvanceEscalations(ctx); err != nil {
		t.Fatalf("advance escalations: %v", err)
	}
	steps := stepPayloads(t, st, ctx)
	if len(steps) != 1 {
		t.Fatalf("%d steps fired, want 1", len(steps))
	}
	for _, id := range steps[0].ChannelIDs {
		if id == other.ID {
			t.Fatalf("the open incident paged the REWRITTEN ladder's target; it must climb the one "+
				"it froze at open (channels %v)", steps[0].ChannelIDs)
		}
	}
}
