package domain

import (
	"testing"
	"time"
)

// TestRecoverySentence pins the one rendering of a recovery snapshot (FR-043 §4.6): present exactly
// when N > 1, `N/M` in the run-then-threshold order, the first-OK instant as
// `firstOK.UTC().Format(time.RFC3339)` — whole seconds, `Z` — whatever zone and precision the snapshot
// was read in. The incident update and the notification both append this string (I14), so a format
// chosen by either surface on its own would let the two drift.
func TestRecoverySentence(t *testing.T) {
	// A non-UTC instant with a sub-second part: the rendering must convert and truncate, not echo.
	baku := time.FixedZone("UTC+4", 4*3600)
	firstOK := time.Date(2026, 11, 16, 18, 4, 5, 987654321, baku) // 14:04:05.987654321Z
	cases := []struct {
		name string
		snap *RecoverySnapshot
		want string
	}{
		{"nil snapshot (every event before the upgrade)", nil, ""},
		{"run of one is today's recovery", &RecoverySnapshot{Run: 1, Threshold: 1, FirstOK: firstOK}, ""},
		{"zero run", &RecoverySnapshot{Run: 0, Threshold: 3, FirstOK: firstOK}, ""},
		{"confirmed run", &RecoverySnapshot{Run: 3, Threshold: 3, FirstOK: firstOK},
			"Recovered: 3/3 consecutive checks passed; first OK at 2026-11-16T14:04:05Z"},
		// D10: three successes at threshold 4, lowered to 2, confirmed on the fourth.
		{"threshold lowered during the run", &RecoverySnapshot{Run: 4, Threshold: 2, FirstOK: firstOK},
			"Recovered: 4/2 consecutive checks passed; first OK at 2026-11-16T14:04:05Z"},
		{"run of two", &RecoverySnapshot{Run: 2, Threshold: 2, FirstOK: firstOK.UTC()},
			"Recovered: 2/2 consecutive checks passed; first OK at 2026-11-16T14:04:05Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.snap.Sentence(); got != c.want {
				t.Fatalf("Sentence() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestMonitorTransitionRecoveryRoundTrip: the snapshot rides the outbox payload and comes back as
// the same sentence; an event without the three fields (a legacy one) has no snapshot at all.
func TestMonitorTransitionRecoveryRoundTrip(t *testing.T) {
	firstOK := time.Date(2026, 11, 16, 14, 4, 5, 0, time.UTC)
	mt := MonitorTransition{MonitorID: "m1", Prev: StatusDown, Cur: StatusUp,
		RecoveryRun: 3, RecoveryThreshold: 3, RecoveryFirstOK: &firstOK}
	if got, want := mt.Recovery().Sentence(), "Recovered: 3/3 consecutive checks passed; first OK at 2026-11-16T14:04:05Z"; got != want {
		t.Fatalf("event sentence = %q, want %q", got, want)
	}
	legacy := MonitorTransition{MonitorID: "m1", Prev: StatusDown, Cur: StatusUp}
	if legacy.Recovery() != nil || legacy.Recovery().Sentence() != "" {
		t.Fatalf("a legacy event must carry no snapshot and render no sentence")
	}
}
