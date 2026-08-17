package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.6a / §16.4a — the paging write surface, judged by what it ENDS.
//
// Every test here drives a REAL onset through the evaluator first, then makes the edit, then reads
// what was published. That order is the point: the hazard in this surface is not that it stores the
// wrong boolean, it is that an operator narrowing a policy leaves an announcement open that nothing
// will ever close, or ends one without telling the people who heard it start.

// pagingPolicy is the fixture policy — owning, down-only, confirm 2 — with the caller's edits
// applied by the test. Written as a helper because every test below differs from it in one field,
// and that one field is the subject of the test.
func pagingPolicy(states ...domain.ServiceAlertState) domain.ServiceAlertPolicy {
	return domain.ServiceAlertPolicy{OwnsPaging: true, PageOn: states, ConfirmEvaluations: 2}
}

// oneBurnRuleSet is `oneBurnRule` as the domain type: canonical key "page/300/120/14".
func oneBurnRuleSet() []domain.BurnRule {
	return []domain.BurnRule{{
		LongWindowSeconds: 300, ShortWindowSeconds: 120,
		Threshold: 14, Severity: domain.BurnSeverityPage,
	}}
}

// twoBurnRuleSet is `twoRulesJSON` as the domain type: "page/300/120/14" and "ticket/300/120/30".
func twoBurnRuleSet() []domain.BurnRule {
	return append(oneBurnRuleSet(), domain.BurnRule{
		LongWindowSeconds: 300, ShortWindowSeconds: 120,
		Threshold: 30, Severity: domain.BurnSeverityTicket,
	})
}

const twoRulesJSON = `[{"long_window_seconds":300,"short_window_seconds":120,` +
	`"threshold":14,"severity":"page"},` +
	`{"long_window_seconds":300,"short_window_seconds":120,` +
	`"threshold":30,"severity":"ticket"}]`

const ticketBurnRuleKey = "ticket/300/120/30"

// auditTargets returns the targets of every audit row with this action, oldest first.
func auditTargets(t *testing.T, st *Store, ctx context.Context, action string) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT target FROM audit_logs WHERE action = $1 ORDER BY created_at, id`, action)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func countRows(t *testing.T, st *Store, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func alertConfigGeneration(t *testing.T, st *Store, ctx context.Context, serviceID string) int64 {
	t.Helper()
	var g int64
	if err := st.pool.QueryRow(ctx,
		`SELECT alert_config_generation FROM services WHERE id = $1`, serviceID).Scan(&g); err != nil {
		t.Fatalf("generation: %v", err)
	}
	return g
}

func openHealthEpisodes(t *testing.T, st *Store, ctx context.Context, serviceID string) int {
	t.Helper()
	return countRows(t, st, ctx, `
		SELECT count(*) FROM service_alert_episodes
		 WHERE service_id = $1 AND signal = 'health' AND closed_at IS NULL`, serviceID)
}

func liveFiring(t *testing.T, st *Store, ctx context.Context, serviceID string) bool {
	t.Helper()
	var firing bool
	if err := st.pool.QueryRow(ctx,
		`SELECT live_firing FROM service_alert_state WHERE service_id = $1`, serviceID).
		Scan(&firing); err != nil {
		t.Fatalf("live latch: %v", err)
	}
	return firing
}

// (1) Turning ownership OFF while a burn rule is firing. The operator has withdrawn the service's
// right to speak, so the announcement ends — but it ends as `ownership_disabled`, reaching the
// ONSET's recipients and naming the target it was about, because after this commit nothing will
// evaluate that rule again and no evaluator could ever produce this close.
func TestDisowningPagingClosesAFiringBurnAnnouncement(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60) // ~16.7×, over the rule's threshold of 14

	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want a firing rule to disown", got.Onsets)
	}
	onset := burnEventsFor(t, st, ctx, f.targetID)
	if len(onset) != 1 || !onset[0].Firing {
		t.Fatalf("expected one onset, got %+v", onset)
	}

	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		domain.ServiceAlertPolicy{OwnsPaging: false, PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown},
			ConfirmEvaluations: 2}, AlertActor{ViaToken: true})
	if err != nil {
		t.Fatalf("disown: %v", err)
	}
	if stored.OwnsPaging {
		t.Fatalf("stored policy = %+v, want ownership off", stored)
	}

	events := burnEventsFor(t, st, ctx, f.targetID)
	if len(events) != 2 {
		t.Fatalf("%d events after disowning a firing service, want the onset and ONE close", len(events))
	}
	got := events[1]
	switch {
	case got.Firing:
		t.Fatal("disowning published another onset")
	case got.CloseReason != domain.CloseOwnershipDisabled:
		t.Fatalf("close reason = %q, want ownership_disabled — turning paging off is not a recovery",
			got.CloseReason)
	case got.Signal != domain.ServiceSignalBurn || got.RuleKey != oneBurnRuleKey:
		t.Fatalf("close identity = %q/%q", got.Signal, got.RuleKey)
	case got.SLATargetID != f.targetID || got.Window != "30d":
		t.Fatalf("close target = %q/%q, want the firing target's own identity",
			got.SLATargetID, got.Window)
	case len(got.Recipients) == 0 || len(got.Recipients) != len(onset[0].Recipients):
		t.Fatalf("close recipients = %v, want the ONSET's snapshot %v",
			got.Recipients, onset[0].Recipients)
	case got.EpisodeID != onset[0].EpisodeID:
		t.Fatalf("the close ends episode %q, want the onset's %q", got.EpisodeID, onset[0].EpisodeID)
	case got.Seq <= onset[0].Seq:
		t.Fatalf("close seq %d does not follow the onset's %d", got.Seq, onset[0].Seq)
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 0 {
		t.Fatalf("%d episodes still open after the close", n)
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); s.firing {
		t.Fatal("the latch is still FIRING with no open episode: re-enabling ownership would " +
			"swallow the next onset as 'no edge'")
	}

	// §16.6a: every paging-config change is audited with its actor and its before/after.
	audits := auditTargets(t, st, ctx, "service.alerting")
	if len(audits) != 1 {
		t.Fatalf("audit rows = %d, want exactly one", len(audits))
	}
	if !strings.Contains(audits[0], "service="+f.serviceID) ||
		!strings.Contains(audits[0], "owns_paging:true→false") {
		t.Fatalf("audit target = %q, want the service and the field that moved", audits[0])
	}
	if strings.Contains(audits[0], "confirm_evaluations") {
		t.Fatalf("audit target = %q, want ONLY the changed fields", audits[0])
	}
}

// (2) `page_on` narrowed so the state the service ANNOUNCED is no longer pageable. The operator has
// withdrawn the statement; the episode ends as `policy_changed`, which is the only honest reason —
// nothing here is evidence about the service.
func TestNarrowingPageOnClosesTheAnnouncementItNoLongerCovers(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	onset := alertEvents(t, st, ctx)
	if len(onset) != 1 || onset[0].State != domain.ServiceAlertDown {
		t.Fatalf("expected one DOWN onset, got %+v", onset)
	}

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		pagingPolicy(domain.ServiceAlertDegraded), AlertActor{}); err != nil {
		t.Fatalf("narrow page_on: %v", err)
	}

	events := alertEvents(t, st, ctx)
	if len(events) != 2 {
		t.Fatalf("%d events, want the onset and ONE close", len(events))
	}
	got := events[1]
	switch {
	case got.Firing:
		t.Fatal("narrowing the policy published another onset")
	case got.CloseReason != domain.ClosePolicyChanged:
		t.Fatalf("close reason = %q, want policy_changed", got.CloseReason)
	case got.Signal != domain.ServiceSignalHealth || got.State != domain.ServiceAlertDown:
		t.Fatalf("the close does not name what ended: %+v", got)
	case got.EpisodeID != onset[0].EpisodeID:
		t.Fatalf("the close ends episode %q, want the onset's %q", got.EpisodeID, onset[0].EpisodeID)
	case len(got.Recipients) == 0 || len(got.Recipients) != len(onset[0].Recipients):
		t.Fatalf("close recipients = %v, want the ONSET's snapshot %v",
			got.Recipients, onset[0].Recipients)
	case got.Seq <= onset[0].Seq:
		t.Fatalf("close seq %d does not follow the onset's %d", got.Seq, onset[0].Seq)
	}
	if n := openHealthEpisodes(t, st, ctx, f.serviceID); n != 0 {
		t.Fatalf("%d health episodes still open after the close", n)
	}
	if liveFiring(t, st, ctx, f.serviceID) {
		t.Fatal("the live latch is still FIRING with no open episode")
	}
	if audits := auditTargets(t, st, ctx, "service.alerting"); len(audits) != 1 ||
		!strings.Contains(audits[0], "page_on:{down}→{degraded}") {
		t.Fatalf("audit = %v, want the page_on before→after", audits)
	}
}

// The negative control for (2), and the reason the close is judged with `Pages` rather than with
// "the policy changed": an edit that still covers the announced state must close NOTHING. Closing
// here would end an outage announcement while the outage is still happening.
func TestWideningPageOnClosesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	if len(alertEvents(t, st, ctx)) != 1 {
		t.Fatal("expected one onset before the edit")
	}

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		pagingPolicy(domain.ServiceAlertDegraded, domain.ServiceAlertDown), AlertActor{}); err != nil {
		t.Fatalf("widen page_on: %v", err)
	}

	if events := alertEvents(t, st, ctx); len(events) != 1 {
		t.Fatalf("%d events, want only the onset — the new policy still pages for DOWN, so the "+
			"announcement is still true", len(events))
	}
	if n := openHealthEpisodes(t, st, ctx, f.serviceID); n != 1 {
		t.Fatalf("open health episodes = %d, want the announcement still open", n)
	}
	if !liveFiring(t, st, ctx, f.serviceID) {
		t.Fatal("the live latch was cleared by an edit that closed nothing")
	}
	// It is still a paging-config change, so it is still audited — with the canonical, sorted value.
	if audits := auditTargets(t, st, ctx, "service.alerting"); len(audits) != 1 ||
		!strings.Contains(audits[0], "page_on:{down}→{degraded,down}") {
		t.Fatalf("audit = %v, want the canonical page_on before→after", audits)
	}
}

// (3) §16.4a states it explicitly: confirmation governs the ONSET, not an announcement that is
// already open. The edit still bumps `alert_config_generation` — it dis-arms delegation until the
// new configuration has been evaluated, which is the safe direction.
func TestConfirmEvaluationsEditClosesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	if len(alertEvents(t, st, ctx)) != 1 {
		t.Fatal("expected one onset before the edit")
	}
	before := alertConfigGeneration(t, st, ctx, f.serviceID)

	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		domain.ServiceAlertPolicy{OwnsPaging: true,
			PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown}, ConfirmEvaluations: 5},
		AlertActor{})
	if err != nil {
		t.Fatalf("confirm edit: %v", err)
	}
	if stored.ConfirmEvaluations != 5 {
		t.Fatalf("stored policy = %+v, want confirm_evaluations 5", stored)
	}

	if events := alertEvents(t, st, ctx); len(events) != 1 {
		t.Fatalf("%d events, want only the onset: a confirmation edit ends nothing", len(events))
	}
	if n := openHealthEpisodes(t, st, ctx, f.serviceID); n != 1 {
		t.Fatalf("open health episodes = %d, want the announcement untouched", n)
	}
	if !liveFiring(t, st, ctx, f.serviceID) {
		t.Fatal("a confirmation edit cleared the live latch")
	}
	if after := alertConfigGeneration(t, st, ctx, f.serviceID); after != before+1 {
		t.Fatalf("alert_config_generation %d → %d, want exactly one bump: the edit must dis-arm "+
			"delegation until it has been evaluated", before, after)
	}
	audits := auditTargets(t, st, ctx, "service.alerting")
	if len(audits) != 1 || !strings.Contains(audits[0], "confirm_evaluations:2→5") {
		t.Fatalf("audit = %v, want the confirm_evaluations before→after", audits)
	}
	if strings.Contains(audits[0], "page_on") || strings.Contains(audits[0], "owns_paging") {
		t.Fatalf("audit target = %q, want ONLY the field that moved", audits[0])
	}
}

// (4) Disabling burn alerting on ONE target ends that target's announcements and takes its latch
// rows with them — and touches no other target of the same service, which share nothing but the
// service and (legally) their canonical rule keys.
func TestDisablingBurnClosesOnlyThatTargetAndDropsItsLatches(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule) // the 30d target, objective 99.9

	var weekID string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1,'7d',99.5,true,$2::jsonb) RETURNING id`,
		f.serviceID, oneBurnRule).Scan(&weekID); err != nil {
		t.Fatalf("second target: %v", err)
	}
	plantBurn(t, st, ctx, f, 5, minute/10) // 100× / 20×: both targets breach
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 2 {
		t.Fatalf("onsets = %d, want one per target", got.Onsets)
	}
	weekOnset := burnEventsFor(t, st, ctx, weekID)

	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "7d", false,
		oneBurnRuleSet(), AlertActor{}); err != nil {
		t.Fatalf("disable burn: %v", err)
	}

	week := burnEventsFor(t, st, ctx, weekID)
	if len(week) != 2 {
		t.Fatalf("%d events for the disabled target, want the onset and ONE close", len(week))
	}
	got := week[1]
	switch {
	case got.Firing:
		t.Fatal("disabling burn published another onset")
	case got.CloseReason != domain.CloseBurnDisabled:
		t.Fatalf("close reason = %q, want burn_disabled", got.CloseReason)
	case got.Window != "7d" || got.SLATargetID != weekID || got.RuleKey != oneBurnRuleKey:
		t.Fatalf("the close does not name what ended: %+v", got)
	case got.EpisodeID != weekOnset[0].EpisodeID:
		t.Fatalf("the close ends episode %q, want the onset's %q", got.EpisodeID, weekOnset[0].EpisodeID)
	case len(got.Recipients) != len(weekOnset[0].Recipients) || len(got.Recipients) == 0:
		t.Fatalf("close recipients = %v, want the ONSET's snapshot", got.Recipients)
	}
	if n := openBurnEpisodes(t, st, ctx, weekID); n != 0 {
		t.Fatalf("%d episodes still open on the disabled target", n)
	}
	if n := countRows(t, st, ctx,
		`SELECT count(*) FROM service_burn_alert_state WHERE sla_target_id = $1`, weekID); n != 0 {
		t.Fatalf("%d latch rows survive a disabled target: the arming query would keep asking them "+
			"to speak, their lease would expire, and burn coverage would be dis-armed forever", n)
	}

	// The other target of the SAME service is untouched: still firing, still open, still latched.
	if events := burnEventsFor(t, st, ctx, f.targetID); len(events) != 1 {
		t.Fatalf("the 30d target published %d events, want only its onset", len(events))
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("the 30d episode is not open (%d): the 7d disable ended the wrong alert", n)
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); !s.firing {
		t.Fatalf("the 30d latch = %+v, want untouched by the other target's disable", s)
	}

	audits := auditTargets(t, st, ctx, "service.burn_alerting")
	if len(audits) != 1 || !strings.Contains(audits[0], "window=7d") ||
		!strings.Contains(audits[0], "enabled:true→false") {
		t.Fatalf("audit = %v, want the window and the switch's before→after", audits)
	}
	if strings.Contains(audits[0], "rules:") {
		t.Fatalf("audit target = %q, want ONLY the field that moved — the rules were kept", audits[0])
	}
}

// (5) Removing ONE rule from a two-rule target ends exactly that rule's announcement, with
// `rule_removed`, and leaves its sibling firing. The two share a target and nothing else.
func TestRemovingOneRuleClosesOnlyThatRule(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, twoRulesJSON)
	plantBurn(t, st, ctx, f, 5, 2*minute/60) // 33.3×: over BOTH thresholds
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 2 {
		t.Fatalf("onsets = %d, want both rules firing before the removal", got.Onsets)
	}

	// The ticket rule is dropped; the page rule is re-declared unchanged.
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		oneBurnRuleSet(), AlertActor{}); err != nil {
		t.Fatalf("remove rule: %v", err)
	}

	events := burnEventsFor(t, st, ctx, f.targetID)
	if len(events) != 3 {
		t.Fatalf("%d events, want two onsets and ONE close", len(events))
	}
	var closes []domain.ServiceAlert
	for _, e := range events {
		if !e.Firing {
			closes = append(closes, e)
		}
	}
	if len(closes) != 1 {
		t.Fatalf("%d closes, want exactly the removed rule's", len(closes))
	}
	got := closes[0]
	switch {
	case got.CloseReason != domain.CloseRuleRemoved:
		t.Fatalf("close reason = %q, want rule_removed", got.CloseReason)
	case got.RuleKey != ticketBurnRuleKey:
		t.Fatalf("the close names rule %q, want the removed one %q", got.RuleKey, ticketBurnRuleKey)
	case got.SLATargetID != f.targetID || got.Window != "30d":
		t.Fatalf("the close does not name its target: %+v", got)
	case len(got.Recipients) == 0:
		t.Fatal("the close reaches nobody")
	}

	// The surviving rule is untouched, on every axis that could silence it later.
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); !s.firing || s.seq != 1 {
		t.Fatalf("the surviving rule's latch = %+v, want still firing on its original sequence", s)
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("open episodes = %d, want the surviving rule's still open", n)
	}
	// The removed rule's latch row is GONE — see the next test for why that is the load-bearing half.
	if n := countRows(t, st, ctx, `
		SELECT count(*) FROM service_burn_alert_state
		 WHERE sla_target_id = $1 AND rule_key = $2`, f.targetID, ticketBurnRuleKey); n != 0 {
		t.Fatalf("%d latch rows for a rule that no longer exists", n)
	}
	if n := countRows(t, st, ctx,
		`SELECT count(*) FROM service_burn_alert_state WHERE sla_target_id = $1`, f.targetID); n != 1 {
		t.Fatalf("latch rows = %d, want exactly the one declared rule", n)
	}

	audits := auditTargets(t, st, ctx, "service.burn_alerting")
	if len(audits) != 1 || !strings.Contains(audits[0],
		"rules:{page/300/120/14,ticket/300/120/30}→{page/300/120/14}") {
		t.Fatalf("audit = %v, want the rule-key set's before→after", audits)
	}
	if strings.Contains(audits[0], "enabled:") {
		t.Fatalf("audit target = %q, want ONLY the field that moved", audits[0])
	}
}

// (6) The consequence of (5), and the whole reason the latch row is deleted rather than left behind.
//
// `activeBurnDelegationSQL` has two halves: one replacement must EXIST, and NOTHING the service owns
// may be blind. A latch row for a rule that no longer exists sits forever in the second half — no
// evaluator writes it, so its lease expires and is never renewed — and burn coverage for the whole
// service is dis-armed permanently. The members would page for themselves forever, which is safe and
// is not what the operator asked for.
func TestRemovedRuleLatchDeletionKeepsBurnCoverageArmed(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, twoRulesJSON)
	plantBurn(t, st, ctx, f, 5, 2*minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 2 {
		t.Fatalf("onsets = %d, want both rules firing before the removal", got.Onsets)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		oneBurnRuleSet(), AlertActor{}); err != nil {
		t.Fatalf("remove rule: %v", err)
	}

	// Time passes: every lease written before the edit runs out. Only rules the target still
	// declares get a new one, because only those are evaluated.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_burn_alert_state
		   SET evaluated_at = now() - interval '10 minutes', lease_until = now() - interval '5 minutes'`,
	); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if got := burnEvalOnce(t, st, ctx); got.Rules != 1 {
		t.Fatalf("the pass after the removal evaluated %d rules, want only the declared one", got.Rules)
	}
	if !delegated(t, st, ctx, f.armFixture, DelegationBurn) {
		t.Fatal("burn coverage is dis-armed after a rule removal: the surviving rule is quotable and " +
			"fresh, so the only thing that could dis-arm the service is a latch nothing evaluates")
	}

	// The counterfactual, so this test proves the DELETE and not merely the happy path: put the
	// removed rule's latch back, exactly as leaving it behind would have, and coverage collapses.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict,
		   target_generation, config_generation, evaluated_at, lease_until)
		SELECT s.id, s.project_id, t.id, $3, false, 'clear', t.alert_generation,
		       s.alert_config_generation, now() - interval '10 minutes', now() - interval '5 minutes'
		  FROM services s JOIN sla_targets t ON t.id = $2
		 WHERE s.id = $1`, f.serviceID, f.targetID, ticketBurnRuleKey); err != nil {
		t.Fatalf("resurrect the stale latch: %v", err)
	}
	if delegated(t, st, ctx, f.armFixture, DelegationBurn) {
		t.Fatal("a latch for a rule that no longer exists did NOT dis-arm burn coverage — then the " +
			"deletion in SetServiceBurnAlerting is not what keeps coverage armed, and this test " +
			"proves nothing")
	}
}

// (7) A file-managed service renders these fields read-only and REFUSES the edit (§16.6a). The
// refusal must also write nothing: a 409 that had already closed an announcement would be the worst
// of both answers.
func TestFileManagedServiceRefusesPagingWrites(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want a firing rule", got.Onsets)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO managed_services (service_id, provider_id, org_id, project_id, source_uid)
		SELECT $1, 'file:shop.yaml', p.org_id, p.id, 'checkout' FROM projects p WHERE p.id = $2`,
		f.serviceID, f.projectID); err != nil {
		t.Fatalf("manage: %v", err)
	}
	genBefore := alertConfigGeneration(t, st, ctx, f.serviceID)

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		pagingPolicy(domain.ServiceAlertDegraded), AlertActor{}); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("policy write on a file-owned service = %v, want ErrServiceManagedByFile", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", false,
		oneBurnRuleSet(), AlertActor{}); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("burn write on a file-owned service = %v, want ErrServiceManagedByFile", err)
	}

	// Nothing moved: not the declaration, not the generation, not the announcement, not the audit.
	var owns, unknown bool
	var pageOn []string
	var confirm int
	var enabled bool
	if err := st.pool.QueryRow(ctx, `
		SELECT s.owns_paging, s.page_on, s.page_on_unknown, s.confirm_evaluations, t.burn_alert_enabled
		  FROM services s JOIN sla_targets t ON t.id = $2 WHERE s.id = $1`,
		f.serviceID, f.targetID).Scan(&owns, &pageOn, &unknown, &confirm, &enabled); err != nil {
		t.Fatalf("read declaration: %v", err)
	}
	if !owns || len(pageOn) != 1 || pageOn[0] != "down" || unknown || confirm != 2 || !enabled {
		t.Fatalf("the refused writes changed the declaration: owns=%v page_on=%v unknown=%v "+
			"confirm=%d burn_enabled=%v", owns, pageOn, unknown, confirm, enabled)
	}
	if got := alertConfigGeneration(t, st, ctx, f.serviceID); got != genBefore {
		t.Fatalf("alert_config_generation %d → %d on a refused write", genBefore, got)
	}
	if events := burnEventsFor(t, st, ctx, f.targetID); len(events) != 1 {
		t.Fatalf("%d events, want only the onset — a refused edit closed an announcement", len(events))
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("open episodes = %d, want the announcement untouched", n)
	}
	if n := countRows(t, st, ctx,
		`SELECT count(*) FROM service_burn_alert_state WHERE sla_target_id = $1`, f.targetID); n != 1 {
		t.Fatalf("latch rows = %d, want the firing rule's row untouched", n)
	}
	if a := auditTargets(t, st, ctx, "service.alerting"); len(a) != 0 {
		t.Fatalf("a refused write audited %v", a)
	}
	if a := auditTargets(t, st, ctx, "service.burn_alerting"); len(a) != 0 {
		t.Fatalf("a refused write audited %v", a)
	}
}

// (8) A foreign tenant gets ErrNotFound — the same answer an unknown id gets, so existence never
// leaks across the boundary — and writes nothing.
func TestForeignProjectCannotEditPagingConfig(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want a firing rule", got.Onsets)
	}
	other, err := st.CreateProject(ctx, f.orgID, "other", "Other")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	genBefore := alertConfigGeneration(t, st, ctx, f.serviceID)

	if _, err := st.UpdateServiceAlertPolicy(ctx, other.ID, f.serviceID,
		pagingPolicy(domain.ServiceAlertDegraded), AlertActor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant policy write = %v, want ErrNotFound", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, other.ID, f.serviceID, "30d", false,
		oneBurnRuleSet(), AlertActor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant burn write = %v, want ErrNotFound", err)
	}
	// An id that is not even a uuid answers the same way rather than surfacing a driver error.
	if _, err := st.UpdateServiceAlertPolicy(ctx, "not-a-uuid", f.serviceID,
		pagingPolicy(domain.ServiceAlertDown), AlertActor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed project id = %v, want ErrNotFound", err)
	}

	if got := alertConfigGeneration(t, st, ctx, f.serviceID); got != genBefore {
		t.Fatalf("alert_config_generation %d → %d on a cross-tenant write", genBefore, got)
	}
	if events := burnEventsFor(t, st, ctx, f.targetID); len(events) != 1 {
		t.Fatalf("%d events, want only the onset", len(events))
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("open episodes = %d, want the announcement untouched", n)
	}
	if n := countRows(t, st, ctx,
		`SELECT count(*) FROM service_burn_alert_state WHERE sla_target_id = $1`, f.targetID); n != 1 {
		t.Fatalf("latch rows = %d, want the firing rule's row untouched", n)
	}
	if a := auditTargets(t, st, ctx, "service.alerting"); len(a) != 0 {
		t.Fatalf("a refused write audited %v", a)
	}
	if a := auditTargets(t, st, ctx, "service.burn_alerting"); len(a) != 0 {
		t.Fatalf("a refused write audited %v", a)
	}
}

// The window a service does not have is ErrNotFound, not a created target: enabling burn alerting on
// an objective nobody declared would page against a number that does not exist.
func TestBurnAlertingRefusesAnUndeclaredWindow(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "90d", true,
		oneBurnRuleSet(), AlertActor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("burn write for an undeclared window = %v, want ErrNotFound", err)
	}
	if n := countRows(t, st, ctx,
		`SELECT count(*) FROM sla_targets WHERE service_id = $1`, f.serviceID); n != 1 {
		t.Fatalf("sla_targets = %d, want the write to have created nothing", n)
	}
}

// Invalid input is refused BEFORE anything is written, by the one domain validator — including the
// §16.4b duplicate-key rule, which exists because one latch cannot answer for two rules.
func TestPagingWritesRejectInvalidInputBeforeWriting(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	genBefore := alertConfigGeneration(t, st, ctx, f.serviceID)

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		domain.ServiceAlertPolicy{OwnsPaging: true,
			PageOn: []domain.ServiceAlertState{domain.ServiceAlertUnknown}, ConfirmEvaluations: 2},
		AlertActor{}); err == nil {
		t.Fatal("page_on accepted `unknown`, which has its own switch")
	}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		domain.ServiceAlertPolicy{OwnsPaging: true,
			PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown}, ConfirmEvaluations: 99},
		AlertActor{}); err == nil {
		t.Fatal("confirm_evaluations accepted 99")
	}
	dup := append(oneBurnRuleSet(), oneBurnRuleSet()...)
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true, dup,
		AlertActor{}); err == nil {
		t.Fatal("two rules with one canonical key were accepted: the latch would be ambiguous")
	}
	if got := alertConfigGeneration(t, st, ctx, f.serviceID); got != genBefore {
		t.Fatalf("alert_config_generation %d → %d on a rejected write", genBefore, got)
	}
	// The canonical form is what is STORED and what comes back, not the caller's spelling.
	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		pagingPolicy(domain.ServiceAlertDown, domain.ServiceAlertDegraded, domain.ServiceAlertDown),
		AlertActor{})
	if err != nil {
		t.Fatalf("canonical write: %v", err)
	}
	if len(stored.PageOn) != 2 || stored.PageOn[0] != domain.ServiceAlertDegraded ||
		stored.PageOn[1] != domain.ServiceAlertDown {
		t.Fatalf("returned policy = %+v, want the canonical sorted, deduped list", stored.PageOn)
	}
	var pageOn []string
	if err := st.pool.QueryRow(ctx,
		`SELECT page_on FROM services WHERE id = $1`, f.serviceID).Scan(&pageOn); err != nil {
		t.Fatalf("read page_on: %v", err)
	}
	if len(pageOn) != 2 || pageOn[0] != "degraded" || pageOn[1] != "down" {
		t.Fatalf("stored page_on = %v, want the canonical form", pageOn)
	}
}
