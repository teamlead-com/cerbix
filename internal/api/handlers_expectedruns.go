package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-032 §13a — the expected-run read API.
//
// Authorization is not the foreign key's job: the composite `(monitor_id, project_id)` key protects
// STORAGE integrity — it stops a row outliving its tenant — and says nothing about who may READ a
// row. The two are separate and both are needed, which is what party [231] was right to insist on.

const (
	// expectedRunListDefaultLimit and Max are the bounds every other paged endpoint here
	// publishes. §13a's first draft said 1..1000 and contradicted the established convention,
	// which the reviewer caught at [244]; this is the convention.
	expectedRunListDefaultLimit = 50
	expectedRunListLimitMax     = 200
)

// expectedRunWindow is one window as the API renders it.
//
// Verdicts are COMPUTED here from the row's timestamps and never stored (invariant 19), which is
// what lets a late terminal change a window's reading with nothing to keep consistent.
type expectedRunWindow struct {
	DueAt             time.Time `json:"due_at"`
	Verdict           string    `json:"verdict"`
	JobID             string    `json:"job_id,omitempty"`
	CarrierGeneration int       `json:"carrier_generation,omitempty"`
	ExecutionRevision int64     `json:"execution_revision"`
	Region            string    `json:"region"`
	IntervalSeconds   int       `json:"interval_seconds"`
	// IntervalAssumed says the lateness threshold was ASSUMED rather than observed — §14.2's
	// conservative minimum, used only on a window the core did not record at issue.
	//
	// It is EXPLANATORY ONLY (invariant 20g). It exists so a caller can see that a `covered_late`
	// may be an artefact of the assumption, and it may never be read as licence to treat such a
	// window as `covered`: not by the stroke gate, not by a coverage numerator, not by a future
	// surface arguing the lateness is "probably not real". Handing an implementer a flag that
	// softens a truthfulness verdict is how the gate gets widened without anyone deciding to widen
	// it, which is why the prohibition is a named invariant with its own tests rather than a
	// convention.
	IntervalAssumed bool       `json:"interval_assumed"`
	IssuedAt        *time.Time `json:"issued_at,omitempty"`
	ClaimedAt       *time.Time `json:"claimed_at,omitempty"`
	TerminalAt      *time.Time `json:"terminal_at,omitempty"`
	Outcome         string     `json:"outcome,omitempty"`
	RefusedAt       *time.Time `json:"refused_at,omitempty"`
	RefusedReason   string     `json:"refused_reason,omitempty"`
	SkipReason      string     `json:"skip_reason,omitempty"`
}

// listExpectedRuns answers GET /api/v1/projects/{projectID}/monitors/{monitorID}/expected-runs.
//
// Mounted behind the session-auth middleware like every project route — never on `PublicRouter`,
// never on `AgentRouter`. A monitor outside the caller's tenancy is 404 HIDDEN rather than 403,
// matching `handlers_monitors.go`'s stated contract, and a `projectID`/`monitorID` pair that does
// not belong together is 404 too: the PAIR is validated, not each half.
func (h *Handler) listExpectedRuns(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	mon, err := h.store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_monitor", err)
		return
	}
	// The pair, not each half. A monitor the caller CAN see through another project is still not
	// this project's monitor, and answering for it would let a caller enumerate one project's
	// monitors through another's route.
	if mon.ProjectID != proj.ID {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	q := r.URL.Query()
	fromS, toS := q.Get("from"), q.Get("to")
	if fromS == "" || toS == "" {
		writeError(w, http.StatusBadRequest, "range_required")
		return
	}
	from, errFrom := time.Parse(time.RFC3339Nano, fromS)
	to, errTo := time.Parse(time.RFC3339Nano, toS)
	if errFrom != nil || errTo != nil || !to.After(from) {
		writeError(w, http.StatusBadRequest, "range_invalid")
		return
	}
	// At most the retention window. A wider range cannot be answered anyway — the windows are
	// gone — and refusing it is better than returning a page that silently begins later.
	if to.Sub(from) > time.Duration(domain.DefaultExpectedRunRetentionDays)*24*time.Hour {
		writeError(w, http.StatusBadRequest, "range_too_wide")
		return
	}
	limit := expectedRunListDefaultLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > expectedRunListLimitMax {
			writeError(w, http.StatusBadRequest, "limit_invalid")
			return
		}
		limit = n
	}
	var cursor *time.Time
	if raw := q.Get("cursor"); raw != "" {
		at, err := store.DecodeExpectedRunCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor_invalid")
			return
		}
		// A cursor outside the requested range cannot belong to this traversal, and ignoring it
		// would return a page from a DIFFERENT query than the caller asked for.
		if at.Before(from) || !at.Before(to) {
			writeError(w, http.StatusBadRequest, "cursor_invalid")
			return
		}
		cursor = &at
	}

	page, err := h.store.ListExpectedRuns(r.Context(), store.ExpectedRunQuery{
		ProjectID: proj.ID, MonitorID: mon.ID, From: from, To: to, Limit: limit, CursorDueAt: cursor,
	})
	if err != nil {
		h.serverError(w, "list_expected_runs", err)
		return
	}

	windows := make([]expectedRunWindow, 0, len(page.Windows))
	for _, w := range page.Windows {
		windows = append(windows, expectedRunWindow{
			DueAt: w.DueAt, Verdict: string(w.Verdict()), JobID: w.JobID,
			CarrierGeneration: w.CarrierGeneration, ExecutionRevision: w.ExecutionRevision,
			Region: w.Region, IntervalSeconds: w.IntervalSeconds, IntervalAssumed: w.IntervalAssumed,
			IssuedAt: w.IssuedAt, ClaimedAt: w.ClaimedAt, TerminalAt: w.TerminalAt,
			Outcome: w.Outcome, RefusedAt: w.RefusedAt, RefusedReason: w.RefusedReason,
			SkipReason: w.SkipReason,
		})
	}
	body := map[string]any{
		"windows": windows,
		// The two facts that BOUND what the answer means (invariant 26b). A caller holding the
		// rows and not the bounds reads an empty range as "nothing was due" when the truth is
		// "the ledger cannot say", which is the over-claim this whole requirement exists to
		// prevent.
		"ledger_from":          nil,
		"gap_truncated_before": page.GapTruncatedBefore,
		"next_cursor":          nil,
	}
	if page.HasLedgerFrom {
		body["ledger_from"] = page.LedgerFrom
	}
	if page.NextCursor != "" {
		body["next_cursor"] = page.NextCursor
	}
	// A range reaching BEFORE ledger_from is answered with what the ledger has plus the bound that
	// says the rest is unanswerable — never silently clipped to a later start, which a caller
	// could not distinguish from a covered span.
	writeJSON(w, http.StatusOK, body)
}
