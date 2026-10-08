package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0203 (func-status-pages-incidents.md §13, FR-039 / NFR-033): the page reads a BOUNDED number
// of past incidents, and the rest are paged by UTC month with a keyset cursor.

// historyFixture is a project with incidents whose resolution instants the test chooses. The
// product cannot backdate a resolution, so the rows are adjusted in SQL after creation; everything
// the queries read (project, status, resolved_at, id) is a real column of a real incident.
type historyFixture struct {
	st      *Store
	ctx     context.Context
	project string
	other   string
}

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "hist", "History")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	p, err := st.CreateProject(ctx, org.ID, "hist", "History")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	o, err := st.CreateProject(ctx, org.ID, "elsewhere", "Elsewhere")
	if err != nil {
		t.Fatalf("other project: %v", err)
	}
	return historyFixture{st: st, ctx: ctx, project: p.ID, other: o.ID}
}

// resolved creates an incident in project and marks it resolved at `at`. It returns its id.
func (f historyFixture) resolved(t *testing.T, project, title string, at time.Time) string {
	t.Helper()
	inc, err := f.st.CreateIncidentBySystem(f.ctx, domain.Incident{
		ProjectID: project, Title: title, Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, "", "test")
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	if _, err := f.st.pool.Exec(f.ctx,
		`UPDATE incidents SET status = 'resolved', resolved_at = $2::timestamptz,
		        started_at = $2::timestamptz - interval '5 minutes' WHERE id = $1`,
		inc.ID, at); err != nil {
		t.Fatalf("resolve %q: %v", title, err)
	}
	return inc.ID
}

func (f historyFixture) open(t *testing.T, title string) string {
	t.Helper()
	inc, err := f.st.CreateIncidentBySystem(f.ctx, domain.Incident{
		ProjectID: f.project, Title: title, Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, "", "test")
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return inc.ID
}

func titles(incs []domain.Incident) []string {
	out := make([]string, len(incs))
	for i, in := range incs {
		out[i] = in.Title
	}
	return out
}

// AC-0203-1 / AC-0203-2: the page reads at most `limit` past incidents, the newest first, and only
// flags — never returns — the ones beyond.
func TestPageListsAtMostItsLimitOfPastIncidentsAndFlagsTheRest(t *testing.T) {
	f := newHistoryFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-90 * 24 * time.Hour)
	for i := 0; i < 12; i++ {
		f.resolved(t, f.project, fmt.Sprintf("past-%02d", i), now.Add(-time.Duration(i+1)*time.Hour))
	}
	f.resolved(t, f.project, "too-old", since.Add(-time.Minute))
	f.resolved(t, f.other, "other-project", now.Add(-30*time.Minute))
	f.open(t, "still-open")

	got, err := f.st.IncidentsForPage(f.ctx, []string{f.project}, since, now, 10)
	if err != nil {
		t.Fatalf("page incidents: %v", err)
	}
	want := []string{"past-00", "past-01", "past-02", "past-03", "past-04", "past-05", "past-06", "past-07", "past-08", "past-09"}
	if strings.Join(titles(got.Recent), ",") != strings.Join(want, ",") {
		t.Fatalf("recent = %v, want the ten newest %v", titles(got.Recent), want)
	}
	if !got.RecentMore {
		t.Fatalf("twelve past incidents and a limit of ten: RecentMore is false, so the page would hide two without a link")
	}
	if len(got.Active) != 1 || got.Active[0].Title != "still-open" {
		t.Fatalf("active = %v, want only still-open: the limit is for past incidents alone", titles(got.Active))
	}

	// Exactly at the limit there is nothing more to show.
	if _, err := f.st.pool.Exec(f.ctx, `DELETE FROM incidents WHERE title IN ('past-10', 'past-11')`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = f.st.IncidentsForPage(f.ctx, []string{f.project}, since, now, 10)
	if err != nil {
		t.Fatalf("page incidents: %v", err)
	}
	if len(got.Recent) != 10 || got.RecentMore {
		t.Fatalf("exactly ten past incidents: got %d with RecentMore=%v, want 10 and false", len(got.Recent), got.RecentMore)
	}
}

// §13.2: history order is resolved_at DESC, then id DESC — total, so a cursor is well defined.
//
// The ORDER BY must say so itself. Through the partial index the rows already come out in id order,
// so a query that forgot the tie-break would still pass on the index plan; the first version of this
// test did exactly that and survived the mutation removing it. Here index scans are disabled, so
// the rows arrive in heap (insertion) order and only the ORDER BY can put six tied incidents with
// random ids into id-descending order.
func TestPastIncidentsWithTheSameResolutionInstantAreOrderedByIDDescending(t *testing.T) {
	f := newHistoryFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(-time.Hour)
	// Three tied incidents in EACH of two projects: the per-project LATERAL orders each project's
	// rows, and only the outer ORDER BY can interleave the two projects by id (a mutation dropping
	// the outer tie-break survived the one-project version of this test).
	var ids []string
	for i := 0; i < 6; i++ {
		project := f.project
		if i%2 == 1 {
			project = f.other
		}
		ids = append(ids, f.resolved(t, project, fmt.Sprintf("tie-%d", i), at))
	}
	want := append([]string(nil), ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(want)))

	conn, err := f.st.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(f.ctx, `SET enable_indexscan = off; SET enable_bitmapscan = off; SET enable_indexonlyscan = off`); err != nil {
		t.Fatalf("set: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ALL`) }()

	since := now.Add(-90 * 24 * time.Hour)
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	for name, run := range map[string]func() ([]domain.Incident, error){
		"page": func() ([]domain.Incident, error) {
			return f.st.queryIncidents(f.ctx, conn, pastIncidentsPageSQL, []string{f.project, f.other}, since, now, 11)
		},
		"month": func() ([]domain.Incident, error) {
			return f.st.queryIncidents(f.ctx, conn, historyMonthSQL, []string{f.project, f.other}, since, now,
				month, month.AddDate(0, 1, 0), 51)
		},
	} {
		got, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		gotIDs := make([]string, len(got))
		for i, in := range got {
			gotIDs[i] = in.ID
		}
		if strings.Join(gotIDs, ",") != strings.Join(want, ",") {
			t.Fatalf("%s query, tied resolution instants:\n got %v\nwant %v (id descending)", name, gotIDs, want)
		}
	}
}

// AC-0203-5 / AC-0203-6: a month holds only its own incidents inside the window; counts are per UTC
// month; the oldest month starts at `since`, not on its first day.
func TestHistoryMonthBoundsAreUTCAndTheOldestMonthStartsAtTheWindow(t *testing.T) {
	f := newHistoryFixture(t)
	since := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	f.resolved(t, f.project, "jul-before-window", since.Add(-time.Second))
	f.resolved(t, f.project, "jul-at-window", since) // the window is OPEN at since: excluded
	f.resolved(t, f.project, "jul-in", since.Add(time.Second))
	f.resolved(t, f.project, "sep-last-instant", time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC))
	f.resolved(t, f.project, "oct-first-instant", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	// 02:00 on 1 Oct at UTC+4 is still 30 Sep in UTC: it belongs to September.
	f.resolved(t, f.project, "sep-by-utc", time.Date(2026, 10, 1, 2, 0, 0, 0, time.FixedZone("UTC+4", 4*3600)))
	f.resolved(t, f.other, "other-project-sep", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC))

	until := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	sep := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	page, err := f.st.IncidentHistory(f.ctx, []string{f.project}, since, until, sep, sep.AddDate(0, 1, 0), nil, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	// sep-last-instant is 30 Sep 23:59:59 UTC; sep-by-utc is 30 Sep 22:00 UTC. Newest first.
	if got := strings.Join(titles(page.Incidents), ","); got != "sep-last-instant,sep-by-utc" {
		t.Fatalf("September = %q, want sep-last-instant,sep-by-utc (UTC month, newest first, own project only)", got)
	}
	if page.More {
		t.Fatalf("two incidents and a page of 50: More is true")
	}
	want := map[string]int{"2026-07": 1, "2026-09": 2, "2026-10": 1}
	if len(page.Counts) != len(want) {
		t.Fatalf("counts = %v, want %v", page.Counts, want)
	}
	for m, n := range want {
		if page.Counts[m] != n {
			t.Fatalf("counts = %v, want %v (month %s)", page.Counts, want, m)
		}
	}

	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	page, err = f.st.IncidentHistory(f.ctx, []string{f.project}, since, until, jul, jul.AddDate(0, 1, 0), nil, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if got := strings.Join(titles(page.Incidents), ","); got != "jul-in" {
		t.Fatalf("July = %q, want only jul-in: the oldest month starts at the window, which is open at since", got)
	}
}

// AC-0203-5: a traversal by cursor returns every incident of the month exactly once, in history
// order, never more than the page size — including across a tie on resolved_at at a page boundary.
func TestHistoryKeysetReturnsEachIncidentOnceAcrossPagesAndTies(t *testing.T) {
	f := newHistoryFixture(t)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	since := oct.AddDate(0, 0, -80)
	tie := oct.Add(48 * time.Hour)
	want := map[string]bool{}
	for i := 0; i < 4; i++ { // four share one instant, so a page boundary falls inside the tie
		want[f.resolved(t, f.project, fmt.Sprintf("tie-%d", i), tie)] = true
	}
	for i := 0; i < 3; i++ {
		want[f.resolved(t, f.project, fmt.Sprintf("solo-%d", i), oct.Add(time.Duration(i+1)*time.Hour))] = true
	}
	f.resolved(t, f.project, "next-month", oct.AddDate(0, 1, 0))

	seen := map[string]bool{}
	var order []domain.Incident
	var after *HistoryCursor
	var sizes []int
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatalf("traversal did not end after 5 pages: sizes %v", sizes)
		}
		page, err := f.st.IncidentHistory(f.ctx, []string{f.project}, since, oct.AddDate(0, 2, 0), oct, oct.AddDate(0, 1, 0), after, 3)
		if err != nil {
			t.Fatalf("history page %d: %v", pages, err)
		}
		sizes = append(sizes, len(page.Incidents))
		for _, in := range page.Incidents {
			if seen[in.ID] {
				t.Fatalf("incident %s (%s) returned twice: sizes %v", in.ID, in.Title, sizes)
			}
			seen[in.ID] = true
			order = append(order, in)
		}
		if !page.More {
			break
		}
		last := page.Incidents[len(page.Incidents)-1]
		after = &HistoryCursor{ResolvedAt: *last.ResolvedAt, ID: last.ID}
	}
	if fmt.Sprint(sizes) != "[3 3 1]" {
		t.Fatalf("page sizes %v, want [3 3 1] for seven incidents at three per page", sizes)
	}
	if len(seen) != len(want) {
		t.Fatalf("traversal returned %d incidents, want the month's %d", len(seen), len(want))
	}
	for i := 1; i < len(order); i++ {
		a, b := order[i-1], order[i]
		if a.ResolvedAt.Before(*b.ResolvedAt) || (a.ResolvedAt.Equal(*b.ResolvedAt) && a.ID < b.ID) {
			t.Fatalf("order went backwards at %d: %s (%s) before %s (%s)", i, a.Title, a.ID, b.Title, b.ID)
		}
	}
}

// §13.4 rule 3 / AC-0203-6: the month counts and the list are read in ONE snapshot. The seam runs
// between the two statements and resolves a new incident from another connection; neither the
// counts nor the list may see it, so they cannot disagree.
func TestHistoryCountsAndListAreReadInOneSnapshot(t *testing.T) {
	f := newHistoryFixture(t)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	since := oct.AddDate(0, 0, -80)
	f.resolved(t, f.project, "before", oct.Add(time.Hour))

	ran := false
	f.st.historyBetweenReads = func() {
		ran = true
		f.resolved(t, f.project, "during", oct.Add(2*time.Hour))
	}
	t.Cleanup(func() { f.st.historyBetweenReads = nil })

	page, err := f.st.IncidentHistory(f.ctx, []string{f.project}, since, oct.AddDate(0, 1, 0), oct, oct.AddDate(0, 1, 0), nil, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !ran {
		t.Fatalf("the seam between the two reads never ran, so this test proves nothing")
	}
	if page.Counts["2026-10"] != len(page.Incidents) {
		t.Fatalf("October count %d but %d incidents listed (%v): the two reads saw different data",
			page.Counts["2026-10"], len(page.Incidents), titles(page.Incidents))
	}
	if got := strings.Join(titles(page.Incidents), ","); got != "before" {
		t.Fatalf("list = %q, want only the incident that existed when the snapshot began", got)
	}
}

// Review R1-4: the window is (since, until] and the response says `to = until`, so nothing resolved
// after `until` may appear — not in the list and not in the counts — and a cursor may not sit there.
func TestHistoryAndPageExcludeIncidentsResolvedAfterUntil(t *testing.T) {
	f := newHistoryFixture(t)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := oct.Add(10 * 24 * time.Hour)
	since := until.Add(-90 * 24 * time.Hour)
	f.resolved(t, f.project, "at-until", until)
	f.resolved(t, f.project, "after-until", until.Add(time.Second))

	page, err := f.st.IncidentHistory(f.ctx, []string{f.project}, since, until, oct, oct.AddDate(0, 1, 0), nil, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if got := strings.Join(titles(page.Incidents), ","); got != "at-until" {
		t.Fatalf("list = %q, want only at-until: the window ends at until", got)
	}
	if page.Counts["2026-10"] != 1 {
		t.Fatalf("October count = %d, want 1: the counts read past until", page.Counts["2026-10"])
	}
	got, err := f.st.IncidentsForPage(f.ctx, []string{f.project}, since, until, 10)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if strings.Join(titles(got.Recent), ",") != "at-until" {
		t.Fatalf("page recent = %v, want only at-until", titles(got.Recent))
	}
}

// Review R1-5: LIMIT bounded what the queries RETURNED, not what they READ. With several projects
// on a page, an index led by project_id gives no global (resolved_at, id) order, so the planner read
// every matching row and sorted them for the top N. Each list is now a per-project top-K through
// the index (LATERAL), then a global top-K. Proved on the planner's own choice — no setting is
// changed — over two projects with 3 000 past incidents each and fresh statistics: the incident
// rows the plan reads must not exceed K per project.
func TestPastIncidentListsReadAtMostTheirLimitPerProject(t *testing.T) {
	f := newHistoryFixture(t)
	sep30 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{f.project, f.other} {
		if _, err := f.st.pool.Exec(f.ctx, `
			INSERT INTO incidents (project_id, title, status, impact, source, started_at, resolved_at)
			SELECT $1, 'bulk-' || g, 'resolved', 'minor', 'manual',
			       $2::timestamptz - g * interval '1 minute' - interval '5 minutes',
			       $2::timestamptz - g * interval '1 minute'
			  FROM generate_series(1, 3000) g`, p, sep30); err != nil {
			t.Fatalf("bulk insert: %v", err)
		}
	}
	if _, err := f.st.pool.Exec(f.ctx, `ANALYZE incidents`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	ids := []string{f.project, f.other}
	until := sep30
	since := until.Add(-90 * 24 * time.Hour)
	sep := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		sql   string
		args  []any
		limit int
	}{
		"page":  {pastIncidentsPageSQL, []any{ids, since, until, 11}, 11},
		"month": {historyMonthSQL, []any{ids, since, until, sep, sep.AddDate(0, 1, 0), 51}, 51},
	}
	for name, c := range cases {
		var raw []byte
		if err := f.st.pool.QueryRow(f.ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+c.sql, c.args...).Scan(&raw); err != nil {
			t.Fatalf("%s explain: %v", name, err)
		}
		read := incidentRowsRead(t, raw)
		if read > float64(len(ids)*c.limit) {
			t.Fatalf("%s query read %.0f incident rows for a limit of %d over %d projects; want at most %d.\nplan: %s",
				name, read, c.limit, len(ids), len(ids)*c.limit, raw)
		}
		// The bound comes from the partial index's order. (A separate test that only disabled
		// sequential scans on a near-empty table was dropped in review round 1: it proved the index
		// existed, not that a real plan uses it, and plain PostgreSQL chose another index there.)
		if !strings.Contains(string(raw), "incidents_resolved_history_idx") {
			t.Fatalf("%s query's plan does not use incidents_resolved_history_idx:\n%s", name, raw)
		}
	}
}

// Review round 2: the cursor page under a GENERIC plan. pgx caches prepared statements, and after a
// few executions PostgreSQL may plan them generically; a keyset written as `$6 IS NULL OR (…) < ($6,
// $7)` then cannot be an index condition and becomes a Filter that walks the month from its newest
// row to the cursor. The cursor page is its own statement with a plain tuple comparison, which is an
// index condition in a generic plan too. The plan is forced generic here with PREPARE and
// plan_cache_mode — that is the state production reaches on its own — and the cursor sits deep in
// the month, so a Filter-shaped keyset would discard thousands of rows.
func TestHistoryCursorPageReadsAtMostItsLimitUnderAGenericPlan(t *testing.T) {
	f := newHistoryFixture(t)
	sep30 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{f.project, f.other} {
		if _, err := f.st.pool.Exec(f.ctx, `
			INSERT INTO incidents (project_id, title, status, impact, source, started_at, resolved_at)
			SELECT $1, 'bulk-' || g, 'resolved', 'minor', 'manual',
			       $2::timestamptz - g * interval '1 minute' - interval '5 minutes',
			       $2::timestamptz - g * interval '1 minute'
			  FROM generate_series(1, 3000) g`, p, sep30); err != nil {
			t.Fatalf("bulk insert: %v", err)
		}
	}
	if _, err := f.st.pool.Exec(f.ctx, `ANALYZE incidents`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// A cursor 2 800 minutes back: deep in the month for both projects.
	var curAt time.Time
	var curID string
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT resolved_at, id::text FROM incidents
		 WHERE project_id = $1 AND title = 'bulk-2800'`, f.project).Scan(&curAt, &curID); err != nil {
		t.Fatalf("cursor row: %v", err)
	}

	conn, err := f.st.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	defer func() {
		_, _ = conn.Exec(context.Background(), `DEALLOCATE ALL`)
		_, _ = conn.Exec(context.Background(), `RESET plan_cache_mode`)
	}()
	if _, err := conn.Exec(f.ctx, `SET plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := conn.Exec(f.ctx, `PREPARE hist_after(uuid[], timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, uuid, int) AS `+
		historyMonthAfterSQL); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	until := sep30
	since := until.Add(-90 * 24 * time.Hour)
	sep := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	lit := func(t time.Time) string { return "'" + t.UTC().Format(time.RFC3339Nano) + "'::timestamptz" }
	exec := fmt.Sprintf(`EXECUTE hist_after(ARRAY['%s','%s']::uuid[], %s, %s, %s, %s, %s, '%s'::uuid, 51)`,
		f.project, f.other, lit(since), lit(until), lit(sep), lit(sep.AddDate(0, 1, 0)), lit(curAt), curID)
	var raw []byte
	if err := conn.QueryRow(f.ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+exec).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(string(raw), "$6") && !strings.Contains(string(raw), "$7") {
		t.Fatalf("the plan is not generic (no parameter symbols in it), so this test proves nothing:\n%s", raw)
	}
	if read := incidentRowsRead(t, raw); read > 2*51 {
		t.Fatalf("the cursor page read %.0f incident rows under a generic plan; want at most %d.\nplan: %s", read, 2*51, raw)
	}
}

// incidentRowsRead sums, over every plan node scanning `incidents`, the rows it produced times its
// loops — what the statement actually read from the table.
func incidentRowsRead(t *testing.T, raw []byte) float64 {
	t.Helper()
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) == 0 {
		t.Fatalf("decode plan: %v", err)
	}
	var walk func(n map[string]any) float64
	walk = func(n map[string]any) float64 {
		total := 0.0
		if n["Relation Name"] == "incidents" {
			// A scan READS the rows it returns AND the rows its filter or recheck throws away; all
			// three are per-loop averages in EXPLAIN. Counting returned rows alone hid a keyset that
			// had become a Filter: 51 returned per loop, 2 799 discarded (review round 2).
			rows, _ := n["Actual Rows"].(float64)
			filtered, _ := n["Rows Removed by Filter"].(float64)
			rechecked, _ := n["Rows Removed by Index Recheck"].(float64)
			loops, _ := n["Actual Loops"].(float64)
			total += (rows + filtered + rechecked) * loops
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					total += walk(m)
				}
			}
		}
		return total
	}
	return walk(plans[0].Plan)
}
