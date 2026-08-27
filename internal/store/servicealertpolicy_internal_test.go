package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

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

// alertAuditRow is an audit row read back with its ACTOR, not only its text. The target string says
// what changed; these three columns say WHO changed it and inside which tenant, which is the half a
// reader needs when the question is "who turned paging off at 03:00".
type alertAuditRow struct {
	orgID    string
	actorID  *string
	viaToken bool
	target   string
}

func alertAuditRows(t *testing.T, st *Store, ctx context.Context, action string) []alertAuditRow {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT org_id, actor_user_id, via_token, target
		  FROM audit_logs WHERE action = $1 ORDER BY created_at, id`, action)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()
	var out []alertAuditRow
	for rows.Next() {
		var r alertAuditRow
		if err := rows.Scan(&r.orgID, &r.actorID, &r.viaToken, &r.target); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// storedPolicy reads the four declared columns back as the domain type, so a test can compare what
// the DATABASE holds against what the write surface said it stored.
func storedPolicy(t *testing.T, st *Store, ctx context.Context, serviceID string) domain.ServiceAlertPolicy {
	t.Helper()
	var p domain.ServiceAlertPolicy
	var pageOn []string
	if err := st.pool.QueryRow(ctx, `
		SELECT owns_paging, page_on, page_on_unknown, confirm_evaluations
		  FROM services WHERE id = $1`, serviceID).
		Scan(&p.OwnsPaging, &pageOn, &p.PageOnUnknown, &p.ConfirmEvaluations); err != nil {
		t.Fatalf("read stored policy: %v", err)
	}
	for _, s := range pageOn {
		p.PageOn = append(p.PageOn, domain.ServiceAlertState(s))
	}
	return p
}

// storedBurn reads a target's alerting declaration: the switch, the generation, and the rules as
// JSONB TEXT — the exact value the generation trigger compares, which is what makes "a reorder is
// not a change" a claim about the stored bytes rather than about the caller's intent.
func storedBurn(t *testing.T, st *Store, ctx context.Context, targetID string) (bool, int64, string) {
	t.Helper()
	var enabled bool
	var generation int64
	var rules string
	if err := st.pool.QueryRow(ctx, `
		SELECT burn_alert_enabled, alert_generation, burn_rules::text
		  FROM sla_targets WHERE id = $1`, targetID).Scan(&enabled, &generation, &rules); err != nil {
		t.Fatalf("read stored burn declaration: %v", err)
	}
	return enabled, generation, rules
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

	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: false, PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown},
		ConfirmEvaluations: 2}), AlertActor{ViaToken: true})
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

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDegraded)), AlertActor{}); err != nil {
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

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDegraded, domain.ServiceAlertDown)), AlertActor{}); err != nil {
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

	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: true,
		PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown}, ConfirmEvaluations: 5}),
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
// The burn conjunction has two halves: one replacement must EXIST, and NOTHING the service owns
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
	// The surviving rule's announcement reached somebody (D-0179): this test is about the REMOVED
	// rule's latch, and without the credit it would pass or fail for an unrelated reason.
	creditBurnDeliveries(t, st, ctx, f.serviceID)
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

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDegraded)), AlertActor{}); !errors.Is(err, ErrServiceManagedByFile) {
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

	if _, err := st.UpdateServiceAlertPolicy(ctx, other.ID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDegraded)), AlertActor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant policy write = %v, want ErrNotFound", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, other.ID, f.serviceID, "30d", false,
		oneBurnRuleSet(), AlertActor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant burn write = %v, want ErrNotFound", err)
	}
	// An id that is not even a uuid answers the same way rather than surfacing a driver error.
	if _, err := st.UpdateServiceAlertPolicy(ctx, "not-a-uuid", f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDown)), AlertActor{}); !errors.Is(err, ErrNotFound) {
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

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: true,
		PageOn: []domain.ServiceAlertState{domain.ServiceAlertUnknown}, ConfirmEvaluations: 2}),
		AlertActor{}); err == nil {
		t.Fatal("page_on accepted `unknown`, which has its own switch")
	}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: true,
		PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown}, ConfirmEvaluations: 99}),
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
	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDown, domain.ServiceAlertDegraded, domain.ServiceAlertDown)),
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

// The OTHER half of the pageability declaration, and the half every test above narrows around:
// `page_on_unknown` true→false is exactly as destructive as narrowing `page_on`. A service
// announcing that nobody can see it is announcing a state the new policy no longer covers, so the
// episode has to end here — nothing else ever will, because after the commit the evaluator judges
// `unknown` as unpageable and has nothing to close. And it ends as `policy_changed`: the service did
// not become visible, the operator stopped asking to hear about it, so `recovered` would assert
// evidence that does not exist.
func TestClearingPageOnUnknownClosesTheAnnouncementItNoLongerCovers(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	// Declared through the write surface itself, so the true→false edit below is judged against a
	// value this path stored rather than one a test wrote behind its back.
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: true,
		PageOn:        []domain.ServiceAlertState{domain.ServiceAlertDown},
		PageOnUnknown: true, ConfirmEvaluations: 2}), AlertActor{}); err != nil {
		t.Fatalf("declare page_on_unknown: %v", err)
	}
	// No heartbeats at all: nothing is decidable, so the service is UNKNOWN rather than down.
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	onset := alertEvents(t, st, ctx)
	if len(onset) != 1 || !onset[0].Firing || onset[0].State != domain.ServiceAlertUnknown {
		t.Fatalf("expected one UNKNOWN onset, got %+v", onset)
	}

	// Only `page_on_unknown` moves: `page_on` stays {down} and the confirmation stays 2, so the close
	// below cannot be attributed to any other field.
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDown)), AlertActor{}); err != nil {
		t.Fatalf("clear page_on_unknown: %v", err)
	}

	events := alertEvents(t, st, ctx)
	if len(events) != 2 {
		t.Fatalf("%d events, want the onset and ONE close — an UNKNOWN announcement left open by "+
			"the switch that produced it is one nothing can ever end", len(events))
	}
	got := events[1]
	switch {
	case got.Firing:
		t.Fatal("clearing page_on_unknown published another onset")
	case got.CloseReason != domain.ClosePolicyChanged:
		t.Fatalf("close reason = %q, want policy_changed — the service is still unmeasurable, so "+
			"nothing here is evidence that it recovered", got.CloseReason)
	case got.Signal != domain.ServiceSignalHealth || got.State != domain.ServiceAlertUnknown:
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
		t.Fatal("the live latch is still FIRING with no open episode: re-declaring page_on_unknown " +
			"would swallow the next onset as 'no edge'")
	}
	audits := auditTargets(t, st, ctx, "service.alerting")
	if len(audits) != 2 || !strings.Contains(audits[1], "page_on_unknown:true→false") {
		t.Fatalf("audit = %v, want the page_on_unknown before→after", audits)
	}
	if strings.Contains(audits[1], "page_on:") || strings.Contains(audits[1], "confirm_evaluations") {
		t.Fatalf("audit target = %q, want ONLY the field that moved", audits[1])
	}
}

// Disowning a service that is firing on BOTH signals: one edit, TWO announcements to end, and each
// one is a separate promise to a separate set of people. The failure this pins is cardinality — one
// close covering both episodes leaves one announcement open forever, and two closes for one episode
// tells the same recipients twice that something ended once.
//
// The recipient snapshots are deliberately made to DIFFER, by rotating the schedule between the two
// onsets. With one route for both, "each close reached its own onset's people" is not a claim this
// test could ever fail.
func TestDisowningClosesEachOpenSignalExactlyOnce(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	live := alertFixture{projectID: f.projectID, serviceID: f.serviceID, monitorID: f.monitorID}

	rota := liveChannels(t, st, ctx, f.projectID, "burn-oncall", "live-oncall")
	burnChan, liveChan := rota[0], rota[1]
	var schedule string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at, participants)
		VALUES ($1,'primary',604800,now(),jsonb_build_array($2::text)) RETURNING id`,
		f.projectID, burnChan).Scan(&schedule); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET oncall_schedule_id = $2 WHERE id = $1`, f.serviceID, schedule); err != nil {
		t.Fatalf("attach schedule: %v", err)
	}

	// (a) the burn announcement, to "ch-burn".
	plantBurn(t, st, ctx, f, 5, minute/60) // ~16.7×, over the rule's threshold of 14
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want the burn rule firing", got.Onsets)
	}
	burnOnset := burnEventsFor(t, st, ctx, f.targetID)
	if len(burnOnset) != 1 || len(burnOnset[0].Recipients) != 1 ||
		burnOnset[0].Recipients[0] != burnChan {
		t.Fatalf("burn onset = %+v, want one alert to the on-call channel %s", burnOnset, burnChan)
	}

	// (b) the rotation, and then the LIVE announcement, to somebody else entirely.
	if _, err := st.pool.Exec(ctx,
		`UPDATE oncall_schedules SET participants = jsonb_build_array($2::text) WHERE id = $1`,
		schedule, liveChan); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	setMemberHealth(t, st, ctx, live, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	var healthOnset domain.ServiceAlert
	for _, e := range allServiceAlerts(t, st, ctx) {
		if e.Firing && e.Signal == domain.ServiceSignalHealth {
			healthOnset = e
		}
	}
	if healthOnset.EpisodeID == "" || len(healthOnset.Recipients) != 1 ||
		healthOnset.Recipients[0] != liveChan {
		t.Fatalf("health onset = %+v, want one alert to the rotated channel %s", healthOnset, liveChan)
	}
	if healthOnset.EpisodeID == burnOnset[0].EpisodeID {
		t.Fatal("the two signals share an episode: this fixture cannot tell two closes from one")
	}

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: false,
		PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDown},
		ConfirmEvaluations: 2}), AlertActor{}); err != nil {
		t.Fatalf("disown: %v", err)
	}

	events := allServiceAlerts(t, st, ctx)
	if len(events) != 4 {
		t.Fatalf("%d events, want two onsets and ONE close per signal", len(events))
	}
	closes := map[domain.ServiceAlertSignal][]domain.ServiceAlert{}
	perEpisode := map[string]int{}
	for _, e := range events {
		if e.Firing {
			continue
		}
		closes[e.Signal] = append(closes[e.Signal], e)
		perEpisode[e.EpisodeID]++
	}
	if len(closes[domain.ServiceSignalBurn]) != 1 || len(closes[domain.ServiceSignalHealth]) != 1 {
		t.Fatalf("closes per signal = %d burn / %d health, want exactly one each: a single close "+
			"covering both leaves one announcement open forever",
			len(closes[domain.ServiceSignalBurn]), len(closes[domain.ServiceSignalHealth]))
	}
	for id, n := range perEpisode {
		if n != 1 {
			t.Fatalf("episode %q was closed %d times: its recipients are told twice that one "+
				"announcement ended once", id, n)
		}
	}
	for _, tc := range []struct {
		name  string
		got   domain.ServiceAlert
		onset domain.ServiceAlert
	}{
		{"burn", closes[domain.ServiceSignalBurn][0], burnOnset[0]},
		{"health", closes[domain.ServiceSignalHealth][0], healthOnset},
	} {
		switch {
		case tc.got.CloseReason != domain.CloseOwnershipDisabled:
			t.Fatalf("%s close reason = %q, want ownership_disabled", tc.name, tc.got.CloseReason)
		case tc.got.EpisodeID != tc.onset.EpisodeID:
			t.Fatalf("the %s close ends episode %q, want its own onset's %q",
				tc.name, tc.got.EpisodeID, tc.onset.EpisodeID)
		case len(tc.got.Recipients) != 1 || tc.got.Recipients[0] != tc.onset.Recipients[0]:
			t.Fatalf("%s close recipients = %v, want its OWN onset's snapshot %v — the two signals "+
				"were announced to different people", tc.name, tc.got.Recipients, tc.onset.Recipients)
		case tc.got.Seq <= tc.onset.Seq:
			t.Fatalf("%s close seq %d does not follow its onset's %d", tc.name, tc.got.Seq, tc.onset.Seq)
		}
	}

	// Both episodes are closed and both latches are cleared: a level left set with no open episode
	// would swallow the next onset as "no edge" once ownership came back.
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 0 {
		t.Fatalf("%d burn episodes still open after disowning", n)
	}
	if n := openHealthEpisodes(t, st, ctx, f.serviceID); n != 0 {
		t.Fatalf("%d health episodes still open after disowning", n)
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); s.firing {
		t.Fatal("the burn latch is still FIRING with no open episode")
	}
	if liveFiring(t, st, ctx, f.serviceID) {
		t.Fatal("the live latch is still FIRING with no open episode")
	}
	// One edit, one audit line — two closes do not make two configuration changes.
	if audits := auditTargets(t, st, ctx, "service.alerting"); len(audits) != 1 ||
		!strings.Contains(audits[0], "owns_paging:true→false") {
		t.Fatalf("audit = %v, want exactly one row naming the ownership flip", audits)
	}
}

// §16.6a requires every paging-config change to carry its ACTOR, and the actor is three columns, not
// a sentence in `target`: who (or NULL for a machine principal), whether the principal was an API
// token, and the tenant the row belongs to. A row that names the change but not the changer answers
// nothing the audit log exists to answer, and one filed under the wrong org is invisible to the only
// people entitled to read it.
func TestPagingWritesAuditTheirActor(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	// A decoy org and project, so "the service's own org" is a claim that can fail: with one org in
	// the table any value passes.
	decoy, err := st.CreateOrganization(ctx, "decoy", "Decoy")
	if err != nil {
		t.Fatalf("decoy org: %v", err)
	}
	if _, err := st.CreateProject(ctx, decoy.ID, "elsewhere", "Elsewhere"); err != nil {
		t.Fatalf("decoy project: %v", err)
	}
	f := burnAlertService(t, st, ctx, oneBurnRule)

	// `actor_user_id` is a soft FK to a real row (00018), so the human actor has to exist.
	var userID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name) VALUES ('ops@example.com','Ops') RETURNING id`).
		Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}

	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: true,
		PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDown},
		ConfirmEvaluations: 5}), AlertActor{ActorUserID: userID}); err != nil {
		t.Fatalf("policy edit by a human: %v", err)
	}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: true,
		PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDown},
		ConfirmEvaluations: 6}), AlertActor{ViaToken: true}); err != nil {
		t.Fatalf("policy edit by a token: %v", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		twoBurnRuleSet(), AlertActor{ActorUserID: userID}); err != nil {
		t.Fatalf("burn edit by a human: %v", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", false,
		twoBurnRuleSet(), AlertActor{ViaToken: true}); err != nil {
		t.Fatalf("burn edit by a token: %v", err)
	}

	var orgID string
	if err := st.pool.QueryRow(ctx,
		`SELECT org_id FROM projects WHERE id = $1`, f.projectID).Scan(&orgID); err != nil {
		t.Fatalf("read org: %v", err)
	}
	if orgID == decoy.ID {
		t.Fatal("the fixture is wrong: the decoy org must not be the service's own")
	}
	for _, action := range []string{"service.alerting", "service.burn_alerting"} {
		rows := alertAuditRows(t, st, ctx, action)
		if len(rows) != 2 {
			t.Fatalf("%s audit rows = %d, want the human's and the machine's", action, len(rows))
		}
		human, machine := rows[0], rows[1]
		switch {
		case human.actorID == nil || *human.actorID != userID:
			t.Fatalf("%s: actor_user_id = %v, want the user that made the change — a change with no "+
				"changer answers nothing the audit log exists to answer", action, human.actorID)
		case human.viaToken:
			t.Fatalf("%s: a session principal was recorded as via_token", action)
		case machine.actorID != nil:
			t.Fatalf("%s: an empty ActorUserID stored %q, want NULL — a machine actor must not be "+
				"attributed to a person", action, *machine.actorID)
		case !machine.viaToken:
			t.Fatalf("%s: a token principal was not marked via_token, so a service account reads as "+
				"an interactive operator", action)
		}
		for _, r := range rows {
			if r.orgID != orgID {
				t.Fatalf("%s: org_id = %q, want the service's own org %q — a row filed under another "+
					"tenant is invisible to the only people entitled to read it", action, r.orgID, orgID)
			}
			if !strings.Contains(r.target, "service="+f.serviceID) {
				t.Fatalf("%s: target = %q, want the service it is about", action, r.target)
			}
		}
	}
}

// A save that changes nothing must BE nothing. This is the pin that keeps the UI's "Save" — pressed
// on a form nobody edited, or replayed by a MaC apply — from bumping the generations, dis-arming the
// service and paging every member for a change that was never made; and from closing announcements
// that are still true.
//
// Both spellings are exercised: re-sending the same declaration, and re-sending a REORDERED one. The
// second is the load-bearing half, because both generations are owned by triggers that compare
// stored values, so "a reorder is not a change" is a property of the canonical form this surface
// writes, not of the caller's discipline.
// A no-op must not even STAMP the row: an UPDATE that changes no column still moves `updated_at`
// and burns an MVCC row version, and a UI saving an unchanged form would then show a service as
// freshly edited by nobody.
func TestSemanticNoOpDoesNotEvenStampTheRow(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)

	policy := domain.ServiceAlertPolicy{
		OwnsPaging: true, PageOn: []domain.ServiceAlertState{domain.ServiceAlertDown},
		ConfirmEvaluations: 2,
	}
	rules := []domain.BurnRule{{
		LongWindowSeconds: 300, ShortWindowSeconds: 120, Threshold: 14,
		Severity: domain.BurnSeverityPage,
	}}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		FullServiceAlertPolicyPatch(policy), AlertActor{}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true, rules,
		AlertActor{}); err != nil {
		t.Fatalf("seed rules: %v", err)
	}

	stamps := func() (svc, target time.Time) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `
			SELECT s.updated_at, t.updated_at
			  FROM services s JOIN sla_targets t ON t.id = $2
			 WHERE s.id = $1`, f.serviceID, f.targetID).Scan(&svc, &target); err != nil {
			t.Fatalf("read stamps: %v", err)
		}
		return
	}
	svcBefore, targetBefore := stamps()

	// The same declarations again, the rules re-spelled in the other order.
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		FullServiceAlertPolicyPatch(policy), AlertActor{}); err != nil {
		t.Fatalf("re-apply policy: %v", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true, rules,
		AlertActor{}); err != nil {
		t.Fatalf("re-apply rules: %v", err)
	}

	svcAfter, targetAfter := stamps()
	if !svcAfter.Equal(svcBefore) {
		t.Fatalf("services.updated_at moved %s → %s for a declaration nobody changed",
			svcBefore, svcAfter)
	}
	if !targetAfter.Equal(targetBefore) {
		t.Fatalf("sla_targets.updated_at moved %s → %s for rules nobody changed",
			targetBefore, targetAfter)
	}
}

func TestSemanticNoOpWritesChangeNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, twoRulesJSON)
	live := alertFixture{projectID: f.projectID, serviceID: f.serviceID, monitorID: f.monitorID}

	// Both signals firing, so "closes nothing" is a claim with something to lose.
	plantBurn(t, st, ctx, f, 5, 2*minute/60) // 33.3×: over BOTH thresholds
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 2 {
		t.Fatalf("onsets = %d, want both burn rules firing", got.Onsets)
	}
	setMemberHealth(t, st, ctx, live, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	if n := len(allServiceAlerts(t, st, ctx)); n != 3 {
		t.Fatalf("%d announcements, want two burn onsets and one health onset", n)
	}

	// Both declarations are first written THROUGH the surface, so what follows is compared against
	// the canonical spelling this path stores rather than against the fixture's.
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDegraded, domain.ServiceAlertDown)), AlertActor{}); err != nil {
		t.Fatalf("declare policy: %v", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		twoBurnRuleSet(), AlertActor{}); err != nil {
		t.Fatalf("declare rules: %v", err)
	}

	policy := storedPolicy(t, st, ctx, f.serviceID)
	generation := alertConfigGeneration(t, st, ctx, f.serviceID)
	enabled, targetGeneration, rules := storedBurn(t, st, ctx, f.targetID)
	policyAudits := len(auditTargets(t, st, ctx, "service.alerting"))
	burnAudits := len(auditTargets(t, st, ctx, "service.burn_alerting"))
	events := len(allServiceAlerts(t, st, ctx))

	unchanged := func(after string) {
		t.Helper()
		// The generations FIRST: they are what a drifting write actually costs, and a stored value
		// that moved without moving them would still be a bug this helper reports below.
		if got := alertConfigGeneration(t, st, ctx, f.serviceID); got != generation {
			t.Fatalf("%s: alert_config_generation %d → %d — the service dis-arms and every member "+
				"pages for itself, for a change nobody made", after, generation, got)
		}
		gotEnabled, gotGeneration, gotRules := storedBurn(t, st, ctx, f.targetID)
		if gotGeneration != targetGeneration {
			t.Fatalf("%s: sla_targets.alert_generation %d → %d — burn coverage dis-arms until every "+
				"rule has been re-evaluated, for a change nobody made",
				after, targetGeneration, gotGeneration)
		}
		if got := storedPolicy(t, st, ctx, f.serviceID); got.OwnsPaging != policy.OwnsPaging ||
			got.PageOnUnknown != policy.PageOnUnknown ||
			got.ConfirmEvaluations != policy.ConfirmEvaluations ||
			statesText(got.PageOn) != statesText(policy.PageOn) {
			t.Fatalf("%s: stored policy %+v, want the identical canonical value %+v", after, got, policy)
		}
		if gotEnabled != enabled || gotRules != rules {
			t.Fatalf("%s: stored burn declaration enabled=%v rules=%s, want the identical canonical "+
				"value enabled=%v rules=%s", after, gotEnabled, gotRules, enabled, rules)
		}
		if got := len(auditTargets(t, st, ctx, "service.alerting")); got != policyAudits {
			t.Fatalf("%s: %d service.alerting audit rows, want %d — a no-op edit is not a decision "+
				"and an audit reader must not be trained to skim", after, got, policyAudits)
		}
		if got := len(auditTargets(t, st, ctx, "service.burn_alerting")); got != burnAudits {
			t.Fatalf("%s: %d service.burn_alerting audit rows, want %d", after, got, burnAudits)
		}
		if got := len(allServiceAlerts(t, st, ctx)); got != events {
			t.Fatalf("%s: %d announcements, want %d — a save without changes ended an announcement "+
				"that is still true", after, got, events)
		}
		if n := openHealthEpisodes(t, st, ctx, f.serviceID); n != 1 {
			t.Fatalf("%s: %d open health episodes, want the announcement untouched", after, n)
		}
		if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 2 {
			t.Fatalf("%s: %d open burn episodes, want both untouched", after, n)
		}
		if !liveFiring(t, st, ctx, f.serviceID) {
			t.Fatalf("%s: the live latch was cleared by an edit that changed nothing", after)
		}
		for _, key := range []string{oneBurnRuleKey, ticketBurnRuleKey} {
			if s := burnState(t, st, ctx, f.targetID, key); !s.firing {
				t.Fatalf("%s: the latch for %s was cleared by an edit that changed nothing", after, key)
			}
		}
	}
	unchanged("baseline")

	// (a) the same policy, spelled in the other order.
	stored, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDown, domain.ServiceAlertDegraded)), AlertActor{})
	if err != nil {
		t.Fatalf("rewrite the same policy: %v", err)
	}
	if statesText(stored.PageOn) != statesText(policy.PageOn) {
		t.Fatalf("the returned policy %v is not the stored one %v",
			statesText(stored.PageOn), statesText(policy.PageOn))
	}
	unchanged("after rewriting the same policy")

	// (b) the same rules, in the same order.
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		twoBurnRuleSet(), AlertActor{}); err != nil {
		t.Fatalf("rewrite the same rules: %v", err)
	}
	unchanged("after rewriting the same rules")

	// (c) the same rules, REORDERED — the spelling a UI or a bundle can change without anybody
	// editing anything.
	reordered := twoBurnRuleSet()
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", true,
		reordered, AlertActor{}); err != nil {
		t.Fatalf("reorder the rules: %v", err)
	}
	unchanged("after reordering the same rules")
}

// One answer for four different ways of naming something that is not yours: a foreign project, a
// project id that is not a uuid, a foreign service, and a service id that is not a uuid. They must be
// indistinguishable, or the difference between "not found" and "invalid" becomes a cross-tenant
// existence oracle — and a driver error leaking out would additionally be a 500 where the API owes a
// 404. Both arguments of both methods, because the pair is what addresses the row: scoping by only
// one of them is the mistake this matrix exists to catch, and it is a WRITE mistake, so every case
// also has to leave both tenants' rows exactly as it found them.
func TestPagingWritesRefuseForeignAndMalformedIdentifiers(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want a firing rule the refused writes could destroy", got.Onsets)
	}

	// A second tenant with its own service and its own enabled burn target: the row a write that
	// scoped by service alone — or by project alone — would reach.
	other, err := st.CreateProject(ctx, f.orgID, "other", "Other")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	otherSvc, err := st.CreateService(ctx, domain.Service{
		ProjectID: other.ID, Slug: "orders", Name: "Orders",
	})
	if err != nil {
		t.Fatalf("foreign service: %v", err)
	}
	// Owning and burn-enabled, so BOTH refused writes would visibly land on it if either one were
	// scoped by the service id alone.
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET owns_paging = true WHERE id = $1`, otherSvc.ID); err != nil {
		t.Fatalf("own the foreign service: %v", err)
	}
	var otherTarget string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1,'30d',99.9,true,$2::jsonb) RETURNING id`,
		otherSvc.ID, oneBurnRule).Scan(&otherTarget); err != nil {
		t.Fatalf("foreign target: %v", err)
	}

	policy := storedPolicy(t, st, ctx, f.serviceID)
	generation := alertConfigGeneration(t, st, ctx, f.serviceID)
	enabled, targetGeneration, rules := storedBurn(t, st, ctx, f.targetID)

	for _, tc := range []struct {
		name, projectID, serviceID string
	}{
		{"foreign project", other.ID, f.serviceID},
		{"malformed project", "not-a-uuid", f.serviceID},
		{"foreign service", f.projectID, otherSvc.ID},
		{"malformed service", f.projectID, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both edits are destructive if they land: the policy disowns the service, and the burn
			// write turns its target off. Both would close a firing announcement.
			if _, err := st.UpdateServiceAlertPolicy(ctx, tc.projectID, tc.serviceID, FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{OwnsPaging: false,
				PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDown},
				ConfirmEvaluations: 2}), AlertActor{ViaToken: true}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("policy write = %v, want ErrNotFound — one answer for every way of naming "+
					"something that is not yours", err)
			}
			if err := st.SetServiceBurnAlerting(ctx, tc.projectID, tc.serviceID, "30d", false,
				oneBurnRuleSet(), AlertActor{ViaToken: true}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("burn write = %v, want ErrNotFound", err)
			}
		})
	}

	// Nothing moved on the addressed tenant: not the declaration, not either generation, not the
	// announcement, not the latch.
	if got := storedPolicy(t, st, ctx, f.serviceID); got.OwnsPaging != policy.OwnsPaging ||
		got.PageOnUnknown != policy.PageOnUnknown ||
		got.ConfirmEvaluations != policy.ConfirmEvaluations ||
		statesText(got.PageOn) != statesText(policy.PageOn) {
		t.Fatalf("a refused write changed the policy to %+v, want %+v", got, policy)
	}
	if got := alertConfigGeneration(t, st, ctx, f.serviceID); got != generation {
		t.Fatalf("alert_config_generation %d → %d on refused writes", generation, got)
	}
	gotEnabled, gotGeneration, gotRules := storedBurn(t, st, ctx, f.targetID)
	if gotEnabled != enabled || gotRules != rules || gotGeneration != targetGeneration {
		t.Fatalf("a refused write changed the burn declaration to enabled=%v gen=%d rules=%s, want "+
			"enabled=%v gen=%d rules=%s", gotEnabled, gotGeneration, gotRules,
			enabled, targetGeneration, rules)
	}
	if events := allServiceAlerts(t, st, ctx); len(events) != 1 {
		t.Fatalf("%d events, want only the onset — a refused write ended an announcement", len(events))
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("open episodes = %d, want the announcement untouched", n)
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); !s.firing {
		t.Fatal("a refused write cleared the firing latch")
	}
	if a := auditTargets(t, st, ctx, "service.alerting"); len(a) != 0 {
		t.Fatalf("a refused write audited %v", a)
	}
	if a := auditTargets(t, st, ctx, "service.burn_alerting"); len(a) != 0 {
		t.Fatalf("a refused write audited %v", a)
	}

	// ...and nothing moved on the OTHER tenant either, which is the half that catches a write scoped
	// by service id alone.
	var foreignOwns, foreignEnabled bool
	if err := st.pool.QueryRow(ctx, `
		SELECT s.owns_paging, t.burn_alert_enabled
		  FROM services s JOIN sla_targets t ON t.id = $2 WHERE s.id = $1`,
		otherSvc.ID, otherTarget).Scan(&foreignOwns, &foreignEnabled); err != nil {
		t.Fatalf("read the foreign declaration: %v", err)
	}
	if !foreignOwns || !foreignEnabled {
		t.Fatalf("a write addressed with THIS project's id reached the other tenant's rows: "+
			"owns_paging=%v burn_alert_enabled=%v", foreignOwns, foreignEnabled)
	}
}

// The READ half. A PATCH merges onto it, so it has to answer with the canonical stored value and it
// has to refuse a foreign or malformed id exactly as the writers do — a merge base that leaked
// across a tenant, or that was invented because the read failed open, is how "leave ownership alone"
// becomes a silent disowning.
func TestServiceAlertPolicyReadIsCanonicalAndTenantScoped(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// Stored deliberately UNSORTED and with a repeat, which the write path would have
	// canonicalized — a direct edit is how a row gets into this shape.
	if _, err := st.pool.Exec(ctx, `
		UPDATE services SET owns_paging = true, page_on = '{degraded,down,degraded}',
		       page_on_unknown = true, confirm_evaluations = 5
		 WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.ServiceAlertPolicy(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.PageOn) != 2 || got.PageOn[0] != domain.ServiceAlertDegraded ||
		got.PageOn[1] != domain.ServiceAlertDown {
		t.Fatalf("page_on = %v, want the canonical sorted, deduplicated set", got.PageOn)
	}
	if !got.OwnsPaging || !got.PageOnUnknown || got.ConfirmEvaluations != 5 {
		t.Fatalf("read did not carry the declaration: %+v", got)
	}

	for _, tc := range []struct{ name, project, service string }{
		{"foreign project", f.serviceID, f.serviceID},
		{"malformed project", "not-a-uuid", f.serviceID},
		{"foreign service", f.projectID, f.projectID},
		{"malformed service", f.projectID, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.ServiceAlertPolicy(ctx, tc.project, tc.service); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s answered %v, want ErrNotFound — one answer for all of them, so "+
					"existence never leaks across a tenant", tc.name, err)
			}
		})
	}
}

// Every field of the declaration goes through the WRITE PATH, not just the ones a test happened to
// set with direct SQL.
//
// `alertPolicyDiff` is both the audit line and the no-op gate: a field missing from it means an edit
// to that field reports 200, echoes the new value back, and commits nothing. `renotify_seconds`
// shipped that way, and every test of it wrote the column with `UPDATE services` — so the feature was
// pinned everywhere except the one place an operator uses it.
//
// This walks each field through `UpdateServiceAlertPolicy` and reads the row back, which is the shape
// that would have caught it.
func TestEveryPolicyFieldSurvivesTheWritePath(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	stored := func(t *testing.T) domain.ServiceAlertPolicy {
		t.Helper()
		p, err := st.ServiceAlertPolicy(ctx, f.projectID, f.serviceID)
		if err != nil {
			t.Fatalf("read policy: %v", err)
		}
		return p
	}

	for _, tc := range []struct {
		name  string
		patch ServiceAlertPolicyPatch
		want  func(domain.ServiceAlertPolicy) bool
		got   func(domain.ServiceAlertPolicy) string
	}{
		{
			name:  "renotify_seconds",
			patch: ServiceAlertPolicyPatch{RenotifySeconds: intPtr(900)},
			want:  func(p domain.ServiceAlertPolicy) bool { return p.RenotifySeconds == 900 },
			got:   func(p domain.ServiceAlertPolicy) string { return strconv.Itoa(p.RenotifySeconds) },
		},
		{
			name:  "confirm_evaluations",
			patch: ServiceAlertPolicyPatch{ConfirmEvaluations: intPtr(5)},
			want:  func(p domain.ServiceAlertPolicy) bool { return p.ConfirmEvaluations == 5 },
			got:   func(p domain.ServiceAlertPolicy) string { return strconv.Itoa(p.ConfirmEvaluations) },
		},
		{
			name:  "page_on_unknown",
			patch: ServiceAlertPolicyPatch{PageOnUnknown: boolPtr(true)},
			want:  func(p domain.ServiceAlertPolicy) bool { return p.PageOnUnknown },
			got:   func(p domain.ServiceAlertPolicy) string { return fmt.Sprint(p.PageOnUnknown) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID, tc.patch,
				AlertActor{}); err != nil {
				t.Fatalf("update: %v", err)
			}
			if p := stored(t); !tc.want(p) {
				t.Fatalf("%s reads back as %s after a write that returned success: the field is "+
					"missing from `alertPolicyDiff`, so the write was short-circuited as a no-op",
					tc.name, tc.got(p))
			}
		})
	}

	// And the audit trail names the field, which is the other half of the same function: the diff
	// text IS the audit target, so a field missing from it is invisible in the log as well as
	// unwritten.
	var target string
	if err := st.pool.QueryRow(ctx, `
		SELECT target FROM audit_logs
		 WHERE action LIKE 'service%alert%'
		 ORDER BY created_at DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(target, "page_on_unknown") {
		t.Fatalf("the last paging edit was audited as %q, which does not name what changed", target)
	}
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }
