package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0199 / D-0268 (BUG-0198-7): an SLI window longer than raw retention must read the daily
// rollup for the days whose raw heartbeats are gone, and state its coverage. Every fixture below
// models retention the way production does it — heartbeats are written, rolled up while they exist,
// and then the raw rows older than the cutoff are deleted — so the read under test sees exactly what
// a real installation sees 30 days after a purge.

type longWindowFixture struct {
	st      *Store
	ctx     context.Context
	project string
	today   time.Time
}

func newLongWindowFixture(t *testing.T) longWindowFixture {
	t.Helper()
	st, ctx := rollupTestStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return longWindowFixture{st: st, ctx: ctx, project: proj.ID, today: time.Now().UTC().Truncate(24 * time.Hour)}
}

func (f longWindowFixture) monitor(t *testing.T, name string) string {
	t.Helper()
	m, err := f.st.CreateMonitor(f.ctx, domain.Monitor{
		ProjectID: f.project, Name: name, Type: domain.MonitorHTTP, Target: "https://" + name,
		IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor %s: %v", name, err)
	}
	return m.ID
}

// beats writes `perDay` heartbeats at noon-ish on each UTC day in [fromDay, toDay) days before
// today (negative offsets are the past), `down` of them failing on the day `downDay`.
func (f longWindowFixture) beats(t *testing.T, monitorID string, fromDay, toDay, perDay int, downDay, down int) (total, up int64) {
	t.Helper()
	for d := fromDay; d < toDay; d++ {
		day := f.today.AddDate(0, 0, d)
		for i := 0; i < perDay; i++ {
			isUp := !(d == downDay && i < down)
			if _, err := f.st.pool.Exec(f.ctx,
				`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg) VALUES ($1,$2,$3,10,200,'')`,
				monitorID, day.Add(12*time.Hour+time.Duration(i)*time.Minute), isUp); err != nil {
				t.Fatalf("insert heartbeat: %v", err)
			}
			total++
			if isUp {
				up++
			}
		}
	}
	return total, up
}

// purge rolls the whole history up (as the leader does while the raw rows exist) and then deletes
// every raw heartbeat older than `retentionDays`, as the retention pass does.
func (f longWindowFixture) purge(t *testing.T, retentionDays int) time.Time {
	t.Helper()
	if err := f.st.RollupDailyAvailability(f.ctx, f.today.AddDate(0, 0, -200), f.today); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	cutoff := f.today.AddDate(0, 0, -retentionDays)
	if _, err := f.st.pool.Exec(f.ctx, `DELETE FROM heartbeats WHERE ts < $1`, cutoff); err != nil {
		t.Fatalf("purge: %v", err)
	}
	return cutoff
}

func sameDay(a *time.Time, want time.Time) bool {
	return a != nil && a.UTC().Truncate(24*time.Hour).Equal(want.UTC().Truncate(24*time.Hour))
}

// The defect itself: a monitor with 60 days of history and a 30-day raw retention. Its 90d window
// must count all 60 days (the older 30 from the rollup), not the 30 retained raw days.
func TestLongWindowCountsRollupDaysOlderThanRawRetention(t *testing.T) {
	f := newLongWindowFixture(t)
	mon := f.monitor(t, "checkout")
	total, up := f.beats(t, mon, -60, 0, 24, -50, 6) // six failures on a day the raw purge removes
	cutoff := f.purge(t, 30)

	c, err := f.st.MonitorSLI(f.ctx, mon, time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	if c.Total != total || c.Up != up {
		t.Fatalf("90d window counted up=%d total=%d, want up=%d total=%d (rollup days older than raw retention must count)",
			c.Up, c.Total, up, total)
	}
	// The monitor is 60 days old: the 90d window is incomplete and says where its data starts.
	if !sameDay(c.DataFrom, f.today.AddDate(0, 0, -60)) {
		t.Errorf("data_from = %v, want %s", c.DataFrom, f.today.AddDate(0, 0, -60).Format("2006-01-02"))
	}
	// Latency is raw-only, and raw starts at the purge cutoff, later than availability.
	if !sameDay(c.LatencyFrom, cutoff) {
		t.Errorf("latency_from = %v, want %s", c.LatencyFrom, cutoff.Format("2006-01-02"))
	}

	// The 30d window is covered by raw heartbeats: unchanged, complete, latency not qualified.
	c30, err := f.st.MonitorSLI(f.ctx, mon, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("monitor sli 30d: %v", err)
	}
	if c30.DataFrom != nil || c30.LatencyFrom != nil {
		t.Errorf("30d window over retained raw data reported data_from=%v latency_from=%v, want none", c30.DataFrom, c30.LatencyFrom)
	}
}

// A window that raw heartbeats cover is computed from raw exactly as before: a rollup row that
// disagrees with the raw rows must not leak in.
func TestLongWindowCoveredByRawIgnoresTheRollup(t *testing.T) {
	f := newLongWindowFixture(t)
	mon := f.monitor(t, "search")
	f.beats(t, mon, -10, 0, 24, -5, 3)
	if err := f.st.RollupDailyAvailability(f.ctx, f.today.AddDate(0, 0, -200), f.today); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if _, err := f.st.pool.Exec(f.ctx, `UPDATE heartbeats_daily SET up = 0 WHERE monitor_id = $1`, mon); err != nil {
		t.Fatalf("poison rollup: %v", err)
	}
	since := time.Now().Add(-7 * 24 * time.Hour)
	var wantTotal, wantUp int64
	if err := f.st.pool.QueryRow(f.ctx,
		`SELECT count(*), count(*) FILTER (WHERE up) FROM heartbeats WHERE monitor_id=$1 AND ts >= $2`,
		mon, since).Scan(&wantTotal, &wantUp); err != nil {
		t.Fatalf("raw truth: %v", err)
	}
	c, err := f.st.MonitorSLI(f.ctx, mon, since)
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	if c.Total != wantTotal || c.Up != wantUp || c.DataFrom != nil {
		t.Fatalf("raw-covered 7d window = up %d / total %d data_from %v, want up %d / total %d and complete",
			c.Up, c.Total, c.DataFrom, wantUp, wantTotal)
	}
}

// An old monitor whose raw rows were purged has rollup rows older than the window: complete.
func TestLongWindowWithOlderRollupIsComplete(t *testing.T) {
	f := newLongWindowFixture(t)
	mon := f.monitor(t, "billing")
	total, up := f.beats(t, mon, -120, 0, 4, -100, 1)
	f.purge(t, 30)

	c, err := f.st.MonitorSLI(f.ctx, mon, time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	if c.DataFrom != nil {
		t.Errorf("a monitor with data older than the window reported data_from=%v", c.DataFrom)
	}
	// Days -89 … -1 plus today (no beats today): the partial first day (-90) is not counted.
	var wantTotal int64 = 89 * 4
	if c.Total != wantTotal {
		t.Errorf("90d total = %d, want %d (89 whole UTC days × 4; the partial first day is excluded); history had %d/%d",
			c.Total, wantTotal, up, total)
	}
}

// A project sums its monitors, and is complete when any of them has data older than the window.
func TestLongWindowProjectSumsMonitorsAndCoverage(t *testing.T) {
	f := newLongWindowFixture(t)
	old := f.monitor(t, "old")
	young := f.monitor(t, "young")
	oldTotal, oldUp := f.beats(t, old, -60, 0, 24, -45, 2)
	youngTotal, youngUp := f.beats(t, young, -10, 0, 24, -3, 1)
	f.purge(t, 30)

	c, err := f.st.ProjectSLI(f.ctx, f.project, time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("project sli: %v", err)
	}
	if c.Total != oldTotal+youngTotal || c.Up != oldUp+youngUp {
		t.Fatalf("project 90d = up %d / total %d, want up %d / total %d",
			c.Up, c.Total, oldUp+youngUp, oldTotal+youngTotal)
	}
	if !sameDay(c.DataFrom, f.today.AddDate(0, 0, -60)) {
		t.Errorf("project data_from = %v, want the oldest monitor's first day", c.DataFrom)
	}
}

// The public status page's 90-day uptime of a monitor-backed component reads the same computation
// and states uptime_since for a young monitor.
func TestLongWindowStatusPageProjection(t *testing.T) {
	f := newLongWindowFixture(t)
	old := f.monitor(t, "old")
	young := f.monitor(t, "young")
	oldTotal, oldUp := f.beats(t, old, -60, 0, 24, -45, 12)
	f.beats(t, young, -10, 0, 24, -3, 1)
	f.purge(t, 30)

	got, err := f.st.MonitorPageProjections(f.ctx, []string{old, young}, time.Now().Add(-90*24*time.Hour), true)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	want := float64(oldUp) / float64(oldTotal) * 100
	if p := got[old]; p.Uptime == nil || *p.Uptime != want {
		t.Errorf("old monitor uptime = %v, want %.6f (rollup days count)", p.Uptime, want)
	}
	if p := got[young]; !sameDay(p.UptimeSince, f.today.AddDate(0, 0, -10)) {
		t.Errorf("young monitor uptime_since = %v, want %s", p.UptimeSince, f.today.AddDate(0, 0, -10).Format("2006-01-02"))
	}
}

func (f longWindowFixture) beatAt(t *testing.T, monitorID string, ts time.Time, up bool) {
	t.Helper()
	if _, err := f.st.pool.Exec(f.ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg) VALUES ($1,$2,$3,10,200,'')`,
		monitorID, ts, up); err != nil {
		t.Fatalf("insert heartbeat: %v", err)
	}
}

// The first retained heartbeat is not a retention boundary. A young monitor whose yesterday has not
// been rolled up yet (a read after UTC midnight, before the next rollup pass) still has that raw
// row, and it must count (iter-0199 review, Important 1).
func TestLongWindowCountsRawDaysTheRollupHasNotReached(t *testing.T) {
	f := newLongWindowFixture(t)
	mon := f.monitor(t, "young")
	f.beatAt(t, mon, f.today.Add(-12*time.Hour), false) // yesterday noon, not rolled up
	f.beatAt(t, mon, time.Now().Add(-time.Minute), true)

	since := time.Now().Add(-90 * 24 * time.Hour)
	c, err := f.st.MonitorSLI(f.ctx, mon, since)
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	if c.Total != 2 || c.Up != 1 {
		t.Fatalf("90d = up %d / total %d, want 1/2: yesterday's raw DOWN exists and must count", c.Up, c.Total)
	}
	if !sameDay(c.DataFrom, f.today.AddDate(0, 0, -1)) {
		t.Errorf("data_from = %v, want yesterday", c.DataFrom)
	}
	got, err := f.st.MonitorPageProjections(f.ctx, []string{mon}, since, true)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if p := got[mon]; p.Uptime == nil || *p.Uptime != 50 {
		t.Errorf("status page uptime = %v, want 50", p.Uptime)
	}
}

// A rollup row for the window's FIRST day proves nothing about the time before the window start:
// a monitor whose first heartbeat came after S on S's own UTC day is still younger than the window
// (iter-0199 review, Important 2). Its first-day heartbeat must count, too.
func TestLongWindowRollupOfTheStartDayIsNotEvidenceOfOlderData(t *testing.T) {
	f := newLongWindowFixture(t)
	mon := f.monitor(t, "created-after-start")
	since := time.Now().Add(-90 * 24 * time.Hour)
	first := since.Add(time.Minute)
	if !utcDay(first).Equal(utcDay(since)) {
		t.Skip("the window start is within a minute of UTC midnight; rerun")
	}
	f.beatAt(t, mon, first, false)
	f.beatAt(t, mon, time.Now().Add(-time.Minute), true)
	if err := f.st.RollupDailyAvailability(f.ctx, f.today.AddDate(0, 0, -200), f.today); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	c, err := f.st.MonitorSLI(f.ctx, mon, since)
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	if c.DataFrom == nil {
		t.Errorf("a monitor first seen after the window start was reported complete")
	}
	if c.Total != 2 {
		t.Errorf("90d total = %d, want 2: the first-day heartbeat is inside the window", c.Total)
	}
	got, err := f.st.MonitorPageProjections(f.ctx, []string{mon}, since, true)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if got[mon].UptimeSince == nil {
		t.Errorf("status page projection reported the window complete")
	}
}

// Monitor burn rules have long windows up to 7 days while raw retention can be 2: the long window
// must count the rollup days too, or a burn the full window never crossed pages someone
// (iter-0199 review, Important 3).
func TestMonitorBurnLongWindowCountsRollupDays(t *testing.T) {
	f := newLongWindowFixture(t)
	mon := f.monitor(t, "burn")
	for d := -6; d <= -3; d++ { // four whole days, all good — only in the rollup after the purge
		for i := 0; i < 96; i++ {
			f.beatAt(t, mon, f.today.AddDate(0, 0, d).Add(time.Duration(i)*time.Minute), true)
		}
	}
	for d := -2; d <= -1; d++ { // retained raw days, all good
		for i := 0; i < 15; i++ {
			f.beatAt(t, mon, f.today.AddDate(0, 0, d).Add(12*time.Hour+time.Duration(i)*time.Minute), true)
		}
	}
	for i := 0; i < 10; i++ { // the last ~two minutes: three failures — the short window burns
		f.beatAt(t, mon, time.Now().Add(-time.Duration(i+1)*10*time.Second), i >= 3)
	}
	f.purge(t, 2)

	// Raw only: 3 bad / 40 = 7.5 % → 7.5× on a 99 % objective. Full 7d: 3 / 424 ≈ 0.7×.
	rules := []domain.BurnRule{{LongWindowSeconds: 7 * 24 * 3600, ShortWindowSeconds: 300, Threshold: 5, Severity: domain.BurnSeverityPage}}
	if _, err := f.st.UpsertMonitorSLATarget(f.ctx, mon, "30d", 99, true, rules); err != nil {
		t.Fatalf("burn target: %v", err)
	}
	fired, _, err := f.st.EvaluateBurnAlerts(f.ctx)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if fired != 0 {
		t.Fatalf("burn fired %d time(s): the full 7-day window burns at ~0.7×, below the 5× threshold", fired)
	}
}
