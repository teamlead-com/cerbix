package domain

// FR-021 §16.4 — the burn signal's HOLD matrix, as a pure decision.
//
// The two windows of a rule are computed by the one burn math owner over SEALED facts; what those
// two windows mean for the rule's latch is decided here, without a clock, a database or a leader.
// That separation is what makes "why is this rule still firing" answerable after the fact: the
// answer is a function of the two window verdicts and the latch, and nothing else.

// BurnVerdict is what one evaluation concluded about one rule. The values are exactly the strings
// `service_burn_alert_state.last_verdict` accepts (migration 00082's CHECK), so the row the
// delegation query reads back means what the evaluator wrote — two vocabularies for one column is
// how a badge and a delivery gate end up disagreeing.
type BurnVerdict string

const (
	// BurnFire: both windows are quotable and both are at or over the threshold.
	BurnFire BurnVerdict = "fire"
	// BurnClear: both windows are quotable and at least one is under the threshold.
	BurnClear BurnVerdict = "clear"
	// BurnHold is a SUCCESSFUL evaluation that cannot speak: at least one window may not be quoted
	// at all. It keeps the previous level and records WHY, which is also what dis-arms burn
	// coverage — a rule that cannot fire is not a replacement for its members' own alerts.
	BurnHold BurnVerdict = "hold"
)

// BurnRuleDecision is what one rule evaluation concluded.
type BurnRuleDecision struct {
	Verdict BurnVerdict
	// Firing is the level AFTER this evaluation. Under a HOLD it is the level BEFORE it, unchanged.
	Firing bool
	// HoldReason is the §11.2/§11.3 reason of the window that could not be quoted, stored as
	// `last_reason`. Empty unless Verdict is BurnHold.
	HoldReason string
	// Edge reports that the LEVEL changed, and it is the only thing that enqueues a notification: a
	// rule that is still firing has nothing new to announce, and re-stating it every cadence is how
	// a page becomes something people mute.
	Edge bool
}

// DecideBurnRule decides ONE rule from its two computed windows and the latch it currently holds.
//
//	long, short — the rule's two windows, already judged by the burn math owner
//	threshold   — the rule's burn-rate threshold
//	firing      — the rule's CURRENT latch (`service_burn_alert_state.firing`)
//
// Both windows must breach for a rule to fire (SRE canon: the long window filters noise, the short
// one confirms the burn is still happening), so the multi-window rule is a conjunction on the way
// up and a disjunction on the way down.
//
// Quotability is checked BEFORE the arithmetic, and a HOLD keeps the previous level. This is the
// most dangerous line in the file to get wrong: treating an unquotable window as "burn = 0" would
// silently resolve a live alert, telling an operator the budget stopped burning when the truth is
// that we stopped being able to see it. §11.2 already refuses to quote such a window in a report a
// human is reading; paging — or un-paging — on it would be the strongest action taken on the
// weakest evidence.
func DecideBurnRule(long, short ServiceBurnWindow, threshold float64, firing bool) BurnRuleDecision {
	// The long window is tested first so that when NEITHER window is quotable, the reason recorded
	// is the long one's: it is the wider evidence and the window that decides whether the burn is
	// real, so its complaint is the one that explains the hold — a short window inside a gapped or
	// revision-spanning long window is usually reporting the same defect one level down.
	if !burnQuotable(long) {
		return burnHold(long, firing)
	}
	if !burnQuotable(short) {
		return burnHold(short, firing)
	}
	level := *long.Rate >= threshold && *short.Rate >= threshold
	verdict := BurnClear
	if level {
		verdict = BurnFire
	}
	return BurnRuleDecision{Verdict: verdict, Firing: level, Edge: level != firing}
}

// burnQuotable is §16.4's "quotable": the window passed BOTH honesty axes and carries a rate.
// `partial` is deliberately not quotable here even though it carries its number — §11.2 lets a
// partial window keep its rate WITH its fraction for a human reading a report, and that is a
// different act from waking one up.
func burnQuotable(w ServiceBurnWindow) bool {
	return w.Status == ServiceReportOK && w.Rate != nil
}

// burnHold keeps the level and names the window's reason. An `ok` status with no rate is a
// contradiction the math owner cannot produce; if one ever reaches here it holds as
// `nothing_measured` rather than resolving the contradiction by inventing a zero.
func burnHold(w ServiceBurnWindow, firing bool) BurnRuleDecision {
	reason := w.Reason
	if reason == "" {
		reason = ServiceReportReasonNothingMeasured
	}
	return BurnRuleDecision{Verdict: BurnHold, Firing: firing, HoldReason: reason}
}
