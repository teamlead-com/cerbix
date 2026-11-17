package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// FR-042 / NFR-035, I9 — impact is a classification, not a control.
//
// Two services identical in every respect but their impact mapping (`a` on the defaults, `b` on
// {minor, critical, none}) are driven through the same outage by the REAL evaluators, and every
// reliability, alerting, gate, delegation, suppression and escalation output is compared side by
// side after the identities and instants are stripped. The only difference allowed is the one the
// mapping exists to make: `a`'s incident is `major` and `b`'s is `critical`.
//
// The source half of NFR-035 — that no computation READS impact — is impactguard_internal_test.go.

// nfr035Side is one of the two services and its single SLI member.
type nfr035Side struct {
	name                                                        string
	serviceID, monitorID, targetID, epoch, revision, incidentID string
}

// anonymize replaces the side's own ids in a canonical rendering with role names, so an id that
// appears as a VALUE (a gate reason naming the open incident, a payload naming its target) compares
// by role rather than by uuid — and a value naming the OTHER service's row would still differ.
func (s nfr035Side) anonymize(canon string) string {
	for id, role := range map[string]string{
		s.serviceID: "<service>", s.monitorID: "<monitor>", s.targetID: "<target>",
		s.epoch: "<epoch>", s.revision: "<revision>", s.incidentID: "<incident>",
	} {
		if id != "" {
			canon = strings.ReplaceAll(canon, id, role)
		}
	}
	return canon
}

// nfr035GateWindow is the SLA target's window, and so the gate policy's and the report's.
const nfr035GateWindow = "30d"

func nfr035Setup(t *testing.T, st *Store, ctx context.Context) (projectID string, a, b nfr035Side) {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// ONE route and ONE ladder, shared: a difference in who is paged or how it escalates could only
	// come from the services themselves.
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelWebhook, Name: "ops",
		Config: map[string]string{"url": "https://hook.example/x"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	policy, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: proj.ID, Name: "ladder", Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: ch.ID}}},
		},
	})
	if err != nil {
		t.Fatalf("escalation policy: %v", err)
	}

	side := func(name string) nfr035Side {
		mon, err := st.CreateMonitor(ctx, domain.Monitor{
			RecoveryThreshold: 1,
			ProjectID:         proj.ID, Name: name + "-http", Type: domain.MonitorHTTP,
			Target: "https://checkout.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
		})
		if err != nil {
			t.Fatalf("monitor %s: %v", name, err)
		}
		svc, err := st.CreateService(ctx, domain.Service{ProjectID: proj.ID, Slug: name, Name: name})
		if err != nil {
			t.Fatalf("service %s: %v", name, err)
		}
		if _, _, err := st.PutServiceDeclaration(ctx, proj.ID, svc.ID, domain.ServiceDeclaration{
			Monitors: []string{mon.ID}, SLI: []string{mon.ID},
		}, 0, DeclarationOptions{CreatedBy: "test"}); err != nil {
			t.Fatalf("declaration %s: %v", name, err)
		}
		// As armedService: the revision governs delegation, the epoch governs the evaluator, and
		// both take effect at the next bucket boundary unless backdated.
		for _, q := range []string{
			`UPDATE service_definition_revisions SET effective_at = now() - interval '1 hour' WHERE service_id = $1`,
			`UPDATE service_evaluation_epochs SET effective_at = now() - interval '1 hour' WHERE service_id = $1`,
		} {
			if _, err := st.pool.Exec(ctx, q, svc.ID); err != nil {
				t.Fatalf("backdate %s: %v", name, err)
			}
		}
		if _, err := st.pool.Exec(ctx,
			`UPDATE services SET owns_paging = true, escalation_policy_id = $2 WHERE id = $1`,
			svc.ID, policy.ID); err != nil {
			t.Fatalf("own paging + ladder %s: %v", name, err)
		}
		s := nfr035Side{name: name, serviceID: svc.ID, monitorID: mon.ID}
		// The service-scoped burn target exactly as armedService inserts it.
		if err := st.pool.QueryRow(ctx, `
			INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
			VALUES ($1,'30d',99.9,true,'[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14,"severity":"page"}]')
			RETURNING id`, svc.ID).Scan(&s.targetID); err != nil {
			t.Fatalf("target %s: %v", name, err)
		}
		if err := st.pool.QueryRow(ctx,
			`SELECT id, revision_id FROM service_evaluation_epochs WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`,
			svc.ID).Scan(&s.epoch, &s.revision); err != nil {
			t.Fatalf("epoch %s: %v", name, err)
		}
		// A gate policy on the target's window, so the decision is a decision and not NOT_CONFIGURED.
		doc := gateDoc(nil)
		doc.Window = nfr035GateWindow
		if _, _, err := st.PutGatePolicy(ctx, proj.ID, svc.ID, nil, doc, gateActorToken); err != nil {
			t.Fatalf("gate policy %s: %v", name, err)
		}
		return s
	}
	return proj.ID, side("a"), side("b")
}

// nfr035PlantFacts gives both services the SAME sealed 30-day window — one bad second in every
// minute, so availability, budget and burn all carry a number — with the watermark two minutes
// behind the database clock.
func nfr035PlantFacts(t *testing.T, st *Store, ctx context.Context, projectID string, sides ...nfr035Side) {
	t.Helper()
	sealed := gateDBNow(t, st, ctx).Add(-2 * time.Minute).Truncate(time.Minute)
	from := sealed.Add(-30 * 24 * time.Hour)
	for _, s := range sides {
		rf := reportFixture{declFixture: declFixture{projectID: projectID, serviceID: s.serviceID}, epochID: s.epoch}
		plantRange(t, st, ctx, rf, s.epoch, from, sealed, 59_000_000, 1_000_000, 0, 0, "sealed")
		setWatermark(t, st, ctx, rf, from.Add(-time.Hour), sealed)
	}
}

// nfr035Strip removes, recursively, every identity and every instant: the two services must
// differ in those, and in nothing else.
func nfr035Strip(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if key := nfr035Snake(k); nfr035Identity[key] || nfr035Instant(key) {
				delete(x, k)
				continue
			}
			x[k] = nfr035Strip(child)
		}
		return x
	case []any:
		for i := range x {
			x[i] = nfr035Strip(x[i])
		}
		return x
	default:
		return v
	}
}

var nfr035Identity = map[string]bool{
	"id": true, "service_id": true, "project_id": true, "monitor_id": true, "incident_id": true,
	"episode_id": true, "decision_id": true, "name": true, "slug": true, "service_name": true,
	"service_slug": true, "title": true,
}

func nfr035Instant(k string) bool {
	switch k {
	case "as_of", "from", "to", "sealed_through", "evaluated_at":
		return true
	}
	// The lease is an instant too.
	return strings.HasSuffix(k, "_at") || strings.HasSuffix(k, "_until")
}

// nfr035Snake spells a Go field name the way its json tag would (`ServiceID` → `service_id`,
// `EvaluatedAt` → `evaluated_at`): ServiceAlertingState and MonitorDelegation marshal untagged.
func nfr035Snake(k string) string {
	var b strings.Builder
	for i, r := range k {
		if r >= 'A' && r <= 'Z' {
			if i > 0 && k[i-1] >= 'a' && k[i-1] <= 'z' {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// nfr035Canon marshals v, strips it, and re-marshals it (map keys sorted by encoding/json).
func nfr035Canon(t *testing.T, what string, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", what, err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("%s: decode: %v", what, err)
	}
	out, err := json.Marshal(nfr035Strip(generic))
	if err != nil {
		t.Fatalf("%s: re-marshal: %v", what, err)
	}
	return string(out)
}

// nfr035Same asserts va and vb are non-empty and equal once normalized and anonymized.
func nfr035Same(t *testing.T, what string, sa, sb nfr035Side, va, vb any) {
	t.Helper()
	ca, cb := sa.anonymize(nfr035Canon(t, what, va)), sb.anonymize(nfr035Canon(t, what, vb))
	for _, empty := range []string{"", "null", "{}", "[]", `""`} {
		if ca == empty {
			t.Errorf("%s: compared value is empty (%s) — two empty answers prove nothing", what, ca)
			return
		}
	}
	if ca != cb {
		t.Errorf("%s differs between the two mappings (NFR-035):\n  a (defaults) %s\n  b (mapped)   %s", what, ca, cb)
	}
}

// nfr035Rows reads every row of `query` for one service as JSON objects.
func nfr035Rows(t *testing.T, st *Store, ctx context.Context, what, query string, args ...any) []json.RawMessage {
	t.Helper()
	rows, err := st.pool.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("%s: scan: %v", what, err)
		}
		out = append(out, raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return out
}

func TestTheMappingChangesNoReliabilityAlertingGateOrEscalationOutput(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	projectID, a, b := nfr035Setup(t, st, ctx)

	// The one difference: b's mapping. a stays on the defaults, stored as NULL (R3).
	mapped := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	if _, err := st.UpdateServiceAlertPolicy(ctx, projectID, b.serviceID,
		ServiceAlertPolicyPatch{ImpactByState: &mapped}, AlertActor{}); err != nil {
		t.Fatalf("map b: %v", err)
	}
	var aNull, bNull bool
	if err := st.pool.QueryRow(ctx, `
		SELECT (SELECT impact_by_state IS NULL FROM services WHERE id = $1),
		       (SELECT impact_by_state IS NULL FROM services WHERE id = $2)`,
		a.serviceID, b.serviceID).Scan(&aNull, &bNull); err != nil {
		t.Fatalf("read mappings: %v", err)
	}
	if !aNull || bNull {
		t.Fatalf("fixture: a's mapping NULL=%v (want true), b's NULL=%v (want false)", aNull, bNull)
	}

	nfr035PlantFacts(t, st, ctx, projectID, a, b)

	drive := func(up bool) {
		for _, s := range []nfr035Side{a, b} {
			setMemberHealth(t, st, ctx, alertFixture{projectID: projectID, serviceID: s.serviceID, monitorID: s.monitorID}, up)
		}
	}
	evaluate := func() {
		if _, err := st.evaluateServiceAlertsOn(ctx, st.pool, evalCadence); err != nil {
			t.Fatalf("live evaluation: %v", err)
		}
		if _, err := st.evaluateServiceBurnAlertsOn(ctx, st.pool, evalCadence); err != nil {
			t.Fatalf("burn evaluation: %v", err)
		}
	}
	drive(true)
	evaluate()
	drive(false)
	evaluate()
	evaluate()
	// Every announcement reached its recipient, as the outbox worker records it: delegation then
	// ARMS and the members are suppressed, so the comparison below covers suppression and not only
	// two identical fail-open answers.
	for _, raw := range nfr035Rows(t, st, ctx, "service_alert outbox",
		`SELECT payload FROM outbox_events WHERE topic = 'service_alert' ORDER BY created_at, id`) {
		var alert domain.ServiceAlert
		if err := json.Unmarshal(raw, &alert); err != nil {
			t.Fatalf("decode service_alert: %v", err)
		}
		if err := st.MarkServiceAlertDelivered(ctx, alert); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	}
	if _, err := st.AdvanceEscalations(ctx); err != nil {
		t.Fatalf("advance escalations: %v", err)
	}

	// ── The outputs, a against b ───────────────────────────────────────────────────────────

	incidentOf := func(s nfr035Side) (id string, impact domain.IncidentImpact) {
		if err := st.pool.QueryRow(ctx,
			`SELECT id, impact FROM incidents WHERE service_id = $1 AND resolved_at IS NULL`,
			s.serviceID).Scan(&id, &impact); err != nil {
			t.Fatalf("open incident of %s: %v", s.name, err)
		}
		return id, impact
	}
	var impactA, impactB domain.IncidentImpact
	a.incidentID, impactA = incidentOf(a)
	b.incidentID, impactB = incidentOf(b)

	alertState := func(s nfr035Side) []json.RawMessage {
		return nfr035Rows(t, st, ctx, "service_alert_state", `
			SELECT jsonb_build_object(
			         'observed_state', observed_state, 'candidate_state', candidate_state,
			         'streak', streak, 'live_firing', live_firing,
			         'emitted_state', emitted_state, 'emitted_seq', emitted_seq,
			         'config_generation', config_generation, 'last_error', last_error)
			  FROM service_alert_state WHERE service_id = $1`, s.serviceID)
	}
	nfr035Same(t, "service_alert_state", a, b, alertState(a), alertState(b))

	// D9 / I10 here too: the mapping write moved no configuration generation.
	var genA, genB int64
	if err := st.pool.QueryRow(ctx, `
		SELECT (SELECT alert_config_generation FROM services WHERE id = $1),
		       (SELECT alert_config_generation FROM services WHERE id = $2)`,
		a.serviceID, b.serviceID).Scan(&genA, &genB); err != nil {
		t.Fatalf("read generations: %v", err)
	}
	if genA != genB {
		t.Errorf("alert_config_generation a=%d b=%d — the mapping write bumped the generation (I10)", genA, genB)
	}

	alerting := func(s nfr035Side) ServiceAlertingState {
		v, err := st.ServiceAlertingState(ctx, projectID, s.serviceID)
		if err != nil {
			t.Fatalf("alerting state %s: %v", s.name, err)
		}
		return v
	}
	nfr035Same(t, "ServiceAlertingState", a, b, alerting(a), alerting(b))

	window, ok := sla.WindowByName(nfr035GateWindow)
	if !ok {
		t.Fatalf("no %s window", nfr035GateWindow)
	}
	report := func(s nfr035Side) domain.ServiceWindowReport {
		v, err := st.ServiceReliabilityReport(ctx, projectID, s.serviceID, window)
		if err != nil {
			t.Fatalf("report %s: %v", s.name, err)
		}
		return v
	}
	repA, repB := report(a), report(b)
	if repA.Budget == nil {
		t.Errorf("the report carries no budget — the comparison would not cover it: %+v", repA)
	}
	nfr035Same(t, "ServiceReliabilityReport", a, b, repA, repB)

	gate := func(s nfr035Side) domain.GateDecision {
		v, err := st.DecideGate(ctx, projectID, s.serviceID, gateBudget)
		if err != nil {
			t.Fatalf("gate %s: %v", s.name, err)
		}
		return v
	}
	gateA, gateB := gate(a), gate(b)
	if len(gateA.Reasons) == 0 {
		t.Errorf("the gate decision has no reasons — the comparison would not cover them: %+v", gateA)
	}
	nfr035Same(t, "DecideGate state", a, b, gateA.State, gateB.State)
	nfr035Same(t, "DecideGate reasons", a, b, gateA.Reasons, gateB.Reasons)

	delegation := func(s nfr035Side) MonitorDelegation {
		v, err := st.MonitorDelegation(ctx, s.monitorID, projectID)
		if err != nil {
			t.Fatalf("delegation %s: %v", s.name, err)
		}
		return v
	}
	delA, delB := delegation(a), delegation(b)
	if !delA.Live.Suppress() || !delA.Burn.Suppress() {
		t.Errorf("a's member is not suppressed on both signals — the comparison would not cover "+
			"suppression: %+v", delA)
	}
	nfr035Same(t, "MonitorDelegation", a, b, delA, delB)

	burnState := func(s nfr035Side) []json.RawMessage {
		return nfr035Rows(t, st, ctx, "service_burn_alert_state", `
			SELECT to_jsonb(x)
			  FROM service_burn_alert_state x WHERE service_id = $1 ORDER BY rule_key`, s.serviceID)
	}
	nfr035Same(t, "service_burn_alert_state", a, b, burnState(a), burnState(b))

	alerts := func(s nfr035Side) []json.RawMessage {
		return nfr035Rows(t, st, ctx, "service_alert outbox", `
			SELECT payload FROM outbox_events
			 WHERE topic = 'service_alert' AND payload->>'service_id' = $1
			 ORDER BY created_at, id`, s.serviceID)
	}
	alertsA, alertsB := alerts(a), alerts(b)
	nfr035Same(t, "service_alert payloads", a, b, alertsA, alertsB)

	ladder := func(incidentID string) []json.RawMessage {
		return nfr035Rows(t, st, ctx, "escalation", `
			SELECT jsonb_build_object('steps', sn.steps, 'repeat_last', sn.repeat_last,
			                          'escalation_step', i.escalation_step)
			  FROM incidents i JOIN incident_escalation_snapshots sn ON sn.incident_id = i.id
			 WHERE i.id = $1`, incidentID)
	}
	nfr035Same(t, "escalation snapshot and step", a, b, ladder(a.incidentID), ladder(b.incidentID))

	steps := func(incidentID string) []json.RawMessage {
		return nfr035Rows(t, st, ctx, "escalation_step outbox", `
			SELECT payload - 'subject_name' - 'monitor_name' FROM outbox_events
			 WHERE topic = $1 AND payload->>'incident_id' = $2
			 ORDER BY created_at, id`, domain.TopicEscalationStep, incidentID)
	}
	nfr035Same(t, "escalation_step payloads", a, b, steps(a.incidentID), steps(b.incidentID))

	// ── The ONE difference ─────────────────────────────────────────────────────────────────
	if impactA != domain.ImpactMajor || impactB != domain.ImpactCritical {
		t.Fatalf("impact a=%q b=%q, want major and critical — the mapping did not take effect, so the "+
			"equalities above compared two services the mapping never touched", impactA, impactB)
	}
}
