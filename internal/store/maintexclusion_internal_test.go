package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0204: every reader that aggregates heartbeats excludes the ones inside a maintenance
// window. The pilot measured that exclusion as a per-heartbeat probe of maintenance_windows —
// 1.4–1.6 million buffer reads for one window of one page, 99% of the statement — though the
// database held ONE window. These tests pin, for all eight readers, (1) WHAT is excluded, so a
// faster spelling cannot change the numbers, and (2) that maintenance_windows is read a bounded
// number of times per statement, not once per heartbeat.

type maintFixture struct {
	st         *Store
	ctx        context.Context
	p, q       string // two projects
	m1, m2, m3 string // m1, m2 in p; m3 in q
	base       time.Time
	since      time.Time
	dayStart   time.Time // start of the current UTC day
}

func newMaintFixture(t *testing.T) maintFixture {
	t.Helper()
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "mx", "Maint")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	p, err := st.CreateProject(ctx, org.ID, "mx-p", "P")
	if err != nil {
		t.Fatalf("project p: %v", err)
	}
	q, err := st.CreateProject(ctx, org.ID, "mx-q", "Q")
	if err != nil {
		t.Fatalf("project q: %v", err)
	}
	mk := func(project, name string) string {
		m, err := st.CreateMonitor(ctx, domain.Monitor{ProjectID: project, Name: name, Type: domain.MonitorHTTP,
			Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true})
		if err != nil {
			t.Fatalf("monitor %s: %v", name, err)
		}
		return m.ID
	}
	f := maintFixture{st: st, ctx: ctx, p: p.ID, q: q.ID, m1: mk(p.ID, "m1"), m2: mk(p.ID, "m2"), m3: mk(q.ID, "m3")}
	// Everything happens inside the current UTC day, anchored on its start, so the "today from
	// raw" readers see it too — deterministically, whatever the time of the run. (The first
	// version slept past midnight and could outlive its context; review R1-2.) Shortly after
	// midnight some heartbeats lie a little in the future; no reader has an upper bound, and the
	// rollup is run over the whole day.
	f.dayStart = utcDayStart(time.Now())
	f.base = f.dayStart.Add(10 * time.Second)
	f.since = f.dayStart.Add(-time.Minute)
	return f
}

func utcDayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (f maintFixture) beat(t *testing.T, monitor string, at time.Time, up bool) {
	t.Helper()
	if _, err := f.st.pool.Exec(f.ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg) VALUES ($1, $2, $3, 10, 200, 'ok')`,
		monitor, at, up); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func (f maintFixture) window(t *testing.T, project string, monitor *string, from, to time.Time, cancelAt *time.Time) {
	t.Helper()
	if _, err := f.st.pool.Exec(f.ctx,
		`INSERT INTO maintenance_windows (project_id, monitor_id, starts_at, ends_at, reason, cancel_effective_at)
		 VALUES ($1, $2, $3, $4, 'test', $5)`, project, monitor, from, to, cancelAt); err != nil {
		t.Fatalf("maintenance window: %v", err)
	}
}

// What is excluded, for every reader. Twelve heartbeats ten seconds apart on each monitor (k=5 is
// down), and four windows:
//
//	a. m1's own window [20s, 40s)                         excludes m1 k=2,3 (start inclusive, end exclusive)
//	b. p's project window [60s, 100s) cancelled at 80s     excludes m1 and m2 k=6,7 (the cancellation ends it)
//	c. q's project window over everything                  excludes all of m3, nothing of p
//	d. m2's own window [100s, 120s)                        excludes m2 k=10,11, nothing of m1
//
// So m1 and m2 each keep 8 heartbeats, 7 up; p keeps 16, 14 up; m3 and q keep nothing.
func TestMaintenanceExclusionIsTheSameRuleForEveryReader(t *testing.T) {
	f := newMaintFixture(t)
	at := func(sec int) time.Time { return f.base.Add(time.Duration(sec) * time.Second) }
	for _, m := range []string{f.m1, f.m2, f.m3} {
		for k := 0; k < 12; k++ {
			f.beat(t, m, at(10*k), k != 5)
		}
	}
	m1, m2 := f.m1, f.m2
	cancel := at(80)
	f.window(t, f.p, &m1, at(20), at(40), nil)
	f.window(t, f.p, nil, at(60), at(100), &cancel)
	f.window(t, f.q, nil, at(-60), at(200), nil)
	f.window(t, f.p, &m2, at(100), at(120), nil)

	type counts struct{ total, up int64 }
	want := map[string]counts{f.m1: {8, 7}, f.m2: {8, 7}, f.m3: {0, 0}}
	check := func(reader, scope string, gotTotal, gotUp int64, w counts) {
		t.Helper()
		if gotTotal != w.total || gotUp != w.up {
			t.Errorf("%s(%s): total=%d up=%d, want total=%d up=%d", reader, scope, gotTotal, gotUp, w.total, w.up)
		}
	}

	for _, m := range []string{f.m1, f.m2, f.m3} {
		c, err := f.st.MonitorSLI(f.ctx, m, f.since)
		if err != nil {
			t.Fatalf("MonitorSLI: %v", err)
		}
		check("MonitorSLI", m, c.Total, c.Up, want[m])

		var total, up int64
		var rollTotal, rollUp int64
		var firstRollup *time.Time
		var complete bool
		ceil, day, today := windowDays(f.since)
		if err := f.st.pool.QueryRow(f.ctx, burnWindowSQL, m, f.since, ceil, day, today).
			Scan(&total, &up, &rollTotal, &rollUp, &firstRollup, &complete); err != nil {
			t.Fatalf("burn window: %v", err)
		}
		check("burnWindow", m, total, up, want[m])

		d, ok, err := f.st.monitorTodayAvailability(f.ctx, m)
		if err != nil {
			t.Fatalf("monitorToday: %v", err)
		}
		if !ok {
			d = DailyAvailability{}
		}
		check("monitorToday", m, d.Total, d.Up, want[m])
	}

	pc, err := f.st.ProjectSLI(f.ctx, f.p, f.since)
	if err != nil {
		t.Fatalf("ProjectSLI: %v", err)
	}
	check("ProjectSLI", "p", pc.Total, pc.Up, counts{16, 14})
	qc, err := f.st.ProjectSLI(f.ctx, f.q, f.since)
	if err != nil {
		t.Fatalf("ProjectSLI: %v", err)
	}
	check("ProjectSLI", "q", qc.Total, qc.Up, counts{0, 0})

	pd, err := f.st.ProjectDailyAvailability(f.ctx, f.p, f.since)
	if err != nil {
		t.Fatalf("ProjectDailyAvailability: %v", err)
	}
	if len(pd) == 0 {
		t.Fatalf("ProjectDailyAvailability(p): no today row")
	}
	check("projectToday", "p", pd[len(pd)-1].Total, pd[len(pd)-1].Up, counts{16, 14})

	proj, err := f.st.MonitorPageProjections(f.ctx, []string{f.m1, f.m2, f.m3}, f.since, true)
	if err != nil {
		t.Fatalf("MonitorPageProjections: %v", err)
	}
	for _, m := range []string{f.m1, f.m2} {
		p := proj[m]
		if p.Uptime == nil || *p.Uptime != 7.0/8.0*100 {
			t.Errorf("MonitorPageProjections(%s) uptime = %v, want %v", m, p.Uptime, 7.0/8.0*100)
		}
		if len(p.Daily) == 0 {
			t.Errorf("MonitorPageProjections(%s): no today strip", m)
		} else {
			last := p.Daily[len(p.Daily)-1]
			check("monitorPageDaily", m, last.Total, last.Up, want[m])
		}
	}
	if u := proj[f.m3].Uptime; u != nil {
		t.Errorf("MonitorPageProjections(m3) uptime = %v, want nil (everything in maintenance)", *u)
	}

	dayStart := f.dayStart
	if err := f.st.RollupDailyAvailability(f.ctx, dayStart, dayStart.Add(24*time.Hour)); err != nil {
		t.Fatalf("RollupDailyAvailability: %v", err)
	}
	for _, m := range []string{f.m1, f.m2, f.m3} {
		var total, up int64
		err := f.st.pool.QueryRow(f.ctx, `SELECT total, up FROM heartbeats_daily WHERE monitor_id = $1 AND day = $2::date`,
			m, dayStart).Scan(&total, &up)
		if err != nil && err != pgx.ErrNoRows {
			t.Fatalf("read rollup: %v", err)
		}
		check("RollupDailyAvailability", m, total, up, want[m])
	}
}

// How often maintenance_windows is read. One project window; heartbeats of m1 and m2 one second
// apart from the start of the current UTC day (deterministic, whatever the time of the run). Each
// statement is EXPLAINed at 1 000 and again at 3 000 heartbeats per monitor: the number of reads of
// maintenance_windows must not grow with the heartbeats, and is bounded by the scope — at most one
// per monitor in the database (three here), never one per heartbeat (review R1: the bound is
// explained by the scope, and checked at more than one volume).
func TestMaintenanceWindowsAreReadOncePerStatementNotPerHeartbeat(t *testing.T) {
	f := newMaintFixture(t)
	day := f.dayStart
	insert := func(from, to int) {
		t.Helper()
		for _, m := range []string{f.m1, f.m2} {
			if _, err := f.st.pool.Exec(f.ctx, `
				INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg)
				SELECT $1, $2::timestamptz + g * interval '1 second', g % 17 <> 0, 10 + g % 50, 200, 'ok'
				  FROM generate_series($3::int, $4::int) g`, m, day, from, to); err != nil {
				t.Fatalf("bulk heartbeats: %v", err)
			}
		}
		if _, err := f.st.pool.Exec(f.ctx, `ANALYZE heartbeats; ANALYZE maintenance_windows`); err != nil {
			t.Fatalf("analyze: %v", err)
		}
	}
	f.window(t, f.p, nil, day.Add(10*time.Minute), day.Add(20*time.Minute), nil)
	since := day.Add(-2 * time.Hour)
	ceil, dd, today := windowDays(since)
	ids := []string{f.m1, f.m2}
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"MonitorSLI", monitorSLISQL, []any{f.m1, since, ceil, dd, today}},
		{"ProjectSLI", projectSLISQL, []any{f.p, since, ceil, dd, today}},
		{"burnWindow", burnWindowSQL, []any{f.m1, since, ceil, dd, today}},
		{"monitorPageSLI", monitorPageSLISQL, []any{ids, since, ceil, dd, today}},
		{"monitorPageDaily", monitorPageDailySQL, []any{ids, since}},
		{"monitorToday", monitorTodaySQL, []any{f.m1}},
		{"projectToday", projectTodaySQL, []any{f.p}},
		{"rollupDaily", rollupDailySQL, []any{day, day.Add(24 * time.Hour)}},
	}
	const monitorsInDB = 3
	measure := func() map[string]float64 {
		t.Helper()
		out := map[string]float64{}
		for _, c := range cases {
			tx, err := f.st.pool.Begin(f.ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			// The pilot's planner chose, on its own, a per-heartbeat index probe of
			// maintenance_windows. A small test table lets the planner prefer one materialized seq
			// scan instead, which hides the problem — the first run of this test passed against the
			// unfixed SQL. These two settings reproduce the pilot's plan shape; the fixed statements
			// must read the table a bounded number of times WHATEVER the planner prefers.
			if _, err := tx.Exec(f.ctx, `SET LOCAL enable_material = off; SET LOCAL enable_seqscan = off`); err != nil {
				t.Fatalf("set: %v", err)
			}
			var raw []byte
			err = tx.QueryRow(f.ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+c.sql, c.args...).Scan(&raw)
			_ = tx.Rollback(f.ctx) // the rollup statement writes; nothing may stay
			if err != nil {
				t.Fatalf("%s explain: %v", c.name, err)
			}
			out[c.name] = relationLoops(t, raw, "maintenance_windows")
		}
		return out
	}
	insert(1, 1000)
	small := measure()
	insert(1001, 3000)
	large := measure()
	for _, c := range cases {
		if large[c.name] > monitorsInDB {
			t.Errorf("%s reads maintenance_windows %0.f times at 3 000 heartbeats per monitor — more than once per monitor",
				c.name, large[c.name])
		}
		if large[c.name] != small[c.name] {
			t.Errorf("%s reads maintenance_windows %0.f times at 1 000 heartbeats per monitor and %0.f at 3 000 — it grows with the heartbeats",
				c.name, small[c.name], large[c.name])
		}
	}
}

// Review R1-1: the "today from raw" readers start today at the UTC midnight the rollup's days use,
// whatever the session's TimeZone. They compared a timestamptz with a UTC DATE, which PostgreSQL
// reads as midnight in the session's zone — on a +03 session (the pilot's) "today" began at 21:00Z
// the day before, counting three hours the rollup already counted for yesterday; on a negative
// zone it began hours after UTC midnight and dropped them. With iter-0204's pruning of maintenance
// windows at UTC midnight, a +zone session also lost the exclusion of a window just before it.
//
// Each reader runs on a store whose sessions are in Asia/Tokyo (+09), then America/Los_Angeles: the
// heartbeats are a down one at 23:30Z yesterday inside a maintenance window [23:15Z, 23:45Z), an up
// one at 23:50Z yesterday, and an up one at 00:00:01Z today. Today, in UTC, is exactly 1 up of 1.
func TestTodayReadersStartAtUTCMidnightWhateverTheSessionTimeZone(t *testing.T) {
	f := newMaintFixture(t)
	day := f.dayStart
	f.beat(t, f.m1, day.Add(-30*time.Minute), false)
	f.beat(t, f.m1, day.Add(-10*time.Minute), true)
	f.beat(t, f.m1, day.Add(time.Second), true)
	m1 := f.m1
	f.window(t, f.p, &m1, day.Add(-45*time.Minute), day.Add(-15*time.Minute), nil)

	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	for _, zone := range []string{"Asia/Tokyo", "America/Los_Angeles"} {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		st, err := Open(f.ctx, dsn+sep+"timezone="+zone)
		if err != nil {
			t.Fatalf("open %s: %v", zone, err)
		}
		var got string
		if err := st.pool.QueryRow(f.ctx, `SHOW timezone`).Scan(&got); err != nil || got != zone {
			st.Close()
			t.Fatalf("session timezone = %q (%v), want %s: this case proves nothing", got, err, zone)
		}
		d, ok, err := st.monitorTodayAvailability(f.ctx, f.m1)
		if err != nil || !ok || d.Total != 1 || d.Up != 1 {
			t.Errorf("%s monitorToday: %+v ok=%v err=%v, want 1 up of 1", zone, d, ok, err)
		}
		pd, err := st.ProjectDailyAvailability(f.ctx, f.p, day)
		if err != nil || len(pd) == 0 || pd[len(pd)-1].Total != 1 || pd[len(pd)-1].Up != 1 {
			t.Errorf("%s projectToday: %+v err=%v, want today 1 up of 1", zone, pd, err)
		}
		proj, err := st.MonitorPageProjections(f.ctx, []string{f.m1}, day.Add(-24*time.Hour), true)
		if err != nil {
			t.Fatalf("%s projections: %v", zone, err)
		}
		daily := proj[f.m1].Daily
		if len(daily) == 0 || daily[len(daily)-1].Total != 1 || daily[len(daily)-1].Up != 1 {
			t.Errorf("%s monitorPageDaily: %+v, want today 1 up of 1", zone, daily)
		}
		st.Close()
	}
}

// relationLoops sums Actual Loops over every plan node that scans `rel`.
func relationLoops(t *testing.T, raw []byte, rel string) float64 {
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
		if n["Relation Name"] == rel {
			loops, _ := n["Actual Loops"].(float64)
			total += loops
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
