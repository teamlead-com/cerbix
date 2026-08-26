package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.1 — the arming rules, each of which exists because its absence loses a page. Every test
// here starts from a FULLY ARMED service and removes exactly one clause, because "it suppresses" and
// "it suppresses for the right reasons" are different claims and only the second one is worth having.

type armFixture struct {
	orgID, projectID string
	serviceID        string
	monitorID        string
	targetID         string
}

// armedService builds a service that ACTIVELY covers both signals for one monitor: owning, with a
// pageable policy, an effective declaration naming the monitor as an SLI, a fresh error-free
// evaluation of the current generation, a quotable burn verdict, and a route.
func armedService(t *testing.T, st *Store, ctx context.Context) armFixture {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "checkout-http", Type: domain.MonitorHTTP,
		Target: "https://checkout.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	svc, err := st.CreateService(ctx, domain.Service{
		ProjectID: proj.ID, Slug: "checkout", Name: "Checkout",
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, proj.ID, svc.ID, domain.ServiceDeclaration{
		Monitors: []string{mon.ID}, SLI: []string{mon.ID},
	}, 0, DeclarationOptions{CreatedBy: "test"}); err != nil {
		t.Fatalf("declaration: %v", err)
	}
	// A declaration and its EPOCH both take effect at the next bucket boundary, which §16.2 makes
	// load-bearing. Backdating both puts the fixture in the state the arming question presupposes:
	// the revision is what delegation reads, and the epoch is what the EVALUATOR reads, so moving
	// only one produced a service that armed but could never be evaluated.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_definition_revisions SET effective_at = now() - interval '1 hour'
		 WHERE service_id = $1`, svc.ID); err != nil {
		t.Fatalf("backdate revision: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_evaluation_epochs SET effective_at = now() - interval '1 hour'
		 WHERE service_id = $1`, svc.ID); err != nil {
		t.Fatalf("backdate epoch: %v", err)
	}

	f := armFixture{orgID: org.ID, projectID: proj.ID, serviceID: svc.ID, monitorID: mon.ID}

	// Ownership + a route (an enabled project channel).
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET owns_paging = true WHERE id = $1`, svc.ID); err != nil {
		t.Fatalf("own paging: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO notification_channels (project_id, type, name, config, enabled)
		VALUES ($1,'webhook','ops','{"url":"https://hook.example/x"}',true)`, proj.ID); err != nil {
		t.Fatalf("channel: %v", err)
	}
	armLive(t, st, ctx, f)

	// A service-scoped burn target with a quotable verdict.
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1,'30d',99.9,true,'[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14,"severity":"page"}]')
		RETURNING id`, svc.ID).Scan(&f.targetID); err != nil {
		t.Fatalf("target: %v", err)
	}
	armBurn(t, st, ctx, f, "clear")
	return f
}

func armLive(t *testing.T, st *Store, ctx context.Context, f armFixture) {
	t.Helper()
	// The fixture stands in for a SUCCESSFUL evaluation, so it stamps what an evaluation stamps:
	// the declaration that governs now. Leaving revision_id NULL would arm a service whose verdict
	// came from no declaration at all (§16.1).
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_alert_state
		  (service_id, project_id, observed_state, candidate_state, streak, live_firing,
		   config_generation, revision_id, evaluated_at, lease_until)
		SELECT s.id, s.project_id, 'healthy', 'healthy', 3, false, s.alert_config_generation,
		       (SELECT r.id FROM service_definition_revisions r
		         WHERE r.service_id = s.id AND r.state = 'effective' AND r.effective_at <= now()
		         ORDER BY r.effective_at DESC, r.revision DESC LIMIT 1),
		       now(), now() + interval '90 seconds'
		  FROM services s WHERE s.id = $1
		ON CONFLICT (service_id) DO UPDATE SET
		   config_generation = EXCLUDED.config_generation,
		   revision_id = EXCLUDED.revision_id,
		   evaluated_at = EXCLUDED.evaluated_at, lease_until = EXCLUDED.lease_until,
		   last_error = NULL`, f.serviceID); err != nil {
		t.Fatalf("arm live: %v", err)
	}
}

func armBurn(t *testing.T, st *Store, ctx context.Context, f armFixture, verdict string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict,
		   target_generation, config_generation, evaluated_at, lease_until)
		SELECT s.id, s.project_id, t.id, 'rule-1', false, $3, t.alert_generation,
		       s.alert_config_generation, now(), now() + interval '90 seconds'
		  FROM services s JOIN sla_targets t ON t.service_id = s.id
		 WHERE s.id = $1 AND t.id = $2
		ON CONFLICT (service_id, project_id, sla_target_id, rule_key) DO UPDATE SET
		   last_verdict = EXCLUDED.last_verdict, target_generation = EXCLUDED.target_generation,
		   config_generation = EXCLUDED.config_generation, evaluated_at = EXCLUDED.evaluated_at,
		   lease_until = EXCLUDED.lease_until, last_error = NULL`,
		f.serviceID, f.targetID, verdict); err != nil {
		t.Fatalf("arm burn: %v", err)
	}
}

func delegated(t *testing.T, st *Store, ctx context.Context, f armFixture, sig DelegationSignal) bool {
	t.Helper()
	v, err := st.ActiveDelegation(ctx, f.monitorID, f.projectID, sig)
	if err != nil {
		t.Fatalf("delegation lookup (%s): %v", sig, err)
	}
	if !v.Suppress() && v.FailOpenReason == "" {
		t.Fatalf("%s: not suppressing and no reason given — silence must always have a name", sig)
	}
	return v.Suppress()
}

// The baseline: fully armed, both signals suppress, and the owner is named.
func TestActiveDelegationArmed(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	for _, sig := range []DelegationSignal{DelegationLive, DelegationBurn} {
		v, err := st.ActiveDelegation(ctx, f.monitorID, f.projectID, sig)
		if err != nil {
			t.Fatalf("%s: %v", sig, err)
		}
		if !v.Suppress() {
			t.Fatalf("%s: a fully armed service does not suppress (%s)", sig, v.FailOpenReason)
		}
		if len(v.Owners) != 1 || v.Owners[0].Name != "Checkout" {
			t.Fatalf("%s: owners = %+v, want the covering service named", sig, v.Owners)
		}
	}
}

// [326] P0-1 — a HOLD is a SUCCESSFUL evaluation that cannot fire. Arming on "successful" would mute
// a member's real burn alert behind a replacement structurally unable to speak.
func TestHeldBurnVerdictDisarmsBurnOnly(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	armBurn(t, st, ctx, f, "hold")

	if delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("a HELD rule armed burn coverage: the member's burn alert would be silenced by a " +
			"replacement that cannot fire")
	}
	// ...and it must not touch the live signal, which is a different replacement entirely.
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a held BURN verdict dis-armed the LIVE signal too")
	}
}

// [326] P0-3 — an unroutable service satisfies every other clause and delivers nothing.
func TestUnroutableServiceDisarmsBothSignals(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// Disable the only channel AFTER arming: nothing on the service changes, which is precisely why
	// routability cannot be cached in a generation.
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	for _, sig := range []DelegationSignal{DelegationLive, DelegationBurn} {
		if delegated(t, st, ctx, f, sig) {
			t.Fatalf("%s: a service with nobody to notify still silenced the monitor", sig)
		}
	}
	// An on-call schedule restores the route. Its participants are CHANNEL IDS (see
	// `api/tenant_scope.go`), and this fixture used to hold `[{"kind":"email",...}]` — a shape
	// nothing resolves, which armed coverage only because the old predicate counted array entries
	// without looking at them. A schedule that names a live channel is the real restoration.
	var liveChannel string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO notification_channels (project_id, type, name, config, enabled)
		VALUES ($1,'webhook','oncall','{"url":"https://hook.example/oncall"}',true)
		RETURNING id::text`, f.projectID).Scan(&liveChannel); err != nil {
		t.Fatalf("on-call channel: %v", err)
	}
	var schedule string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at, participants)
		VALUES ($1,'primary',604800,now(),jsonb_build_array($2::text))
		RETURNING id`, f.projectID, liveChannel).Scan(&schedule); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET oncall_schedule_id = $2 WHERE id = $1`, f.serviceID, schedule); err != nil {
		t.Fatalf("attach schedule: %v", err)
	}
	// Attaching the schedule bumped nothing on the alert config — the generation trigger only watches
	// paging fields — so live state is still current and coverage returns on its own.
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a restored route did not re-arm live coverage")
	}
}

// Freshness, generation and error, one clause at a time.
func TestDelegationDisarmsOnStaleGenerationAndError(t *testing.T) {
	st, ctx := serviceSchemaStore(t)

	t.Run("stale lease", func(t *testing.T) {
		f := armedService(t, st, ctx)
		if _, err := st.pool.Exec(ctx,
			`UPDATE service_alert_state SET lease_until = now() - interval '1 second'`); err != nil {
			t.Fatalf("expire: %v", err)
		}
		if delegated(t, st, ctx, f, DelegationLive) {
			t.Fatal("an expired lease still suppressed: a stalled evaluator would silence forever")
		}
	})

	t.Run("generation mismatch", func(t *testing.T) {
		st, ctx := serviceSchemaStore(t)
		f := armedService(t, st, ctx)
		// A policy edit bumps the generation; the existing evaluation is now about a configuration
		// nobody is running.
		if _, err := st.pool.Exec(ctx,
			`UPDATE services SET page_on = '{down,degraded}' WHERE id = $1`, f.serviceID); err != nil {
			t.Fatalf("edit policy: %v", err)
		}
		if delegated(t, st, ctx, f, DelegationLive) {
			t.Fatal("a verdict for a superseded configuration still suppressed")
		}
		armLive(t, st, ctx, f) // re-evaluated under the new generation
		if !delegated(t, st, ctx, f, DelegationLive) {
			t.Fatal("re-evaluating the new generation did not re-arm")
		}
	})

	t.Run("evaluation error", func(t *testing.T) {
		st, ctx := serviceSchemaStore(t)
		f := armedService(t, st, ctx)
		if _, err := st.pool.Exec(ctx,
			`UPDATE service_alert_state SET last_error = 'boom'`); err != nil {
			t.Fatalf("set error: %v", err)
		}
		if delegated(t, st, ctx, f, DelegationLive) {
			t.Fatal("an errored evaluation still suppressed")
		}
	})

	t.Run("no live state at all", func(t *testing.T) {
		st, ctx := serviceSchemaStore(t)
		f := armedService(t, st, ctx)
		if _, err := st.pool.Exec(ctx, `DELETE FROM service_alert_state`); err != nil {
			t.Fatalf("delete state: %v", err)
		}
		if delegated(t, st, ctx, f, DelegationLive) {
			t.Fatal("a service that has never been evaluated suppressed anyway")
		}
	})

	t.Run("policy that pages nothing", func(t *testing.T) {
		st, ctx := serviceSchemaStore(t)
		f := armedService(t, st, ctx)
		if _, err := st.pool.Exec(ctx,
			`UPDATE services SET page_on = '{}', page_on_unknown = false WHERE id = $1`, f.serviceID); err != nil {
			t.Fatalf("empty policy: %v", err)
		}
		armLive(t, st, ctx, f) // fresh evaluation of the NEW generation, so only the policy is at fault
		if delegated(t, st, ctx, f, DelegationLive) {
			t.Fatal("a service that pages for no state still silenced the monitor")
		}
	})
}

// [326] P1-1 — membership is the EFFECTIVE revision, never the authored refs.
func TestDelegationUsesTheEffectiveRevision(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// A second monitor, added to the SLI by a declaration that is not yet effective.
	newMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "checkout-db", Type: domain.MonitorHTTP,
		Target: "https://db.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.monitorID, newMon.ID}, SLI: []string{f.monitorID, newMon.ID},
	}, 1, DeclarationOptions{CreatedBy: "test"}); err != nil {
		t.Fatalf("second declaration: %v", err)
	}
	// The refs already contain the new monitor; the revision is future-effective.
	var inRefs bool
	if err := st.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM service_member_refs
		                WHERE service_id = $1 AND monitor_id = $2 AND role = 'sli')`,
		f.serviceID, newMon.ID).Scan(&inRefs); err != nil {
		t.Fatalf("refs: %v", err)
	}
	if !inRefs {
		t.Fatal("the fixture is wrong: the authored refs should already name the new monitor")
	}

	armLive(t, st, ctx, f)
	future := armFixture{projectID: f.projectID, monitorID: newMon.ID}
	if delegated(t, st, ctx, future, DelegationLive) {
		t.Fatal("a monitor added by a FUTURE-EFFECTIVE declaration was already suppressed — the " +
			"service cannot replace a page for a member it is not yet measuring")
	}
	// Once revision 2 is effective, the same monitor IS covered. Only the NEW revision moves:
	// revisions are unique per effective boundary, and backdating both would collide.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_definition_revisions SET effective_at = now() - interval '1 minute'
		 WHERE service_id = $1 AND revision = 2`, f.serviceID); err != nil {
		t.Fatalf("make effective: %v", err)
	}
	// Crossing the boundary is NOT coverage: the only successful evaluation on record was computed
	// from revision 1, which never looked at this monitor. Arming requires an evaluation OF the
	// revision that governs now (§16.1), so the member keeps paging for itself until then.
	if delegated(t, st, ctx, future, DelegationLive) {
		t.Fatal("a member of the NEW revision was suppressed by a verdict computed from the OLD " +
			"one — the service cannot replace a page it has never evaluated")
	}
	armLive(t, st, ctx, f) // re-evaluated under revision 2
	if !delegated(t, st, ctx, future, DelegationLive) {
		t.Fatal("re-evaluating under the effective revision did not re-arm")
	}
}

// The revision half of the arming conjunction, stated on its own: an evaluation is coverage only for
// the declaration it was computed from. Membership is not enough — the original member is an SLI of
// BOTH revisions here, so only the stamp can tell the two verdicts apart.
func TestRevisionChangeDisarmsUntilReevaluated(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	armLive(t, st, ctx, f)
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("the fixture is wrong: an evaluated, routable owner should be armed")
	}

	// A new declaration keeping the SAME SLI set: membership cannot distinguish the revisions, so a
	// stamp is the only thing that can.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.monitorID}, SLI: []string{f.monitorID},
	}, 1, DeclarationOptions{CreatedBy: "test"}); err != nil {
		t.Fatalf("second declaration: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_definition_revisions SET effective_at = now() - interval '1 minute'
		 WHERE service_id = $1 AND revision = 2`, f.serviceID); err != nil {
		t.Fatalf("make effective: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a verdict computed from the superseded revision still suppressed")
	}
	armLive(t, st, ctx, f)
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("re-evaluating the new revision did not re-arm")
	}

	// A row that names no declaration at all is not evidence of coverage either.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_alert_state SET revision_id = NULL WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("clear stamp: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("an unstamped evaluation suppressed: absence of evidence is not coverage")
	}
}

// The suppression record is idempotent under redelivery and explains itself once.
func TestRecordSuppressionIsIdempotent(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// An open auto-incident to annotate.
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, MonitorID: f.monitorID, Title: "checkout-http is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "monitor reported down", "system")
	if err != nil {
		t.Fatalf("incident: %v", err)
	}
	var eventID string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO outbox_events (topic, payload) VALUES ('monitor_transition','{}') RETURNING id`).
		Scan(&eventID); err != nil {
		t.Fatalf("event: %v", err)
	}
	owners := []DelegationOwner{{ServiceID: f.serviceID, Slug: "checkout", Name: "Checkout"}}

	for i := 0; i < 3; i++ {
		if err := st.RecordSuppression(ctx, eventID, f.monitorID, f.projectID,
			domain.TopicMonitorTransition, owners); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	var rows, notes int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_suppressions`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM incident_updates
		 WHERE incident_id = $1 AND author = 'system' AND body LIKE $2`,
		inc.ID, domain.SuppressionMarker+"%").Scan(&notes); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d suppression rows after 3 redeliveries — the outbox is at-least-once", rows)
	}
	if notes != 1 {
		t.Fatalf("%d notes after 3 redeliveries, want exactly 1", notes)
	}

	// A CHANGED owner set is a different statement and earns its own line.
	other, err := st.CreateService(ctx, domain.Service{
		ProjectID: f.projectID, Slug: "orders", Name: "Orders",
	})
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	if err := st.RecordSuppression(ctx, eventID, f.monitorID, f.projectID,
		domain.TopicMonitorTransition, append(owners,
			DelegationOwner{ServiceID: other.ID, Slug: "orders", Name: "Orders"})); err != nil {
		t.Fatalf("record with two owners: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM incident_updates
		 WHERE incident_id = $1 AND author = 'system' AND body LIKE $2`,
		inc.ID, domain.SuppressionMarker+"%").Scan(&notes); err != nil {
		t.Fatalf("recount notes: %v", err)
	}
	if notes != 2 {
		t.Fatalf("a changed owner set produced %d notes, want a second line naming both", notes)
	}
	// And no open incident is a valid outcome, not a failure.
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_suppressions`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("%d suppression rows for two owners, want one per (event, owner)", rows)
	}
	_ = time.Now
}

// FR-021 §16.4b — the delivery fence must be keyed by the LATCH's identity, not by half of it.
//
// 00077 keys service targets by (service_id, window_name), so one service may legally hold a 7d and a
// 30d target whose burn rules are identical — the canonical key is severity/windows/threshold and
// names no window. A fence scoped by (service, rule) matches BOTH rows and takes whichever Postgres
// returns first, which is not a cosmetic error: one target's onset gets dropped as "superseded" by
// the other target's sequence, and the page is simply never delivered.
func TestBurnSequenceIsScopedToItsTarget(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// A SECOND target on the same service, different window, THE SAME rule.
	var second string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1,'7d',99.9,true,'[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14,"severity":"page"}]')
		RETURNING id`, f.serviceID).Scan(&second); err != nil {
		t.Fatalf("second target: %v", err)
	}
	// One shared canonical key, two independent sequences.
	const shared = "page/3600/300/14"
	for _, tc := range []struct {
		target string
		seq    int64
	}{{f.targetID, 4}, {second, 11}} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO service_burn_alert_state
			  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict,
			   target_generation, config_generation, emitted_seq, evaluated_at, lease_until)
			SELECT s.id, s.project_id, $2, $3, true, 'fire', t.alert_generation,
			       s.alert_config_generation, $4, now(), now() + interval '90 seconds'
			  FROM services s JOIN sla_targets t ON t.id = $2
			 WHERE s.id = $1`, f.serviceID, tc.target, shared, tc.seq); err != nil {
			t.Fatalf("latch %s: %v", tc.target, err)
		}
	}

	for _, tc := range []struct {
		name   string
		target string
		want   int64
	}{
		{"30d target reads its own sequence", f.targetID, 4},
		{"7d target reads its own sequence", second, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ServiceAlertSequence(ctx, domain.ServiceAlert{
				ServiceID: f.serviceID, ProjectID: f.projectID,
				Signal:  domain.ServiceSignalBurn,
				RuleKey: shared, SLATargetID: tc.target,
			})
			if err != nil {
				t.Fatalf("sequence: %v", err)
			}
			if got != tc.want {
				t.Fatalf("sequence %d, want %d — the fence read the other target's latch", got, tc.want)
			}
		})
	}

	// A burn payload that cannot identify its latch is an ERROR, not an unfenced delivery: the
	// outbox retries and dead-letters visibly, where a silent pass would page somebody with an
	// ordering nobody checked.
	if _, err := st.ServiceAlertSequence(ctx, domain.ServiceAlert{
		ServiceID: f.serviceID, ProjectID: f.projectID,
		Signal: domain.ServiceSignalBurn, RuleKey: shared,
	}); err == nil {
		t.Fatal("a burn payload with no target identity was fenced anyway, want a loud error")
	}

	// Cross-tenant safety on the health latch: the right service in the WRONG project is absent,
	// not answered from the service row alone.
	if _, err := st.ServiceAlertSequence(ctx, domain.ServiceAlert{
		ServiceID: f.serviceID, ProjectID: f.serviceID, // a real uuid that is not this project
		Signal: domain.ServiceSignalHealth,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("health sequence for a foreign project: %v, want ErrNotFound", err)
	}
}

// declareTwoBurnRules makes the target DECLARE two rules, for tests that latch two of them. The
// canonical keys of the declaration and of the latch rows need not match for these tests — arming
// compares how MANY verdicts exist against how many rules are declared, and the identity half is the
// target generation — but a target that latches more rules than it declares is not a configuration
// any write path can produce.
func declareTwoBurnRules(t *testing.T, st *Store, ctx context.Context, f armFixture) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		UPDATE sla_targets SET burn_rules = '[
		    {"long_window_seconds":3600,"short_window_seconds":300,"threshold":14,"severity":"page"},
		    {"long_window_seconds":21600,"short_window_seconds":1800,"threshold":6,"severity":"ticket"}
		]'::jsonb WHERE id = $1`, f.targetID); err != nil {
		t.Fatalf("declare two rules: %v", err)
	}
	// The declaration change bumps the target generation, so the existing latch rows have to be
	// re-stamped exactly as a fresh evaluation would stamp them.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_burn_alert_state bs SET target_generation = t.alert_generation
		  FROM sla_targets t WHERE t.id = bs.sla_target_id AND t.id = $1`, f.targetID); err != nil {
		t.Fatalf("restamp latches: %v", err)
	}
}

// armBurnRule writes ONE rule's latch, for tests about services that own more than one rule.
func armBurnRule(t *testing.T, st *Store, ctx context.Context, f armFixture,
	targetID, ruleKey, verdict string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict,
		   target_generation, config_generation, evaluated_at, lease_until)
		SELECT s.id, s.project_id, t.id, $3, false, $4, t.alert_generation,
		       s.alert_config_generation, now(), now() + interval '90 seconds'
		  FROM services s JOIN sla_targets t ON t.id = $2
		 WHERE s.id = $1
		ON CONFLICT (service_id, project_id, sla_target_id, rule_key) DO UPDATE SET
		   last_verdict = EXCLUDED.last_verdict, evaluated_at = EXCLUDED.evaluated_at,
		   lease_until = EXCLUDED.lease_until, last_error = NULL`,
		f.serviceID, targetID, ruleKey, verdict); err != nil {
		t.Fatalf("arm burn rule %s: %v", ruleKey, err)
	}
}

// FR-021 §16.1 — "**Any HOLD dis-arms BURN coverage**" is a statement about the SERVICE's coverage,
// not about one row of it. A target carries an ARRAY of up to four rules and 00077 lets one service
// hold several enabled targets, so "some rule is quotable" and "every rule can speak" are different
// predicates that coincide only when there is exactly one rule — which is exactly the shape every
// earlier test had, and how the gap survived. The direction of the error is the dangerous one: the
// member's own burn alert is muted while part of the replacement is structurally unable to fire.
func TestOneHoldingRuleAmongManyDisarmsBurnCoverage(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("fixture is not armed for burn: the rest of this test proves nothing")
	}

	// A SECOND rule on the SAME target, also quotable. The negative control: without it, the test
	// would pass for the trivial reason that any second row dis-arms.
	//
	// DECLARED as well as latched: arming requires a verdict for every rule the target declares, so
	// a latch row for a rule nobody declares is an inconsistency in its own right and dis-arms.
	declareTwoBurnRules(t, st, ctx, f)
	armBurnRule(t, st, ctx, f, f.targetID, "rule-2", "clear")
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("a second QUOTABLE rule dis-armed burn coverage")
	}

	// Now that rule cannot speak. One of the two replacements the service owns is blind.
	armBurnRule(t, st, ctx, f, f.targetID, "rule-2", "hold")
	if delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("a service with a HELD rule armed burn coverage anyway: the member's own burn " +
			"alert is muted while one of the replacement rules cannot fire")
	}
	// The live signal is a different replacement and must be untouched.
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a held BURN rule dis-armed the LIVE signal too")
	}
}

// The same coverage question asked of a target the evaluator has never answered for: an operator can
// enable a second burn target at any moment, and between that write and the next evaluation there is
// no verdict to be quotable. Absence of evidence is not coverage (§16.1), so it dis-arms — the safe
// direction, in which members keep paging for themselves.
func TestEnabledTargetWithNoVerdictDisarmsBurnCoverage(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	if _, err := st.pool.Exec(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1,'7d',99.9,true,'[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14,"severity":"page"}]')`,
		f.serviceID); err != nil {
		t.Fatalf("second target: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("an enabled burn target with no evaluation at all armed burn coverage")
	}

	// Once the evaluator has answered for it, coverage returns — the dis-arm is about missing
	// evidence, not about owning two targets.
	var second string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM sla_targets WHERE service_id = $1 AND window_name = '7d'`,
		f.serviceID).Scan(&second); err != nil {
		t.Fatalf("read second target: %v", err)
	}
	armBurnRule(t, st, ctx, f, second, "rule-1", "clear")
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("two targets, both quotable, fresh and current, did not arm burn coverage")
	}
}

// FR-021 §16.1 — a rule that has never been evaluated is not coverage, and neither is a target
// whose latch rows do not account for every rule it declares.
//
// The clause that walks existing latch rows cannot see this: with rules {A} armed and fresh, adding
// B leaves A's row quotable, fresh and generation-matched, so "nothing is blind" was satisfied by a
// configuration in which half the replacement had never once run. The member monitor's own burn
// alert stayed suppressed the whole time.
func TestAddingARuleDisarmsUntilItHasBeenEvaluated(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("fixture is not armed for burn")
	}

	// A SECOND rule, declared through the product write path.
	rules := []domain.BurnRule{
		{LongWindowSeconds: 3600, ShortWindowSeconds: 300, Threshold: 14, Severity: domain.BurnSeverityPage},
		{LongWindowSeconds: 21600, ShortWindowSeconds: 1800, Threshold: 6, Severity: domain.BurnSeverityTicket},
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true, rules,
		AlertActor{}); err != nil {
		t.Fatalf("declare a second rule: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("burn coverage stayed armed after a rule was added: the member's own burn alert is " +
			"suppressed while the new rule has never been evaluated")
	}

	// Once every declared rule has a current, quotable verdict, coverage returns.
	for _, key := range []string{"page/3600/300/14", "ticket/21600/1800/6"} {
		armBurnRule(t, st, ctx, f, f.targetID, key, "clear")
	}
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("a fully evaluated two-rule target did not re-arm burn coverage")
	}

	// And the same hole from the other side: a latch row deleted directly leaves a declared rule
	// with no verdict at all, which the generation cannot see because no configuration changed.
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM service_burn_alert_state WHERE rule_key = 'ticket/21600/1800/6'`); err != nil {
		t.Fatalf("delete one latch: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("a declared rule with no latch row at all left burn coverage armed")
	}
}

// The generation bump must be SEMANTIC: reordering the same rules is not a change, and treating it
// as one would dis-arm a service — and page its members — for an edit nobody made.
func TestReorderingRulesIsNotAConfigurationChange(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	rules := []domain.BurnRule{
		{LongWindowSeconds: 3600, ShortWindowSeconds: 300, Threshold: 14, Severity: domain.BurnSeverityPage},
		{LongWindowSeconds: 21600, ShortWindowSeconds: 1800, Threshold: 6, Severity: domain.BurnSeverityTicket},
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true, rules,
		AlertActor{}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	var before int64
	if err := st.pool.QueryRow(ctx,
		`SELECT alert_generation FROM sla_targets WHERE id = $1`, f.targetID).Scan(&before); err != nil {
		t.Fatalf("read generation: %v", err)
	}

	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		[]domain.BurnRule{rules[1], rules[0]}, AlertActor{}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	var after int64
	if err := st.pool.QueryRow(ctx,
		`SELECT alert_generation FROM sla_targets WHERE id = $1`, f.targetID).Scan(&after); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	if after != before {
		t.Fatalf("a reorder bumped the target generation %d→%d: the service would dis-arm and its "+
			"members would page for a change nobody made", before, after)
	}
}

// Cardinality answers "is a rule missing a verdict"; it cannot answer "is this verdict ABOUT the
// rule that is declared". Swapping a rule's threshold in place keeps the count identical while every
// latch row now describes a rule nobody declares — and this is reachable without the product write
// path, which prunes such rows itself. The target generation is what closes that gap: the trigger
// bumps it on any semantic change to the declared rules, and a verdict for an older generation is
// not coverage.
func TestReplacingARuleInPlaceDisarmsDespiteMatchingCardinality(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("fixture is not armed for burn")
	}

	// A DIRECT edit, one rule replaced by another: still exactly one declared rule, still exactly
	// one latch row, and that row is fresh, error-free and quotable.
	if _, err := st.pool.Exec(ctx, `
		UPDATE sla_targets
		   SET burn_rules = '[{"long_window_seconds":3600,"short_window_seconds":300,
		                       "threshold":25,"severity":"page"}]'::jsonb
		 WHERE id = $1`, f.targetID); err != nil {
		t.Fatalf("replace the rule: %v", err)
	}
	var declared, latches int
	if err := st.pool.QueryRow(ctx, `
		SELECT jsonb_array_length(burn_rules),
		       (SELECT count(*) FROM service_burn_alert_state b WHERE b.sla_target_id = $1)
		  FROM sla_targets WHERE id = $1`, f.targetID).Scan(&declared, &latches); err != nil {
		t.Fatalf("read counts: %v", err)
	}
	if declared != latches {
		t.Fatalf("the fixture no longer isolates the identity gap: %d declared, %d latches",
			declared, latches)
	}

	if delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("burn coverage stayed armed on a verdict about a rule that is no longer declared: " +
			"the counts match, so only the target generation can tell these apart")
	}
}

// A schedule's `participants` is a JSON array of channel ids that NOTHING prunes: deleting a
// notification channel removes its row and leaves the id behind, and disabling one changes no JSON at
// all. Arming on "the array is non-empty" therefore let a service suppress its members' alerts on the
// strength of a rotation pointing at a channel that cannot receive anything — while the service's own
// page went to that same dead channel. Both paths silent, which is the one outcome §16.1 exists to
// prevent.
func TestADeadScheduleTargetDoesNotArmAnythingWhenTheProjectHasNoChannels(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// Point the service at a schedule whose only participant is the project's channel.
	var channelID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id::text FROM notification_channels WHERE project_id = $1 LIMIT 1`, f.projectID).
		Scan(&channelID); err != nil {
		t.Fatalf("read channel: %v", err)
	}
	sched, err := st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: f.projectID, Name: "primary", ShiftSeconds: 86400,
		Participants: []string{channelID}, AnchorAt: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET oncall_schedule_id = $2 WHERE id = $1`, f.serviceID, sched.ID); err != nil {
		t.Fatalf("attach schedule: %v", err)
	}
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("the fixture is not armed with a live schedule target: the rest proves nothing")
	}

	// Disable the channel. The schedule still names it, and its JSON is untouched.
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = false WHERE id = $1`, channelID); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	var participants int
	if err := st.pool.QueryRow(ctx,
		`SELECT jsonb_array_length(participants) FROM oncall_schedules WHERE id = $1`, sched.ID).
		Scan(&participants); err != nil {
		t.Fatalf("read participants: %v", err)
	}
	if participants == 0 {
		t.Fatal("disabling the channel emptied the schedule; this test no longer covers the gap it " +
			"was written for")
	}

	if delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a schedule whose only target is DISABLED still armed live coverage: the member's " +
			"alert is suppressed and the service's own page goes nowhere")
	}
	// The resolver agrees — the two must never disagree about who can be reached.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only probe
	route, err := resolveServiceRecipientsTx(ctx, tx, f.projectID, sched.ID, time.Now())
	if err != nil {
		t.Fatalf("resolve recipients: %v", err)
	}
	if len(route) != 0 {
		t.Fatalf("the resolver returned %v for a schedule pointing at a disabled channel", route)
	}

	// §16.6's documented fallback: give the project another live channel and BOTH arm again.
	other, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: f.projectID, Type: domain.ChannelWebhook, Name: "fallback",
		Config: map[string]string{"url": "https://hook.example/fallback"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("fallback channel: %v", err)
	}
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a project channel exists, so the service is routable again")
	}
	route, err = resolveServiceRecipientsTx(ctx, tx, f.projectID, sched.ID, time.Now())
	if err != nil {
		t.Fatalf("resolve recipients after fallback: %v", err)
	}
	if len(route) != 1 || route[0] != other.ID {
		t.Fatalf("the resolver returned %v, want the project's live channel %s", route, other.ID)
	}
}

// §16.1 asks whether the live policy can page the state the service is IN. The clause implemented
// `cardinality(page_on) > 0 OR page_on_unknown` — "this policy pages SOMETHING" — and the gap between
// those two questions is a lost page: a service sitting at DEGRADED with `page_on = {down}` announces
// nothing at all, while its member's DOWN alert was suppressed on the strength of a policy that does
// not cover what is happening.
func TestAPolicyThatDoesNotCoverTheCurrentStateArmsNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// The service is DOWN and the policy pages DOWN: armed, and the announcement is committed.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_alert_state
		   SET observed_state = 'down', candidate_state = 'down', live_firing = true,
		       emitted_state = 'down'
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("set down: %v", err)
	}
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a DOWN service with page_on={down} and a committed onset must cover its members")
	}

	// Now it is DEGRADED, which this policy does not page. The LATCH IS LEFT COMMITTED AND MATCHING
	// on purpose: clearing it would disarm through the committed-onset clause instead, and the test
	// would pass without ever exercising the one it is about.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_alert_state
		   SET observed_state = 'degraded', candidate_state = 'degraded', live_firing = true,
		       emitted_state = 'degraded'
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("set degraded: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a DEGRADED service with page_on={down} silenced its member: it announces nothing " +
			"for the state it is actually in")
	}

	// UNKNOWN with the opt-out off is the same shape, and the reason the switch exists at all.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_alert_state
		   SET observed_state = 'unknown', candidate_state = 'unknown', emitted_state = 'unknown'
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("set unknown: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("an UNKNOWN service with page_on_unknown=false silenced its member")
	}
}

// D-0176 opens a window on purpose: an onset withheld for want of a recipient leaves the service
// pageable-in-principle and silent in fact. Restoring the route must NOT arm coverage before the next
// evaluation announces anything, or the member falls silent first and the service speaks second.
func TestARestoredRouteDoesNotArmBeforeTheOnsetIsCommitted(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// The shape a withheld onset leaves behind: observed DOWN, nothing announced.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_alert_state
		   SET observed_state = 'down', candidate_state = 'down', streak = 3,
		       live_firing = false, emitted_state = NULL
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("withheld shape: %v", err)
	}
	if delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("coverage armed for a DOWN service that has announced nothing: the member would be " +
			"silenced before the service's own onset exists")
	}

	// The evaluator commits the onset. ONLY now is there a replacement to suppress in favour of.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_alert_state SET live_firing = true, emitted_state = 'down', emitted_seq = 1
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("commit onset: %v", err)
	}
	if !delegated(t, st, ctx, f, DelegationLive) {
		t.Fatal("a committed onset did not arm coverage")
	}
}

// A participant that is not a channel id was never valid configuration, and it must not be treated
// like the ordinary "the channel was deleted" that §16.6 falls back for. Falling back repairs a broken
// schedule silently and makes a typo indistinguishable from a deletion.
func TestACorruptScheduleParticipantIsRefusedAndSurfaced(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// The write path refuses a value that is not a channel id at all...
	_, err := st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: f.projectID, Name: "typo", ShiftSeconds: 86400,
		Participants: []string{"ops-team"}, AnchorAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "not a channel id") {
		t.Fatalf("a schedule naming a non-channel participant: %v", err)
	}
	// ...and one that is well-formed but belongs to nobody here, which is the check that actually
	// matters and the one only the write path can make.
	_, err = st.CreateOnCallSchedule(ctx, domain.OnCallSchedule{
		ProjectID: f.projectID, Name: "foreign", ShiftSeconds: 86400,
		Participants: []string{"1b4e28ba-2fa1-11d2-883f-0016d3cca427"}, AnchorAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "not a channel of this project") {
		t.Fatalf("a schedule naming a foreign channel: %v", err)
	}

	// A row written before that guard existed surfaces as an evaluation error rather than quietly
	// falling back to the project's channels.
	var schedule string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at, participants)
		VALUES ($1,'legacy',86400,now(),'["ops-team"]') RETURNING id`, f.projectID).Scan(&schedule); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only probe
	if _, err := resolveServiceRecipientsTx(ctx, tx, f.projectID, schedule, time.Now()); err == nil {
		t.Fatal("a corrupt participant resolved silently: the project's channels would stand in for " +
			"a schedule nobody fixed, and a typo would page the wrong people forever")
	}
}

// The burn verdict and the burn latch must AGREE. `clear` while still firing is a state no ordinary
// evaluation writes: the rule either came back under the threshold and cleared its latch, or it did
// not. Reading such a row as coverage would silence a member on the strength of a contradiction, so
// it fails open — the same direction every other ambiguity in §16.1 takes.
func TestAnAmbiguousBurnRowArmsNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if !delegated(t, st, ctx, f, DelegationBurn) {
		t.Fatal("the fixture is not armed for burn: the rest proves nothing")
	}

	for _, tc := range []struct {
		name    string
		verdict string
		firing  bool
		seq     int64
	}{
		{"clear while still firing", "clear", true, 1},
		{"fire with no announcement", "fire", false, 0},
		{"fire latched with no sequence", "fire", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, `
				UPDATE service_burn_alert_state
				   SET last_verdict = $2, firing = $3, emitted_seq = $4
				 WHERE service_id = $1`, f.serviceID, tc.verdict, tc.firing, tc.seq); err != nil {
				t.Fatalf("write the row: %v", err)
			}
			if delegated(t, st, ctx, f, DelegationBurn) {
				t.Fatalf("verdict=%s firing=%v seq=%d armed burn coverage", tc.verdict, tc.firing, tc.seq)
			}
		})
	}

	// And the two coherent shapes DO cover.
	for _, tc := range []struct {
		name    string
		verdict string
		firing  bool
		seq     int64
	}{
		{"clear and not firing", "clear", false, 0},
		{"fire, latched, announced", "fire", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, `
				UPDATE service_burn_alert_state
				   SET last_verdict = $2, firing = $3, emitted_seq = $4
				 WHERE service_id = $1`, f.serviceID, tc.verdict, tc.firing, tc.seq); err != nil {
				t.Fatalf("write the row: %v", err)
			}
			if !delegated(t, st, ctx, f, DelegationBurn) {
				t.Fatalf("verdict=%s firing=%v seq=%d did not cover", tc.verdict, tc.firing, tc.seq)
			}
		})
	}
}

// §16.6b promises a bounded, diagnosable reason when coverage fails, and delivery used to answer
// `no_active_owner` for every cause there is: not owned, never evaluated, stale, unroutable — one word
// for all of them, while the badge on the same screen could name the exact clause. Two surfaces
// disagreeing about why a monitor is still paging is a question an operator cannot even ask properly.
func TestDeliveryReportsTheSameReasonTheBadgeShows(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	reasonOf := func(t *testing.T, sig DelegationSignal) string {
		t.Helper()
		v, err := st.ActiveDelegation(ctx, f.monitorID, f.projectID, sig)
		if err != nil {
			t.Fatalf("active delegation: %v", err)
		}
		return v.FailOpenReason
	}
	badgeOf := func(t *testing.T) ServiceAlertingState {
		t.Helper()
		state, err := st.ServiceAlertingState(ctx, f.projectID, f.serviceID)
		if err != nil {
			t.Fatalf("alerting state: %v", err)
		}
		return state
	}

	for _, tc := range []struct {
		name   string
		breaks func(t *testing.T)
		want   string
	}{
		{
			name: "ownership withdrawn",
			breaks: func(t *testing.T) {
				exec(t, st, ctx, `UPDATE services SET owns_paging = false WHERE id = $1`, f.serviceID)
			},
			want: AlertReasonNotOwned,
		},
		{
			name: "never evaluated",
			breaks: func(t *testing.T) {
				exec(t, st, ctx, `UPDATE services SET owns_paging = true WHERE id = $1`, f.serviceID)
				exec(t, st, ctx, `DELETE FROM service_alert_state WHERE service_id = $1`, f.serviceID)
			},
			want: AlertReasonNeverEvaluated,
		},
		{
			name: "nothing to notify",
			breaks: func(t *testing.T) {
				armLive(t, st, ctx, f)
				exec(t, st, ctx, `UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID)
			},
			want: AlertReasonUnroutable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.breaks(t)
			if got := reasonOf(t, DelegationLive); got != tc.want {
				t.Fatalf("delivery reported %q, want %q — an operator asking 'why is this monitor "+
					"still paging' gets this string", got, tc.want)
			}
			if badge := badgeOf(t); badge.Live.Reason != tc.want {
				t.Fatalf("the badge says %q and delivery says %q for the same service at the same "+
					"instant", badge.Live.Reason, tc.want)
			}
		})
	}

	// And a monitor no service claims at all is a different answer from a service that exists and is
	// dis-armed: the two send an operator to different places.
	other, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "unclaimed", Type: domain.MonitorHTTP,
		Target: "https://unclaimed.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("second monitor: %v", err)
	}
	v, err := st.ActiveDelegation(ctx, other.ID, f.projectID, DelegationLive)
	if err != nil {
		t.Fatalf("active delegation: %v", err)
	}
	if v.FailOpenReason != AlertReasonNoOwningService {
		t.Fatalf("a monitor no service declares reported %q, want %q",
			v.FailOpenReason, AlertReasonNoOwningService)
	}
}
