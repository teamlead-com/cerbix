package domain

import "testing"

// FR-021 §16.4 — the hold matrix. Every row of it is a way to page (or un-page) wrongly, and none
// of them needs a database to be wrong, so all of them are decided here.

// quotable is a window that passed both honesty axes and carries a rate.
func quotable(rate float64) ServiceBurnWindow {
	return ServiceBurnWindow{Window: "long", Status: ServiceReportOK, Rate: &rate,
		StorageContinuity: true, Coverage: 1}
}

// unquotable is a window the math owner refused to let anyone quote, with the reason it gave.
func unquotable(status ServiceReportStatus, reason string) ServiceBurnWindow {
	return ServiceBurnWindow{Window: "long", Status: status, Reason: reason}
}

// partialWithRate is §11.2's awkward one: the number EXISTS and is reported with its fraction, and
// it still must not page. A test that only ever built rate-less holds would miss it.
func partialWithRate(reason string, rate float64) ServiceBurnWindow {
	return ServiceBurnWindow{Window: "long", Status: ServiceReportPartial, Reason: reason,
		Rate: &rate, Coverage: 0.4}
}

func TestDecideBurnRule(t *testing.T) {
	const threshold = 14.4

	cases := []struct {
		name        string
		long, short ServiceBurnWindow
		firing      bool
		wantVerdict BurnVerdict
		wantFiring  bool
		wantReason  string
		wantEdge    bool
	}{
		// ── Both windows quotable: the rule actually decides ─────────────────────────────
		{name: "both windows over the threshold fire",
			long: quotable(20), short: quotable(30),
			wantVerdict: BurnFire, wantFiring: true, wantEdge: true},
		{name: "exactly at the threshold counts as breaching",
			long: quotable(threshold), short: quotable(threshold),
			wantVerdict: BurnFire, wantFiring: true, wantEdge: true},
		{name: "the short window under the threshold clears",
			long: quotable(20), short: quotable(3), firing: true,
			wantVerdict: BurnClear, wantEdge: true},
		{name: "the long window under the threshold clears",
			long: quotable(3), short: quotable(40), firing: true,
			wantVerdict: BurnClear, wantEdge: true},
		{name: "neither over: nothing to say and nothing was open",
			long: quotable(1), short: quotable(1),
			wantVerdict: BurnClear},
		// The two no-edge rows: the level did not change, so nothing is enqueued. A rule that
		// re-announces itself every cadence is a rule people mute.
		{name: "still firing is not a new onset",
			long: quotable(20), short: quotable(30), firing: true,
			wantVerdict: BurnFire, wantFiring: true},
		{name: "still clear is not a recovery",
			long: quotable(1), short: quotable(1), firing: false,
			wantVerdict: BurnClear},

		// ── Every HOLD reason, on the LONG window ────────────────────────────────────────
		{name: "hold: nothing sealed at all",
			long:        unquotable(ServiceReportInsufficientSealed, ServiceReportReasonNothingSealed),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonNothingSealed},
		{name: "hold: the watermark is behind the window",
			long:        unquotable(ServiceReportInsufficientSealed, ServiceReportReasonStaleWatermark),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonStaleWatermark},
		{name: "hold: the window precedes the materialization era",
			long:        unquotable(ServiceReportInsufficientHistory, ServiceReportReasonEraShort),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonEraShort},
		{name: "hold: a storage gap",
			long:        unquotable(ServiceReportPartial, ServiceReportReasonStorageGap),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonStorageGap},
		{name: "hold: the window spans definition revisions",
			long:        unquotable(ServiceReportUnavailable, ServiceReportReasonSpansRevisions),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonSpansRevisions},
		{name: "hold: no decidable time in the window",
			long:        unquotable(ServiceReportUnavailable, ServiceReportReasonZeroDecidable),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonZeroDecidable},
		// The row §16.4 argues for at length: the number exists, the report prints it with its
		// fraction, and it still must not page. Paging on evidence §11.2 calls partial would be the
		// strongest action taken on the weakest data.
		{name: "hold: decidable coverage below min, even though the rate exists",
			long:        partialWithRate(ServiceReportReasonLowCoverage, 99),
			short:       quotable(20),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonLowCoverage},

		// ── Every HOLD reason, on the SHORT window: a quotable long window does not rescue it ──
		{name: "hold: short window has nothing sealed",
			long:        quotable(20),
			short:       unquotable(ServiceReportInsufficientSealed, ServiceReportReasonNothingSealed),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonNothingSealed},
		{name: "hold: short window behind the watermark",
			long:        quotable(20),
			short:       unquotable(ServiceReportInsufficientSealed, ServiceReportReasonStaleWatermark),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonStaleWatermark},
		{name: "hold: short window precedes the era",
			long:        quotable(20),
			short:       unquotable(ServiceReportInsufficientHistory, ServiceReportReasonEraShort),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonEraShort},
		{name: "hold: short window has a storage gap",
			long:        quotable(20),
			short:       unquotable(ServiceReportPartial, ServiceReportReasonStorageGap),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonStorageGap},
		{name: "hold: short window spans revisions",
			long:        quotable(20),
			short:       unquotable(ServiceReportUnavailable, ServiceReportReasonSpansRevisions),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonSpansRevisions},
		{name: "hold: short window has no decidable time",
			long:        quotable(20),
			short:       unquotable(ServiceReportUnavailable, ServiceReportReasonZeroDecidable),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonZeroDecidable},
		{name: "hold: short window coverage below min",
			long:        quotable(20),
			short:       partialWithRate(ServiceReportReasonLowCoverage, 99),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonLowCoverage},

		// ── Both windows unquotable: the LONG one's reason is the one recorded ───────────
		{name: "both unquotable reports the long window's reason",
			long:        unquotable(ServiceReportUnavailable, ServiceReportReasonSpansRevisions),
			short:       unquotable(ServiceReportPartial, ServiceReportReasonStorageGap),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonSpansRevisions},

		// ── The dangerous one: a HOLD never resolves a live alert ────────────────────────
		{name: "a firing rule stays firing through a storage gap",
			long:        unquotable(ServiceReportPartial, ServiceReportReasonStorageGap),
			short:       quotable(20),
			firing:      true,
			wantVerdict: BurnHold, wantFiring: true, wantReason: ServiceReportReasonStorageGap},
		{name: "a firing rule stays firing when the watermark falls behind",
			long:        quotable(20),
			short:       unquotable(ServiceReportInsufficientSealed, ServiceReportReasonStaleWatermark),
			firing:      true,
			wantVerdict: BurnHold, wantFiring: true, wantReason: ServiceReportReasonStaleWatermark},
		{name: "a firing rule stays firing when coverage drops below min",
			long:        partialWithRate(ServiceReportReasonLowCoverage, 0.1),
			short:       quotable(0.1),
			firing:      true,
			wantVerdict: BurnHold, wantFiring: true, wantReason: ServiceReportReasonLowCoverage},
		{name: "a clear rule stays clear under a hold",
			long:        unquotable(ServiceReportUnavailable, ServiceReportReasonZeroDecidable),
			short:       quotable(99),
			wantVerdict: BurnHold, wantReason: ServiceReportReasonZeroDecidable},

		// An `ok` status without a rate is a contradiction the math owner cannot produce; if it ever
		// arrives, it holds rather than being read as "burn = 0".
		{name: "ok without a rate holds instead of reading as zero",
			long:        ServiceBurnWindow{Status: ServiceReportOK},
			short:       quotable(20),
			firing:      true,
			wantVerdict: BurnHold, wantFiring: true, wantReason: ServiceReportReasonNothingMeasured},
	}

	for _, c := range cases {
		got := DecideBurnRule(c.long, c.short, threshold, c.firing)
		if got.Verdict != c.wantVerdict {
			t.Errorf("%s: verdict = %q, want %q", c.name, got.Verdict, c.wantVerdict)
		}
		if got.Firing != c.wantFiring {
			t.Errorf("%s: firing = %v, want %v", c.name, got.Firing, c.wantFiring)
		}
		if got.HoldReason != c.wantReason {
			t.Errorf("%s: hold reason = %q, want %q", c.name, got.HoldReason, c.wantReason)
		}
		if got.Edge != c.wantEdge {
			t.Errorf("%s: edge = %v, want %v", c.name, got.Edge, c.wantEdge)
		}
		// A HOLD can never be an edge: it keeps the level by definition, and an edge is what
		// enqueues. This is asserted separately from the table so no row can opt out of it.
		if got.Verdict == BurnHold && got.Edge {
			t.Errorf("%s: a hold produced an edge, which would enqueue on absent data", c.name)
		}
		if got.Verdict != BurnHold && got.HoldReason != "" {
			t.Errorf("%s: verdict %q carried hold reason %q", c.name, got.Verdict, got.HoldReason)
		}
	}
}

// The stored vocabulary is one vocabulary: these three strings are what migration 00082's CHECK
// accepts and what the delegation query matches on ('fire', 'clear'), so a rename here that looks
// harmless would silently dis-arm every armed rule.
func TestBurnVerdictValues(t *testing.T) {
	for _, c := range []struct {
		got  BurnVerdict
		want string
	}{{BurnFire, "fire"}, {BurnClear, "clear"}, {BurnHold, "hold"}} {
		if string(c.got) != c.want {
			t.Errorf("verdict %v = %q, want %q", c.got, string(c.got), c.want)
		}
	}
}
