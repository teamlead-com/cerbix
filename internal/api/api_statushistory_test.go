package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0203 (func-status-pages-incidents.md §13, FR-039 / NFR-033): the page carries at most ten
// past incidents and a flag; the rest are served by two history routes, paged by UTC month.
//
// These tests prove the HANDLER: routing, access, validation, the projection, the cache and the
// ceiling. The fake store's month query is deliberately simple; the real SQL (month bounds, keyset,
// one snapshot, the index) is proved against PostgreSQL in internal/store's
// incidenthistory_internal_test.go, so a green fake cannot stand in for it.

func pastIncident(fs *fakeStore, id string, at time.Time) {
	t := at
	fs.incidents[id] = domain.Incident{ID: id, ProjectID: "p1", Title: "past " + id,
		Status: domain.IncidentResolved, Impact: domain.ImpactMinor, Source: domain.SourceManual,
		StartedAt: at.Add(-10 * time.Minute), ResolvedAt: &t}
}

// encodeTestCursor builds a cursor the way the handler encodes one (base64url of
// "<RFC3339Nano>|<id>"), so the validation cases can be exact about what is wrong with each.
func encodeTestCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func monthStartUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

type historyBody struct {
	Title       string `json:"title"`
	Month       string `json:"month"`
	From        string `json:"from"`
	To          string `json:"to"`
	HistoryFrom string `json:"history_from"`
	Months      []struct {
		Month string `json:"month"`
		Count int    `json:"count"`
	} `json:"months"`
	Incidents []struct {
		ID                   string   `json:"id"`
		ProjectID            string   `json:"project_id"`
		MonitorID            string   `json:"monitor_id"`
		AffectedComponentIDs []string `json:"affected_component_ids"`
		Updates              []struct {
			ID     string `json:"id"`
			Author string `json:"author"`
			Body   string `json:"body"`
		} `json:"updates"`
		Postmortem *struct {
			ID     string `json:"id"`
			Author string `json:"author"`
			Body   string `json:"body"`
		} `json:"postmortem"`
	} `json:"incidents"`
	NextCursor *string `json:"next_cursor"`
}

func decodeHistory(t *testing.T, body []byte) historyBody {
	t.Helper()
	var out historyBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode history: %v\n%s", err, body)
	}
	return out
}

// AC-0203-1 (handler half): the render carries the store's bounded list and its flag, always.
func TestRenderCarriesTenPastIncidentsAndTheMoreFlag(t *testing.T) {
	fs := seededStore()
	now := time.Now()
	for i := 0; i < 12; i++ {
		pastIncident(fs, fmt.Sprintf("r%02d", i), now.Add(-time.Duration(i+1)*time.Hour))
	}
	rec := do(newPublicHandler(fs), outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("render = %d", rec.Code)
	}
	var render struct {
		Recent []struct {
			ID string `json:"id"`
		} `json:"recent_incidents"`
		More *bool `json:"recent_incidents_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &render); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(render.Recent) != 10 || render.More == nil || !*render.More {
		t.Fatalf("recent = %d, more = %v; want 10 and true", len(render.Recent), render.More)
	}
	if render.Recent[0].ID != "r00" || render.Recent[9].ID != "r09" {
		t.Fatalf("recent order = %s … %s, want r00 … r09 (newest first)", render.Recent[0].ID, render.Recent[9].ID)
	}

	fs = seededStore()
	pastIncident(fs, "only", now.Add(-time.Hour))
	rec = do(newPublicHandler(fs), outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	render.More = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &render)
	if render.More == nil || *render.More {
		t.Fatalf("one past incident: recent_incidents_more = %v, want present and false", render.More)
	}
}

// AC-0203-3: both history routes keep their render route's access rules.
func TestHistoryRoutesKeepTheRenderAccessRules(t *testing.T) {
	fs := seededStore()
	pub := newPublicHandler(fs)
	authed := newHandler(fs)
	cases := []struct {
		name string
		code int
		run  func() int
	}{
		{"public page", http.StatusOK, func() int {
			return do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history", "").Code
		}},
		{"internal page, public route", http.StatusNotFound, func() int {
			return do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/internal-status/history", "").Code
		}},
		{"unlisted, no token", http.StatusNotFound, func() int {
			return do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status/history", "").Code
		}},
		{"unlisted, wrong token", http.StatusNotFound, func() int {
			return do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status/history?token=wrong", "").Code
		}},
		{"unlisted, token", http.StatusOK, func() int {
			return do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status/history?token=tok123", "").Code
		}},
		{"internal page, member", http.StatusOK, func() int {
			return do(authed, o1Viewer, http.MethodGet, "/api/v1/status-pages/sp2/history", "").Code
		}},
		{"internal page, outsider", http.StatusNotFound, func() int {
			return do(authed, outsider, http.MethodGet, "/api/v1/status-pages/sp2/history", "").Code
		}},
	}
	for _, c := range cases {
		if got := c.run(); got != c.code {
			t.Errorf("%s: %d, want %d", c.name, got, c.code)
		}
	}
}

// AC-0203-4: every malformed or out-of-range parameter is refused with its own code, and before
// the store is asked anything.
func TestHistoryRefusesBadMonthsAndCursorsBeforeReading(t *testing.T) {
	now := time.Now().UTC()
	cur := monthStartUTC(now)
	tooOld := monthStartUTC(now.AddDate(0, 0, -150)).Format("2006-01")
	future := cur.AddDate(0, 2, 0).Format("2006-01")
	enc := func(s string) string { return url.QueryEscape(encodeTestCursor(s)) }
	outside := enc(cur.AddDate(0, -1, 0).Add(time.Hour).Format(time.RFC3339Nano) + "|" + "00000000-0000-4000-8000-000000000001")
	cases := map[string]string{
		"month=2026-9":                  "invalid_month",
		"month=abc":                     "invalid_month",
		"month=2026-13":                 "invalid_month",
		"month=" + tooOld:               "month_outside_history",
		"month=" + future:               "month_outside_history",
		"cursor=%21%21%21":              "invalid_cursor",
		"cursor=" + enc("not-a-time|x"): "invalid_cursor",
		"cursor=" + enc(now.Format(time.RFC3339Nano)+"|not-a-uuid"): "invalid_cursor",
		"cursor=" + outside: "invalid_cursor",
		// Review R1-4: the window ends at now, so a cursor after now is no position in it.
		"cursor=" + enc(now.Add(2*time.Second).Format(time.RFC3339Nano)+"|00000000-0000-4000-8000-000000000001"): "invalid_cursor",
		// Review R1-6: one position has exactly one spelling — padded base64 and a non-canonical time
		// spell the same position as a valid cursor and would each be a new cache key.
		"cursor=" + url.QueryEscape(base64.URLEncoding.EncodeToString([]byte(now.Add(-time.Second).UTC().Format(time.RFC3339Nano)+"|00000000-0000-4000-8000-000000000001"))+"=="): "invalid_cursor",
		// Truncated to milliseconds so the fixed nine-digit spelling differs from RFC3339Nano's.
		"cursor=" + enc(now.Add(-time.Second).Truncate(time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")+"|00000000-0000-4000-8000-000000000001"): "invalid_cursor",
	}
	for q, code := range cases {
		fs := seededStore()
		rec := do(newPublicHandler(fs), outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?"+q, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), code) {
			t.Errorf("%s: %d %s, want 400 %s", q, rec.Code, rec.Body.String(), code)
		}
		if fs.historyCalls != 0 || fs.pageIncidentCalls != 0 {
			t.Errorf("%s: the store was read (%d history, %d page) before the request was validated", q, fs.historyCalls, fs.pageIncidentCalls)
		}
	}
}

// AC-0203-5 / AC-0203-6 (handler half): the month is paged at 50 with an opaque cursor; the months
// are the offered ones, newest first, with their counts; from/to clip to the window.
func TestHistoryPagesAMonthAndListsTheOfferedMonths(t *testing.T) {
	fs := seededStore()
	now := time.Now().UTC()
	cur := monthStartUTC(now)
	prev := cur.AddDate(0, -1, 0)
	// Ids are canonical UUIDs, as the store returns them: a cursor carries one, and the handler
	// refuses anything else in that position.
	uuidFor := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }
	for i := 0; i < 55; i++ {
		pastIncident(fs, uuidFor(i), prev.Add(time.Duration(i+1)*time.Minute))
	}
	h := newPublicHandler(fs)
	base := "/api/v1/public/status-pages/acme-status/history?month=" + prev.Format("2006-01")
	rec := do(h, outsider, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d %s", rec.Code, rec.Body.String())
	}
	first := decodeHistory(t, rec.Body.Bytes())
	if first.Month != prev.Format("2006-01") || len(first.Incidents) != 50 || first.NextCursor == nil {
		t.Fatalf("first page: month %s, %d incidents, cursor %v; want %s, 50, a cursor",
			first.Month, len(first.Incidents), first.NextCursor, prev.Format("2006-01"))
	}
	if first.Incidents[0].ID != uuidFor(54) {
		t.Fatalf("first page starts at %s, want %s (newest first)", first.Incidents[0].ID, uuidFor(54))
	}
	since := now.Add(-90 * 24 * time.Hour)
	var want []string
	for m := cur; !m.AddDate(0, 1, 0).Before(since) && !m.AddDate(0, 1, 0).Equal(since); m = m.AddDate(0, -1, 0) {
		want = append(want, m.Format("2006-01"))
	}
	var got []string
	for _, m := range first.Months {
		got = append(got, m.Month)
		if m.Month == prev.Format("2006-01") && m.Count != 55 {
			t.Fatalf("count for %s = %d, want 55", m.Month, m.Count)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("months = %v, want the offered months newest first %v", got, want)
	}

	rec = do(h, outsider, http.MethodGet, base+"&cursor="+url.QueryEscape(*first.NextCursor), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second page = %d %s", rec.Code, rec.Body.String())
	}
	second := decodeHistory(t, rec.Body.Bytes())
	if len(second.Incidents) != 5 || second.NextCursor != nil {
		t.Fatalf("second page: %d incidents, cursor %v; want 5 and none", len(second.Incidents), second.NextCursor)
	}
	seen := map[string]bool{}
	for _, in := range append(first.Incidents, second.Incidents...) {
		if seen[in.ID] {
			t.Fatalf("%s returned twice", in.ID)
		}
		seen[in.ID] = true
	}

	// The default month is the current UTC month; its `to` is now, the oldest month's `from` is the
	// window's start.
	rec = do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history", "")
	def := decodeHistory(t, rec.Body.Bytes())
	if def.Title != "Acme" {
		t.Fatalf("title = %q, want the page's title for the history header", def.Title)
	}
	if def.Month != cur.Format("2006-01") {
		t.Fatalf("default month = %s, want %s", def.Month, cur.Format("2006-01"))
	}
	oldest := want[len(want)-1]
	rec = do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+oldest, "")
	old := decodeHistory(t, rec.Body.Bytes())
	if old.From != old.HistoryFrom {
		t.Fatalf("oldest month from = %s, want the window start %s", old.From, old.HistoryFrom)
	}
}

// AC-0203-7: a history incident is the page's projection — timeline, postmortem, the page-local
// affected components — and the public route redacts it the same way.
func TestHistoryIncidentsUseThePageProjectionAndRedaction(t *testing.T) {
	fs := seededStore()
	at := time.Now().Add(-2 * time.Hour)
	pastIncident(fs, "hx", at)
	in := fs.incidents["hx"]
	in.MonitorID = "mon1"
	fs.incidents["hx"] = in
	fs.incUpdates["hx"] = []domain.IncidentUpdate{{ID: "u1", IncidentID: "hx", Status: domain.IncidentResolved, Body: "fixed", Author: "op-uuid"}}
	fs.postmortems["hx"] = domain.Postmortem{ID: "pm1", IncidentID: "hx", Body: "## Summary\nx", Author: "op"}

	rec := do(newPublicHandler(fs), outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history", "")
	body := decodeHistory(t, rec.Body.Bytes())
	if len(body.Incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(body.Incidents))
	}
	got := body.Incidents[0]
	if got.ProjectID != "" || got.MonitorID != "" {
		t.Fatalf("public history leaked internal anchors: project %q monitor %q", got.ProjectID, got.MonitorID)
	}
	if len(got.Updates) != 1 || got.Updates[0].ID != "" || got.Updates[0].Author != "" || got.Updates[0].Body != "fixed" {
		t.Fatalf("updates = %+v, want the body without id or author", got.Updates)
	}
	if got.Postmortem == nil || got.Postmortem.ID != "" || got.Postmortem.Author != "" {
		t.Fatalf("postmortem = %+v, want present and redacted", got.Postmortem)
	}
	if len(got.AffectedComponentIDs) == 0 {
		t.Fatalf("affected_component_ids = %v, want the page component bound to mon1", got.AffectedComponentIDs)
	}

	rec = do(newHandler(fs), o1Viewer, http.MethodGet, "/api/v1/status-pages/sp1/history", "")
	authed := decodeHistory(t, rec.Body.Bytes())
	if len(authed.Incidents) != 1 || authed.Incidents[0].ProjectID != "p1" {
		t.Fatalf("authenticated history = %+v, want the operator fields kept", authed.Incidents)
	}
	if strings.Join(authed.Incidents[0].AffectedComponentIDs, ",") != strings.Join(got.AffectedComponentIDs, ",") {
		t.Fatalf("affected components differ between routes: %v vs %v", authed.Incidents[0].AffectedComponentIDs, got.AffectedComponentIDs)
	}
}

// Review R1-1, over HTTP and in both fill orders: a render requested with a token that spells the
// history's key must not be served to the next history visitor, and the other way round.
func TestRenderAndHistoryNeverServeEachOthersCachedBytes(t *testing.T) {
	month := monthStartUTC(time.Now()).Format("2006-01")
	forged := url.QueryEscape("|history|" + month + "|")
	for _, order := range []string{"render first", "history first"} {
		fs := seededStore()
		h := newPublicHandler(fs)
		render := func() []byte {
			return do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status?token="+forged, "").Body.Bytes()
		}
		history := func() []byte {
			return do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+month, "").Body.Bytes()
		}
		var r, hi []byte
		if order == "render first" {
			r, hi = render(), history()
		} else {
			hi, r = history(), render()
		}
		var asHistory map[string]any
		_ = json.Unmarshal(hi, &asHistory)
		if _, ok := asHistory["incidents"]; !ok {
			t.Fatalf("%s: the history request was served something that is not a history: %s", order, hi)
		}
		var asRender map[string]any
		_ = json.Unmarshal(r, &asRender)
		if _, ok := asRender["components"]; !ok {
			t.Fatalf("%s: the render request was served something that is not a render: %s", order, r)
		}
	}
}

// Review R1-6: a cursor is any valid position, so a caller can mint new cache keys at will and every
// one of them is a cache miss — the bounded cache bounds memory, not work. Uncached public history
// renders are therefore limited per process, independently of the key: with the limit's worth of
// renders in flight, the next distinct request is refused with 429 and Retry-After instead of
// queueing more work. The limit is 8 (historyRenderSlots); a mutation changing it fails here.
func TestPublicHistoryRendersAreLimitedIndependentlyOfTheKey(t *testing.T) {
	fs := seededStore()
	fs.historyGate = make(chan struct{})
	fs.historyInside = new(atomic.Int32)
	h := newPublicHandler(fs)
	month := monthStartUTC(time.Now()).Format("2006-01")
	at := monthStartUTC(time.Now())
	cursor := func(i int) string {
		return url.QueryEscape(encodeTestCursor(at.Format(time.RFC3339Nano) + "|" + fmt.Sprintf("00000000-0000-4000-8000-%012d", i)))
	}
	if time.Since(at) < time.Second { // the cursor must lie before now; avoid the month's first second
		time.Sleep(time.Second)
	}
	const slots = 8
	codes := make(chan int, slots)
	for i := 0; i < slots; i++ {
		go func(i int) {
			codes <- do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+month+"&cursor="+cursor(i), "").Code
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for fs.historyInside.Load() < slots {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d history renders started: the limit is below %d", fs.historyInside.Load(), slots, slots)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Without a limit the extra request would join the held ones and wait at the gate: give it a
	// bounded time to be refused, then release everything either way.
	extraDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		extraDone <- do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+month+"&cursor="+cursor(99), "")
	}()
	var extra *httptest.ResponseRecorder
	select {
	case extra = <-extraDone:
	case <-time.After(3 * time.Second):
		close(fs.historyGate)
		t.Fatalf("a render beyond %d in flight was not refused: it started rendering and waited with the others", slots)
	}
	close(fs.historyGate)
	if extra.Code != http.StatusTooManyRequests || extra.Header().Get("Retry-After") == "" ||
		!strings.Contains(extra.Body.String(), "history_busy") {
		t.Fatalf("a render beyond %d in flight = %d %q (Retry-After %q), want 429 history_busy with Retry-After",
			slots, extra.Code, extra.Body.String(), extra.Header().Get("Retry-After"))
	}
	for i := 0; i < slots; i++ {
		if c := <-codes; c != http.StatusOK {
			t.Fatalf("a render inside the limit = %d, want 200", c)
		}
	}
	// The refusal is not cached: once the slots are free the same request is served.
	again := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+month+"&cursor="+cursor(99), "")
	if again.Code != http.StatusOK {
		t.Fatalf("after the burst the refused request = %d, want 200 (a 429 must not be cached)", again.Code)
	}
}

// AC-0203-8: the public history is cached with month and cursor in the key, and refuses a page
// above the component ceiling.
func TestPublicHistoryCacheKeyAndCeiling(t *testing.T) {
	fs := seededStore()
	now := time.Now().UTC()
	cur := monthStartUTC(now)
	prev := cur.AddDate(0, -1, 0)
	pastIncident(fs, "in-prev", prev.Add(time.Hour))
	h := newPublicHandler(fs)
	a := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+prev.Format("2006-01"), "")
	b := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+cur.Format("2006-01"), "")
	if decodeHistory(t, a.Body.Bytes()).Month == decodeHistory(t, b.Body.Bytes()).Month {
		t.Fatalf("two months served the same bytes: the cache key ignores the month")
	}
	again := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/history?month="+prev.Format("2006-01"), "")
	if again.Header().Get("X-Cerbix-Cache") != "hit" {
		t.Fatalf("a repeated public history request within the TTL was not served from the render cache")
	}

	fs.pages["spover"] = domain.StatusPage{ID: "spover", OrgID: "o1", Slug: "over-status", Title: "Over", Visibility: domain.VisibilityPublic}
	for i := 0; i < 501; i++ {
		id := fmt.Sprintf("o%04d", i)
		fs.components[id] = domain.Component{ID: id, StatusPageID: "spover", OrgID: "o1",
			Name: id, Source: domain.ComponentSourceManual, ManualStatus: domain.CompOperational}
	}
	rec := do(newPublicHandler(fs), outsider, http.MethodGet, "/api/v1/public/status-pages/over-status/history", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "status_page_over_safe_limit") {
		t.Fatalf("over-limit public history = %d %s, want 503 status_page_over_safe_limit", rec.Code, rec.Body.String())
	}
}
