package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.4 / §16.4a / §16.4b — the SEALED evaluator end to end: what it announces, what it
// refuses to announce, and what it does when it cannot see. These run against the REAL burn math
// owner over REAL sealed facts, so a change that let the pager quote a number the reporting card
// would refuse fails here rather than in production.

const burnCadence = 30 * time.Second

// oneBurnRule is the rule every test that only needs one uses: 5-minute long window, 2-minute short
// window. Short windows keep the planted fact range small; nothing in the evaluator cares how long
// they are. Canonical key: "page/300/120/14".
const oneBurnRule = `[{"long_window_seconds":300,"short_window_seconds":120,` +
	`"threshold":14,"severity":"page"}]`

const oneBurnRuleKey = "page/300/120/14"

// burnFixture is an owning service with a service-scoped burn target whose sealed facts the test
// plants directly. `report` exists only so the phase-2 fact helpers (plantRange/setWatermark) can be
// reused rather than re-implemented — a second way to write a sealed bucket is a second way to write
// one wrong.
type burnFixture struct {
	armFixture
	report  reportFixture
	epochID string
	// sealed is the watermark every window is anchored at: [sealed − w, sealed).
	sealed time.Time
	era    time.Time
}

func burnAlertService(t *testing.T, st *Store, ctx context.Context, rules string) burnFixture {
	t.Helper()
	f := armedService(t, st, ctx) // ownership, an effective declaration, a route
	// The evaluator writes its own latch; the arming fixture's stand-in row would otherwise hand the
	// first pass a level it never computed.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_burn_alert_state`); err != nil {
		t.Fatalf("clear burn state: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE sla_targets SET objective = 99.9, burn_rules = $2::jsonb WHERE id = $1`,
		f.targetID, rules); err != nil {
		t.Fatalf("rules: %v", err)
	}
	bf := burnFixture{
		armFixture: f,
		report: reportFixture{
			declFixture: declFixture{projectID: f.projectID, serviceID: f.serviceID},
		},
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT id FROM service_evaluation_epochs
		 WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`, f.serviceID).Scan(&bf.epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	// §11.3 holds a window whose equivalent real-time window contains no sealed time, so the
	// watermark has to be RECENT for any of these rules to be quotable at all.
	bf.sealed = time.Now().UTC().Truncate(time.Minute)
	bf.era = bf.sealed.Add(-time.Hour)
	setWatermark(t, st, ctx, bf.report, bf.era, bf.sealed)
	return bf
}

// plantBurn fills the whole long window with sealed buckets carrying one µs split, so both windows
// of a rule see the same rate. With objective 99.9 the allowed bad fraction is 0.001, so one bad
// second in every sixty is a burn rate of ~16.7×.
func plantBurn(t *testing.T, st *Store, ctx context.Context, f burnFixture, minutes int, badUs int64) {
	t.Helper()
	from := f.sealed.Add(-time.Duration(minutes) * time.Minute)
	plantRange(t, st, ctx, f.report, f.epochID, from, f.sealed, minute-badUs, badUs, 0, 0, "sealed")
}

func burnEvalOnce(t *testing.T, st *Store, ctx context.Context) ServiceBurnEvaluation {
	t.Helper()
	got, err := st.evaluateServiceBurnAlertsOn(ctx, st.pool, burnCadence)
	if err != nil {
		t.Fatalf("evaluate burn: %v", err)
	}
	return got
}

// burnEventsFor reads the alerts published for ONE target, ordered by the sequence they carry.
// Ordering by sequence rather than by row time is deliberate: two onsets enqueued in one
// transaction share a `created_at`, and the payload's own order is the one delivery uses.
func burnEventsFor(t *testing.T, st *Store, ctx context.Context, targetID string) []domain.ServiceAlert {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT payload FROM outbox_events
		 WHERE topic = 'service_alert' AND payload->>'sla_target_id' = $1
		 ORDER BY (payload->>'seq')::bigint`, targetID)
	if err != nil {
		t.Fatalf("read burn events: %v", err)
	}
	defer rows.Close()
	var out []domain.ServiceAlert
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan burn event: %v", err)
		}
		var a domain.ServiceAlert
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decode burn event: %v", err)
		}
		out = append(out, a)
	}
	return out
}

type burnStateRow struct {
	firing     bool
	verdict    string
	reason     string
	lastError  string
	seq        int64
	leaseFresh bool
}

func burnState(t *testing.T, st *Store, ctx context.Context, targetID, ruleKey string) burnStateRow {
	t.Helper()
	var r burnStateRow
	var reason, lastErr *string
	if err := st.pool.QueryRow(ctx, `
		SELECT firing, last_verdict, last_reason, last_error, emitted_seq, evaluated_at < lease_until
		  FROM service_burn_alert_state WHERE sla_target_id = $1 AND rule_key = $2`,
		targetID, ruleKey).Scan(&r.firing, &r.verdict, &reason, &lastErr, &r.seq, &r.leaseFresh); err != nil {
		t.Fatalf("burn state (%s/%s): %v", targetID, ruleKey, err)
	}
	if reason != nil {
		r.reason = *reason
	}
	if lastErr != nil {
		r.lastError = *lastErr
	}
	return r
}

func openBurnEpisodes(t *testing.T, st *Store, ctx context.Context, targetID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_alert_episodes
		 WHERE signal = 'burn' AND target_snapshot_id = $1 AND closed_at IS NULL`, targetID).Scan(&n); err != nil {
		t.Fatalf("count open episodes: %v", err)
	}
	return n
}

func nearly(got, want float64) bool { return math.Abs(got-want) < 0.05 }

// The onset: both windows breach, the rule fires ONCE, and the page carries everything a human needs
// to check the claim — including the watermark it was computed from, because this signal trails the
// seal by construction.
func TestBurnEvaluatorFiresOnceWhenBothWindowsBreach(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60) // 1s bad in 60 → 16.7× against a 0.1% budget

	got := burnEvalOnce(t, st, ctx)
	if got.Onsets != 1 || got.Targets != 1 || got.Rules != 1 || got.Holds != 0 || got.Errors != 0 {
		t.Fatalf("first pass = %+v, want exactly one onset over one rule", got)
	}
	events := burnEventsFor(t, st, ctx, f.targetID)
	if len(events) != 1 {
		t.Fatalf("events = %d, want exactly one onset", len(events))
	}
	e := events[0]
	switch {
	case e.Signal != domain.ServiceSignalBurn || !e.Firing:
		t.Fatalf("not a burn onset: %+v", e)
	case e.RuleKey != oneBurnRuleKey:
		t.Fatalf("rule_key = %q, want the canonical key %q", e.RuleKey, oneBurnRuleKey)
	case e.SLATargetID != f.targetID || e.Window != "30d":
		t.Fatalf("target identity = %q/%q, want %q/30d", e.SLATargetID, e.Window, f.targetID)
	case e.Severity != domain.BurnSeverityPage || !nearly(e.Threshold, 14):
		t.Fatalf("severity/threshold = %q/%v", e.Severity, e.Threshold)
	case e.WindowSeconds != 300 || e.ShortWindowSeconds != 120:
		t.Fatalf("windows = %d/%d, want 300/120", e.WindowSeconds, e.ShortWindowSeconds)
	case !nearly(e.Objective, 99.9) || !nearly(e.BurnRate, 16.667):
		t.Fatalf("objective/burn_rate = %v/%v, want 99.9/~16.7", e.Objective, e.BurnRate)
	case e.SealedThrough == nil || !e.SealedThrough.Equal(f.sealed):
		t.Fatalf("sealed_through = %v, want the watermark %v — a burn number without its basis "+
			"is exactly what §11.2 removed", e.SealedThrough, f.sealed)
	case e.EpisodeID == "":
		t.Fatal("no episode was opened, so the close would have no recipients to reach")
	case len(e.Recipients) == 0:
		t.Fatal("the onset resolved no recipients, so nobody would be paged")
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); !s.firing ||
		s.verdict != string(domain.BurnFire) || s.seq != 1 || !s.leaseFresh {
		t.Fatalf("state after the onset = %+v", s)
	}

	// Staying over the threshold is a LEVEL, not an edge: nothing further is announced.
	for i := 0; i < 2; i++ {
		if got := burnEvalOnce(t, st, ctx); got.Onsets != 0 || got.Closes != 0 {
			t.Fatalf("pass %d re-announced a rule that never stopped firing (%+v)", i, got)
		}
	}
	if events := burnEventsFor(t, st, ctx, f.targetID); len(events) != 1 {
		t.Fatalf("%d events after three passes over one continuous breach", len(events))
	}
}

// The close reaches the ONSET's recipients even after the schedule rotated, and a CLEAR edge names
// itself `recovered` — the only close this path is allowed to claim.
func TestBurnEvaluatorClosesToTheOnsetRecipients(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)

	chans := liveChannels(t, st, ctx, f.projectID, "first", "second")
	var schedule string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at, participants)
		VALUES ($1,'primary',604800,now(),jsonb_build_array($2::text)) RETURNING id`,
		f.projectID, chans[0]).Scan(&schedule); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET oncall_schedule_id = $2 WHERE id = $1`, f.serviceID, schedule); err != nil {
		t.Fatalf("attach schedule: %v", err)
	}

	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("no onset to close (%+v)", got)
	}
	onset := burnEventsFor(t, st, ctx, f.targetID)
	if len(onset[0].Recipients) != 1 || onset[0].Recipients[0] != chans[0] {
		t.Fatalf("onset recipients = %v, want the schedule's on-call", onset[0].Recipients)
	}

	// The rotation happens BEFORE the burn subsides.
	if _, err := st.pool.Exec(ctx,
		`UPDATE oncall_schedules SET participants = jsonb_build_array($2::text) WHERE id = $1`,
		schedule, chans[1]); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	plantBurn(t, st, ctx, f, 5, 0) // nothing bad at all
	if got := burnEvalOnce(t, st, ctx); got.Closes != 1 || got.Onsets != 0 {
		t.Fatalf("the burn subsided and the rule did not close (%+v)", got)
	}

	events := burnEventsFor(t, st, ctx, f.targetID)
	if len(events) != 2 {
		t.Fatalf("events = %d, want the onset and its close", len(events))
	}
	closeEvent := events[1]
	switch {
	case closeEvent.Firing:
		t.Fatalf("the second event is not a close: %+v", closeEvent)
	case closeEvent.CloseReason != domain.CloseRecovered:
		t.Fatalf("close reason = %q, want recovered", closeEvent.CloseReason)
	case closeEvent.RuleKey != oneBurnRuleKey || closeEvent.SLATargetID != f.targetID:
		t.Fatalf("the close does not name what ended: %+v", closeEvent)
	case closeEvent.Seq <= onset[0].Seq:
		t.Fatalf("close seq %d is not after the onset's %d", closeEvent.Seq, onset[0].Seq)
	case len(closeEvent.Recipients) != 1 || closeEvent.Recipients[0] != chans[0]:
		t.Fatalf("close recipients = %v, want the ONSET's snapshot — a rotation must not page a "+
			"stranger and leave the original recipient hanging", closeEvent.Recipients)
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 0 {
		t.Fatalf("%d episodes still open after the close", n)
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); s.firing ||
		s.verdict != string(domain.BurnClear) || s.seq != 2 {
		t.Fatalf("state after the close = %+v", s)
	}
}

// The most dangerous line in the feature: a window that cannot be quoted HOLDS, and a FIRING rule
// under a hold STAYS FIRING. Treating absent evidence as "burn = 0" would silently resolve a live
// alert — telling an operator the budget stopped burning when the truth is we stopped seeing it.
func TestBurnEvaluatorHoldsWithoutResolvingAFiringRule(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("no onset to hold over (%+v)", got)
	}

	// Punch a hole in the LONG window only — outside the 2-minute short one, so the hold is the long
	// window's own complaint rather than a short window reporting the same defect.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM service_reliability_buckets WHERE service_id = $1 AND bucket_start = $2`,
		f.serviceID, f.sealed.Add(-5*time.Minute)); err != nil {
		t.Fatalf("punch gap: %v", err)
	}

	got := burnEvalOnce(t, st, ctx)
	if got.Holds != 1 || got.Onsets != 0 || got.Closes != 0 {
		t.Fatalf("a gapped window did not hold quietly (%+v)", got)
	}
	if events := burnEventsFor(t, st, ctx, f.targetID); len(events) != 1 {
		t.Fatalf("%d events — a HOLD announced something", len(events))
	}
	s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey)
	switch {
	case !s.firing:
		t.Fatal("a HOLD resolved a firing rule: absent evidence was treated as burn = 0, which is " +
			"the most dangerous mistake available in this file")
	case s.verdict != string(domain.BurnHold):
		t.Fatalf("last_verdict = %q, want hold — the delegation query reads this to dis-arm", s.verdict)
	case s.reason != domain.ServiceReportReasonStorageGap:
		t.Fatalf("last_reason = %q, want %q", s.reason, domain.ServiceReportReasonStorageGap)
	case s.lastError != "":
		t.Fatalf("a HOLD recorded an error (%q): it is a SUCCESSFUL evaluation that cannot speak",
			s.lastError)
	case !s.leaseFresh:
		t.Fatal("a HOLD collapsed the lease; only an evaluation ERROR does that")
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("%d open episodes: the held rule's announcement must stay open", n)
	}
}

// Two rules on ONE target latch independently: one keeps firing while the other recovers, each with
// its own episode and its own sequence.
func TestBurnEvaluatorLatchesRulesIndependently(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	const twoRules = `[{"long_window_seconds":300,"short_window_seconds":120,` +
		`"threshold":14,"severity":"page"},` +
		`{"long_window_seconds":300,"short_window_seconds":120,` +
		`"threshold":30,"severity":"ticket"}]`
	const pageKey, ticketKey = "page/300/120/14", "ticket/300/120/30"
	f := burnAlertService(t, st, ctx, twoRules)

	plantBurn(t, st, ctx, f, 5, 2*minute/60) // 2s bad in 60 → 33.3×, over BOTH thresholds
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 2 || got.Rules != 2 || got.Targets != 1 {
		t.Fatalf("first pass = %+v, want both rules of one target firing", got)
	}

	plantBurn(t, st, ctx, f, 5, minute/60) // 16.7×: over 14, under 30
	got := burnEvalOnce(t, st, ctx)
	if got.Closes != 1 || got.Onsets != 0 {
		t.Fatalf("second pass = %+v, want only the 30× rule to recover", got)
	}

	page := burnState(t, st, ctx, f.targetID, pageKey)
	ticket := burnState(t, st, ctx, f.targetID, ticketKey)
	if !page.firing || page.verdict != string(domain.BurnFire) || page.seq != 1 {
		t.Fatalf("the 14× rule = %+v, want still firing on its original sequence", page)
	}
	if ticket.firing || ticket.verdict != string(domain.BurnClear) || ticket.seq != 2 {
		t.Fatalf("the 30× rule = %+v, want cleared with its own next sequence", ticket)
	}

	var open, closed int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE closed_at IS NULL), count(*) FILTER (WHERE closed_at IS NOT NULL)
		  FROM service_alert_episodes WHERE signal = 'burn' AND target_snapshot_id = $1`,
		f.targetID).Scan(&open, &closed); err != nil {
		t.Fatalf("episodes: %v", err)
	}
	if open != 1 || closed != 1 {
		t.Fatalf("episodes open/closed = %d/%d, want one of each — the rules share a target and "+
			"nothing else", open, closed)
	}
	// Three events: two onsets and one close, and the close names the rule that ended.
	events := burnEventsFor(t, st, ctx, f.targetID)
	if len(events) != 3 {
		t.Fatalf("events = %d, want two onsets and one close", len(events))
	}
	for _, e := range events {
		if e.Firing {
			continue
		}
		if e.RuleKey != ticketKey {
			t.Fatalf("the close names rule %q, want the 30× one", e.RuleKey)
		}
	}
}

// [contract] Two TARGETS of one service may legally carry the same canonical rule key: `sla_targets`
// is unique on (service_id, window_name) and the key spells severity/windows/threshold, naming no
// window. They are two independent alerts, and anything scoped by (service, rule) would collide them
// onto one episode, one sequence and one delivery fence.
func TestBurnEvaluatorSeparatesTargetsSharingARuleKey(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule) // the 30d target, objective 99.9

	// The SAME rule on a 7d target with a looser objective, so the two can diverge on identical facts.
	var weekID string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1,'7d',99.5,true,$2::jsonb) RETURNING id`, f.serviceID, oneBurnRule).Scan(&weekID); err != nil {
		t.Fatalf("second target: %v", err)
	}

	// 10% bad: 100× against the 0.1% budget, 20× against the 0.5% one. Both breach.
	plantBurn(t, st, ctx, f, 5, minute/10)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 2 || got.Targets != 2 || got.Rules != 2 {
		t.Fatalf("first pass = %+v, want one onset per target", got)
	}
	month := burnEventsFor(t, st, ctx, f.targetID)
	week := burnEventsFor(t, st, ctx, weekID)
	if len(month) != 1 || len(week) != 1 {
		t.Fatalf("events per target = %d/%d, want one each — the two targets share a rule key and "+
			"must not share an episode", len(month), len(week))
	}
	if month[0].RuleKey != week[0].RuleKey {
		t.Fatalf("the fixture is wrong: the two rules must share a canonical key (%q vs %q)",
			month[0].RuleKey, week[0].RuleKey)
	}
	if month[0].Window != "30d" || week[0].Window != "7d" {
		t.Fatalf("target windows = %q/%q, want 30d/7d — identical rules differ ONLY by their window",
			month[0].Window, week[0].Window)
	}
	if month[0].EpisodeID == week[0].EpisodeID {
		t.Fatal("both targets opened ONE episode: the close would bind the wrong alert")
	}
	if !nearly(month[0].BurnRate, 100) || !nearly(week[0].BurnRate, 20) {
		t.Fatalf("burn rates = %v/%v, want each target judged against its OWN objective",
			month[0].BurnRate, week[0].BurnRate)
	}

	// 2% bad: still 20× for the 30d target, down to 4× for the 7d one.
	plantBurn(t, st, ctx, f, 5, minute/50)
	if got := burnEvalOnce(t, st, ctx); got.Closes != 1 || got.Onsets != 0 {
		t.Fatalf("second pass = %+v, want exactly the 7d target to recover", got)
	}
	if week = burnEventsFor(t, st, ctx, weekID); len(week) != 2 || week[1].Firing {
		t.Fatalf("the 7d target's events = %+v, want its close", week)
	}
	if week[1].Window != "7d" || week[1].CloseReason != domain.CloseRecovered {
		t.Fatalf("the close names %q/%q, want the 7d target recovering",
			week[1].Window, week[1].CloseReason)
	}
	if len(burnEventsFor(t, st, ctx, f.targetID)) != 1 {
		t.Fatal("closing the 7d target also announced something for the 30d one")
	}
	if n := openBurnEpisodes(t, st, ctx, f.targetID); n != 1 {
		t.Fatalf("the 30d episode is not open (%d): the 7d close bound the wrong alert", n)
	}
	if n := openBurnEpisodes(t, st, ctx, weekID); n != 0 {
		t.Fatalf("the 7d episode is still open (%d) after its close", n)
	}
	if s := burnState(t, st, ctx, weekID, oneBurnRuleKey); s.firing || s.seq != 2 {
		t.Fatalf("the 7d latch = %+v, want cleared on its own sequence", s)
	}
	if s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey); !s.firing || s.seq != 1 {
		t.Fatalf("the 30d latch = %+v, want untouched by the other target's close", s)
	}
}

// A service that has sealed nothing cannot produce a sealed verdict. It writes the DIS-ARMING state
// — last_error set, lease collapsed — and invents no rate, because a burn signal computed from no
// watermark is a number with no basis.
func TestBurnEvaluatorDisarmsWithoutAWatermark(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_materialization SET sealed_through = NULL WHERE service_id = $1`,
		f.serviceID); err != nil {
		t.Fatalf("clear watermark: %v", err)
	}

	got := burnEvalOnce(t, st, ctx)
	if got.Errors != 1 || got.Targets != 0 || got.Onsets != 0 {
		t.Fatalf("evaluation = %+v, want one dis-armed target and nothing announced", got)
	}
	if events := burnEventsFor(t, st, ctx, f.targetID); len(events) != 0 {
		t.Fatalf("an unmeasurable service paged anyway: %+v", events)
	}
	s := burnState(t, st, ctx, f.targetID, oneBurnRuleKey)
	switch {
	case s.lastError == "":
		t.Fatal("no last_error recorded, so the delegation query would still arm on this row")
	case s.verdict != string(domain.BurnHold):
		t.Fatalf("last_verdict = %q, want hold", s.verdict)
	case s.reason != domain.ServiceReportReasonNothingSealed:
		t.Fatalf("last_reason = %q, want %q", s.reason, domain.ServiceReportReasonNothingSealed)
	case s.leaseFresh:
		t.Fatal("the lease did not collapse: an errored evaluation must dis-arm NOW, not coast on " +
			"the previous success until it would have expired")
	}
	// And the monitor underneath is paging for itself again, which is the whole point of dis-arming.
	if delegated(t, st, ctx, f.armFixture, DelegationBurn) {
		t.Fatal("a target that cannot be quoted still silenced its member's burn alert")
	}
}

// The slice is bounded and fair: with more burn TARGETS than the cap, one pass takes the cap and the
// next reaches the rest, because the order is by how long each has waited.
func TestBurnEvaluatorSliceIsBoundedAndFair(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_burn_alert_state`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE sla_targets SET burn_rules = $1::jsonb WHERE id = $2`,
		oneBurnRule, f.targetID); err != nil {
		t.Fatalf("rules: %v", err)
	}
	// Enough owning services with a burn target to exceed the cap — spread over several projects,
	// because a project caps its own service count and this test is about the EVALUATOR's bound.
	for i := 0; i < ServiceAlertSliceCap+5; i++ {
		projectID := f.projectID
		if i >= 40 {
			slug := fmt.Sprintf("extra-%d", i/40)
			proj, err := st.CreateProject(ctx, f.orgID, slug, "Extra")
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Fatalf("project: %v", err)
			}
			if err == nil {
				projectID = proj.ID
			} else if err := st.pool.QueryRow(ctx,
				`SELECT id FROM projects WHERE org_id = $1 AND slug = $2`,
				f.orgID, slug).Scan(&projectID); err != nil {
				t.Fatalf("resolve project: %v", err)
			}
		}
		svc, err := st.CreateService(ctx, domain.Service{
			ProjectID: projectID, Slug: fmt.Sprintf("svc-%03d", i), Name: fmt.Sprintf("Svc %d", i),
		})
		if err != nil {
			t.Fatalf("service %d: %v", i, err)
		}
		if _, err := st.pool.Exec(ctx,
			`UPDATE services SET owns_paging = true WHERE id = $1`, svc.ID); err != nil {
			t.Fatalf("own %d: %v", i, err)
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
			VALUES ($1,'30d',99.9,true,$2::jsonb)`, svc.ID, oneBurnRule); err != nil {
			t.Fatalf("target %d: %v", i, err)
		}
	}

	// None of these services has sealed anything, so every one dis-arms — which is still an
	// EVALUATION of that target and still consumes a slot, which is exactly what the bound must hold.
	first := burnEvalOnce(t, st, ctx)
	if first.Targets+first.Errors != ServiceAlertSliceCap {
		t.Fatalf("first slice reached %d targets, want the cap %d",
			first.Targets+first.Errors, ServiceAlertSliceCap)
	}
	second := burnEvalOnce(t, st, ctx)
	if second.Targets+second.Errors == 0 {
		t.Fatal("the second pass reached nothing: targets beyond the cap would starve")
	}
	var unevaluated int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM sla_targets t
		  JOIN services s ON s.id = t.service_id
		 WHERE s.owns_paging AND t.burn_alert_enabled
		   AND NOT EXISTS (SELECT 1 FROM service_burn_alert_state b WHERE b.sla_target_id = t.id)`).
		Scan(&unevaluated); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unevaluated != 0 {
		t.Fatalf("%d burn targets were never reached after two passes", unevaluated)
	}
}

// FR-022 invariant 6: a burn breach opens NO incident, ever. A budget signal is not an outage —
// §16.4's own text calls the burn pair a reporting signal that trails the watermark — and an
// incident is a claim that something is happening NOW.
//
// The premise is asserted first: this burn really did fire. Without that line the test would pass
// just as happily against an evaluator that announced nothing at all.
func TestABurnBreachOpensNoIncidentEver(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)

	got := burnEvalOnce(t, st, ctx)
	if got.Onsets != 1 {
		t.Fatalf("the burn did not fire (%+v) — this test's premise is gone and it would pass for the "+
			"wrong reason", got)
	}
	var incidents, snapshots int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM incidents`).Scan(&incidents); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM incident_member_snapshots`).Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if incidents != 0 || snapshots != 0 {
		t.Fatalf("a burn breach opened %d incident(s) and %d snapshot(s): a budget signal trails the "+
			"watermark and says nothing about now (FR-022 invariant 6)", incidents, snapshots)
	}
}

// The BURN arm owes the same arming rule as the live one (§16.1, D-0176). Without it the swallow the
// live arm just lost survives in the second signal: emit to nobody, latch firing=true, and when the
// route returns there is no edge left to announce — while `activeBurnDelegationSQL` has begun
// suppressing the member's own burn alert. The member goes quiet for an alert the service already
// spent on an empty route.
func TestBurnWithholdsAnOnsetNobodyCanReceiveAndDoesNotLatch(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)

	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("disable channels: %v", err)
	}
	plantBurn(t, st, ctx, f, 5, minute/60) // the same breach the fires-once test uses

	got := burnEvalOnce(t, st, ctx)
	if got.Onsets != 0 {
		t.Fatalf("an unroutable burn FIRE announced %+v", got)
	}
	if got.Withheld[WithheldUnroutable] != 1 {
		t.Fatalf("the withheld FIRE was counted as %+v, want one under %q: silence that means "+
			"'nobody could be told' must not read as 'nothing is burning'", got.Withheld, WithheldUnroutable)
	}
	var firing bool
	var seq int64
	if err := st.pool.QueryRow(ctx,
		`SELECT firing, emitted_seq FROM service_burn_alert_state WHERE sla_target_id = $1`,
		f.targetID).Scan(&firing, &seq); err != nil {
		t.Fatalf("read latch: %v", err)
	}
	if firing || seq != 0 {
		t.Fatalf("the rule latched (firing=%v seq=%d) while announcing nothing: restoring the route "+
			"would leave no edge and the member would be silenced for an alert nobody got", firing, seq)
	}

	// Nothing was written, not merely nothing counted.
	var episodes, events int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_alert_episodes WHERE signal = 'burn'`).Scan(&episodes); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE topic = 'service_alert'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if episodes != 0 || events != 0 {
		t.Fatalf("a withheld FIRE left %d episode(s) and %d event(s)", episodes, events)
	}

	// Route restored — and BEFORE the next evaluation, coverage must still be dis-armed. This is the
	// half the live arm needed too: the verdict is still `fire` while nothing has been announced, so
	// arming here would silence the member's own burn alert for an onset that was never sent.
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = true WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if delegatedBurn(t, st, ctx, f) {
		t.Fatal("burn coverage armed on a restored route before the withheld onset was announced")
	}
	got = burnEvalOnce(t, st, ctx)
	if got.Onsets != 1 {
		t.Fatalf("after the route came back the burn arm reported %+v, want the one onset that was "+
			"waiting", got)
	}
	// ...and only NOW may it suppress the member's own burn alert.
	if !delegatedBurn(t, st, ctx, f) {
		t.Fatal("burn coverage did not arm after its onset was announced")
	}
}

// delegatedBurn asks the real delegation lookup whether this fixture's monitor is covered for burn.
func delegatedBurn(t *testing.T, st *Store, ctx context.Context, f burnFixture) bool {
	t.Helper()
	v, err := st.ActiveDelegation(ctx, f.monitorID, f.projectID, DelegationBurn)
	if err != nil {
		t.Fatalf("active delegation: %v", err)
	}
	return v.Suppress()
}
