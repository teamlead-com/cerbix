package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

// iter-0203 — a bounded status page and a paginated incident history
// (func-status-pages-incidents.md §13, FR-039 / NFR-033, D-0270).

const (
	// pastIncidentsOnPage is how many past incidents the page itself carries (§13.3).
	pastIncidentsOnPage = 10
	// historyPageSize is the most incidents one history response carries (§13.4 rule 1).
	historyPageSize = 50
	// historyMonthLayout is the `month` parameter and the response's month keys: a UTC month.
	historyMonthLayout = "2006-01"
	// historyRenderSlots is how many uncached public history renders one process runs at once;
	// beyond it a distinct request is refused with 429 rather than queued (review R1-6).
	historyRenderSlots = 8
)

// errHistoryBusy is returned by a public history render that found no free slot. It is an error,
// so the render cache neither stores it nor serves it as bytes; the handler answers 429.
var errHistoryBusy = errors.New("history_busy")

// canonicalUUID matches the lower-case canonical form the store returns ids in. A cursor is only
// ever minted by this server, so anything else in its id half is not one of ours.
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// historyRequest is a validated history query: the window, the month inside it, and where to
// resume. Every field is decided before the store or the cache is touched (AC-0203-4).
type historyRequest struct {
	now        time.Time
	since      time.Time // the window's OPEN lower bound: now − 90 days
	month      string    // "2026-09"
	monthStart time.Time
	monthEnd   time.Time
	offered    []string // the months that intersect the window, newest first
	rawCursor  string
	after      *store.HistoryCursor
}

// historyMonthsOffered lists the UTC months that intersect (since, now], newest first. A 90-day
// window touches up to five: on 1 May it starts on 31 January.
func historyMonthsOffered(since, now time.Time) []string {
	var out []string
	for m := utcMonthStart(now); m.AddDate(0, 1, 0).After(since); m = m.AddDate(0, -1, 0) {
		out = append(out, m.Format(historyMonthLayout))
	}
	return out
}

func utcMonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// parseHistoryRequest validates `month` and `cursor`. On a refusal it has written the 400.
func parseHistoryRequest(w http.ResponseWriter, r *http.Request, now time.Time) (historyRequest, bool) {
	win90, _ := sla.WindowByName("90d")
	req := historyRequest{now: now.UTC(), since: now.UTC().Add(-win90.Duration)}
	req.offered = historyMonthsOffered(req.since, req.now)

	q := r.URL.Query()
	req.month = q.Get("month")
	if req.month == "" {
		req.month = req.offered[0]
	}
	start, err := time.Parse(historyMonthLayout, req.month)
	if err != nil || start.Format(historyMonthLayout) != req.month {
		writeError(w, http.StatusBadRequest, "invalid_month: month must be YYYY-MM")
		return req, false
	}
	offered := false
	for _, m := range req.offered {
		offered = offered || m == req.month
	}
	if !offered {
		writeError(w, http.StatusBadRequest, "month_outside_history: history covers the last 90 days, months "+
			strings.Join(req.offered, ", "))
		return req, false
	}
	req.monthStart, req.monthEnd = start.UTC(), start.UTC().AddDate(0, 1, 0)

	if raw := q.Get("cursor"); raw != "" {
		after, ok := decodeHistoryCursor(raw)
		// A cursor names the last incident a page of THIS month returned, so its instant lies in the
		// month and inside the window (since, now]; anything else is not a position in this
		// traversal. It also has exactly ONE spelling — the one this server mints — so the same
		// position cannot be re-encoded into new cache keys (review R1-4, R1-6).
		if !ok || raw != encodeHistoryPosition(after) || !after.ResolvedAt.After(req.since) ||
			after.ResolvedAt.After(req.now) || after.ResolvedAt.Before(req.monthStart) ||
			!after.ResolvedAt.Before(req.monthEnd) {
			writeError(w, http.StatusBadRequest, "invalid_cursor: use next_cursor from the previous response")
			return req, false
		}
		req.rawCursor, req.after = raw, &after
	}
	return req, true
}

// encodeHistoryCursor mints the opaque cursor: base64url of "<resolved_at RFC3339Nano>|<id>".
func encodeHistoryCursor(in domain.Incident) string {
	return encodeHistoryPosition(store.HistoryCursor{ResolvedAt: *in.ResolvedAt, ID: in.ID})
}

// encodeHistoryPosition is the one spelling of a position; decodeHistoryCursor's result must
// re-encode to exactly the cursor it came from.
func encodeHistoryPosition(c store.HistoryCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.ResolvedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

func decodeHistoryCursor(raw string) (store.HistoryCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return store.HistoryCursor{}, false
	}
	at, id, found := strings.Cut(string(b), "|")
	if !found || !canonicalUUID.MatchString(id) {
		return store.HistoryCursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return store.HistoryCursor{}, false
	}
	return store.HistoryCursor{ResolvedAt: t.UTC(), ID: id}, true
}

type historyMonthView struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

type incidentHistoryView struct {
	Title       string               `json:"title"`
	Month       string               `json:"month"`
	From        time.Time            `json:"from"`
	To          time.Time            `json:"to"`
	HistoryFrom time.Time            `json:"history_from"`
	Months      []historyMonthView   `json:"months"`
	Incidents   []incidentDetailView `json:"incidents"`
	NextCursor  *string              `json:"next_cursor"`
}

// incidentHistoryAuthed serves the history to an authenticated member, at any visibility — the
// access rule of the authenticated render.
func (h *Handler) incidentHistoryAuthed(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, false)
	if !ok {
		return
	}
	req, ok := parseHistoryRequest(w, r, time.Now())
	if !ok {
		return
	}
	h.writeIncidentHistory(w, r, sp, req, false)
}

// incidentHistoryPublic serves the history without a session, behind the public render's
// visibility gate and through the same bounded, coalescing render cache, keyed additionally by
// month and cursor. Validation runs first, so a refused query never makes a cache entry.
func (h *Handler) incidentHistoryPublic(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.publicStatusPage(w, r)
	if !ok {
		return
	}
	req, ok := parseHistoryRequest(w, r, time.Now())
	if !ok {
		return
	}
	key := statusPageCacheKey(cacheHistory, sp, true, r.URL.Query().Get("token"), req.month, req.rawCursor)
	body, hit, err := h.renderCache.do(key, func() ([]byte, bool, error) {
		select {
		case h.historySlots <- struct{}{}:
			defer func() { <-h.historySlots }()
		default:
			return nil, false, errHistoryBusy
		}
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		h.writeIncidentHistory(rec, r, sp, req, true)
		return rec.bytesWithStatus(), rec.status == http.StatusOK, nil
	})
	if errors.Is(err, errHistoryBusy) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "history_busy: too many incident history requests; retry shortly")
		return
	}
	if err != nil {
		h.serverError(w, "render_incident_history", err)
		return
	}
	writeBufferedResponse(w, body, hit)
}

// writeIncidentHistory reads one page of a month and projects its incidents exactly as the page
// projects its past incidents: the same enrichIncidents call, the same page-local
// affected_component_ids, the same public redaction (AC-0203-7).
func (h *Handler) writeIncidentHistory(w http.ResponseWriter, r *http.Request, sp domain.StatusPage, req historyRequest, public bool) {
	ctx := r.Context()
	comps, ok := h.pageComponents(w, r, sp, public)
	if !ok {
		return
	}
	projects, err := h.store.StatusPageProjectIDs(ctx, sp.ID)
	if err != nil {
		h.serverError(w, "status_page_projects", err)
		return
	}
	sort.Strings(projects)
	page, err := h.store.IncidentHistory(ctx, projects, req.since, req.now, req.monthStart, req.monthEnd, req.after, historyPageSize)
	if err != nil {
		h.serverError(w, "incident_history", err)
		return
	}
	views, ok := h.enrichIncidents(w, r, page.Incidents, newAffectedComponentIndex(comps), true, public)
	if !ok {
		return
	}
	months := make([]historyMonthView, 0, len(req.offered))
	for _, m := range req.offered {
		months = append(months, historyMonthView{Month: m, Count: page.Counts[m]})
	}
	out := incidentHistoryView{
		Title:       sp.Title,
		Month:       req.month,
		From:        req.monthStart,
		To:          req.monthEnd,
		HistoryFrom: req.since,
		Months:      months,
		Incidents:   views,
	}
	if out.From.Before(req.since) {
		out.From = req.since
	}
	if out.To.After(req.now) {
		out.To = req.now
	}
	if page.More && len(page.Incidents) > 0 {
		c := encodeHistoryCursor(page.Incidents[len(page.Incidents)-1])
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
}
