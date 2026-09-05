package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-032 §13a — the read API's contract, at the handler.
//
// The store's own SQL is tested against a real database; what is asserted here is what the HANDLER
// derives and refuses: the range, the limit, the cursor, the tenancy pair, and the two bounds that
// say what an answer means.

func expectedRunURL(projectID, monitorID, query string) string {
	return "/api/v1/projects/" + projectID + "/monitors/" + monitorID + "/expected-runs?" + query
}

// expectedRunRange is a valid half-open range for the fixtures below.
func expectedRunRange(now time.Time) string {
	return "from=" + now.Add(-time.Hour).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
}

// The pair is validated, not each half. A monitor the caller can see through ANOTHER project is
// still not this project's monitor, and answering for it would let a caller enumerate one project's
// monitors through another's route — a 404, hidden, matching the stated contract of every other
// monitor door.
func TestAProjectAndMonitorThatDoNotBelongTogetherAreNotFound(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	now := time.Now().UTC()

	// mon1 belongs to p1. Asking for it through p2 — which the caller CAN see — must 404.
	rec := do(h, o1Admin, http.MethodGet, expectedRunURL("p2", "mon1", expectedRunRange(now)), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a monitor from another project answered %d, want 404 hidden", rec.Code)
	}
	if len(fs.expectedRunQueries) != 0 {
		t.Errorf("the store was queried for a monitor outside the project: %+v", fs.expectedRunQueries)
	}

	// The converse: its own project answers.
	rec = do(h, o1Admin, http.MethodGet, expectedRunURL("p1", "mon1", expectedRunRange(now)), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the owning project answered %d: %s", rec.Code, rec.Body.String())
	}
	// And a project in ANOTHER ORG is 404 for this caller, hidden rather than forbidden.
	rec = do(h, o1Admin, http.MethodGet, expectedRunURL("p3", "mon3", expectedRunRange(now)), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another org's monitor answered %d, want 404 hidden", rec.Code)
	}
}

// The range is REQUIRED and bounded, and a cursor outside it is refused rather than ignored: it
// cannot belong to this traversal, and ignoring it would return a page from a different query than
// the caller asked for.
func TestTheRangeAndCursorAreRefusedByName(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	now := time.Now().UTC().Truncate(time.Second)
	ok := expectedRunRange(now)

	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"no range at all", "", "range_required"},
		{"only a start", "from=" + now.Add(-time.Hour).Format(time.RFC3339), "range_required"},
		{"a reversed range", "from=" + now.Format(time.RFC3339) + "&to=" + now.Add(-time.Hour).Format(time.RFC3339), "range_invalid"},
		{"an unparseable instant", "from=yesterday&to=" + now.Format(time.RFC3339), "range_invalid"},
		{"wider than retention", "from=" + now.AddDate(0, 0, -30).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339), "range_too_wide"},
		{"a zero limit", ok + "&limit=0", "limit_invalid"},
		{"a limit above the published maximum", ok + "&limit=201", "limit_invalid"},
		{"a non-integer limit", ok + "&limit=many", "limit_invalid"},
		{"a cursor that is not base64", ok + "&cursor=!!!", "cursor_invalid"},
		{"a cursor outside the range", ok + "&cursor=" + store.EncodeExpectedRunCursor(now.Add(-48*time.Hour)), "cursor_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, o1Admin, http.MethodGet, expectedRunURL("p1", "mon1", tc.query), "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.Error != tc.want {
				t.Fatalf("error is %q, want %q — a caller must be able to tell which of its "+
					"parameters was wrong", body.Error, tc.want)
			}
		})
	}
}

// The range bound is the CONFIGURED retention, not the built-in default.
//
// `ledger.expected_run_retention_days` is bounded 2..90 and reaches the store; the handler read
// `domain.DefaultExpectedRunRetentionDays` instead, so the bound was a constant fourteen days. An
// instance keeping ninety days of windows refused a thirty-day range for rows it still held, and
// one configured to keep two days accepted a fourteen-day range it could not answer. Both
// directions are asserted, because a fix that only widened the bound would pass a test that only
// checked the wide case.
func TestTheRangeBoundFollowsTheConfiguredRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	thirtyDays := "from=" + now.AddDate(0, 0, -30).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	fiveDays := "from=" + now.AddDate(0, 0, -5).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)

	for _, tc := range []struct {
		name      string
		retention int
		query     string
		want      int
	}{
		{"ninety days answers a thirty-day range", 90, thirtyDays, http.StatusOK},
		{"the default still refuses thirty days", 0, thirtyDays, http.StatusBadRequest},
		{"two days refuses a five-day range", 2, fiveDays, http.StatusBadRequest},
		{"the default answers five days", 0, fiveDays, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := seededStore()
			fs.expectedRunRetentionDays = tc.retention
			rec := do(newHandler(fs), o1Admin, http.MethodGet, expectedRunURL("p1", "mon1", tc.query), "")
			if rec.Code != tc.want {
				t.Fatalf("retention %d answered %d, want %d: %s",
					tc.retention, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The response carries computed verdicts and the two bounds, and `interval_assumed` reaches the
// caller (invariants 20f, 26b).
//
// `interval_assumed` existed in the table and appeared in NO response until revision 20 — a fact
// stored and never read, which is the mirror image of the defects §5.2 records. It is here so a
// caller can see that a `covered_late` may be an artefact of §14.2's conservative threshold, and
// never so it can promote one.
func TestTheResponseCarriesComputedVerdictsAndItsBounds(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-10 * time.Minute)
	issued := due.Add(5 * time.Minute) // later than the interval that spaced it
	terminal := issued.Add(time.Second)
	fence := now.Add(-time.Hour)
	fs.expectedRunPage = store.ExpectedRunPage{
		Windows: []domain.ExpectedRun{{
			MonitorID: "mon1", DueAt: due, JobID: "11111111-1111-4111-8111-111111111111",
			CarrierGeneration: domain.LedgerMinCarrier, ExecutionRevision: 4, Region: "core",
			IntervalSeconds: 60, IntervalAssumed: true,
			IssuedAt: &issued, TerminalAt: &terminal, Outcome: "result",
		}},
		LedgerFrom: fence, HasLedgerFrom: true, GapTruncatedBefore: &fence,
		NextCursor: "",
	}

	rec := do(h, o1Admin, http.MethodGet, expectedRunURL("p1", "mon1", expectedRunRange(now)), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Windows []struct {
			Verdict         string `json:"verdict"`
			IntervalAssumed bool   `json:"interval_assumed"`
			IntervalSeconds int    `json:"interval_seconds"`
		} `json:"windows"`
		LedgerFrom         *time.Time `json:"ledger_from"`
		GapTruncatedBefore *time.Time `json:"gap_truncated_before"`
		NextCursor         *string    `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(body.Windows) != 1 {
		t.Fatalf("%d windows, want 1", len(body.Windows))
	}
	// COMPUTED, not stored: the store handed rows with timestamps and no verdict.
	if body.Windows[0].Verdict != string(domain.VerdictCoveredLate) {
		t.Fatalf("verdict is %q, want %q — a run ISSUED five minutes after a window spaced at 60 "+
			"seconds is too far from it to prove it", body.Windows[0].Verdict, domain.VerdictCoveredLate)
	}
	if !body.Windows[0].IntervalAssumed {
		t.Error("interval_assumed did not reach the response: a caller cannot then tell a measured " +
			"covered_late from one produced by the conservative threshold")
	}
	if body.LedgerFrom == nil || !body.LedgerFrom.Equal(fence) {
		t.Errorf("ledger_from is %v, want %s", body.LedgerFrom, fence)
	}
	if body.GapTruncatedBefore == nil {
		t.Error("gap_truncated_before did not reach the response")
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor is %v on the last page, want null", *body.NextCursor)
	}
	// And the handler passed the range and limit it derived, rather than its own defaults.
	if len(fs.expectedRunQueries) != 1 {
		t.Fatalf("%d store queries, want 1", len(fs.expectedRunQueries))
	}
	if got := fs.expectedRunQueries[0].Limit; got != 50 {
		t.Errorf("the default limit is %d, want the published 50", got)
	}
	if fs.expectedRunQueries[0].CursorDueAt != nil {
		t.Error("a request with no cursor passed one to the store")
	}
}

// A monitor with no expectation answers with a NULL ledger_from, and the caller must render the
// whole range as not stored rather than as time nothing was due in. That is the same over-claim
// from the other end, and it is why the field is nullable rather than zero-valued.
func TestAMonitorWithNoExpectationAnswersWithANullBound(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	fs.expectedRunPage = store.ExpectedRunPage{HasLedgerFrom: false}

	now := time.Now().UTC()
	rec := do(h, o1Admin, http.MethodGet, expectedRunURL("p1", "mon1", expectedRunRange(now)), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ledger_from"] != nil {
		t.Fatalf("ledger_from is %v for a monitor with no expectation, want null", body["ledger_from"])
	}
	if _, ok := body["ledger_from"]; !ok {
		t.Error("ledger_from is ABSENT rather than null: a caller checking for the key cannot " +
			"distinguish an old server from a monitor that answers for nothing")
	}
}
