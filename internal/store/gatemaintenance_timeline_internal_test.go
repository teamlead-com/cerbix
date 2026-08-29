package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// D10 — the ONE timeline of a pass, measured from passStart: creation [0, 12 s), removal
// [12 s, 27 s), release proof [27 s, 30 s]. Slices are enforced PER STATEMENT as a wall bound.
// These tests use the real clock where a statement must really block and an offset or fake
// clock where only the position in the timeline matters.

// stageLog records every hook invocation with its wall time and the session's bounds.
type stageLog struct {
	mu      sync.Mutex
	entries []stageEntry
}

type stageEntry struct {
	stage string
	at    time.Time
	st    time.Duration // statement_timeout in force, when read
	lt    time.Duration // lock_timeout in force, when read
}

func (l *stageLog) add(e stageEntry) {
	l.mu.Lock()
	l.entries = append(l.entries, e)
	l.mu.Unlock()
}

func (l *stageLog) snapshot() []stageEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]stageEntry(nil), l.entries...)
}

func (l *stageLog) first(prefix string) (stageEntry, bool) {
	for _, e := range l.snapshot() {
		if strings.HasPrefix(e.stage, prefix) {
			return e, true
		}
	}
	return stageEntry{}, false
}

func (l *stageLog) last(prefix string) (stageEntry, bool) {
	var out stageEntry
	found := false
	for _, e := range l.snapshot() {
		if strings.HasPrefix(e.stage, prefix) {
			out, found = e, true
		}
	}
	return out, found
}

// showBounds reads the bounds the pass set for the statement about to run, on the pass's own
// connection or transaction — the mechanism, not the code's arithmetic.
func showBounds(ctx context.Context, conn dbConn) (st, lt time.Duration, err error) {
	var s, l string
	if err := conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&s); err != nil {
		return 0, 0, err
	}
	if err := conn.QueryRow(ctx, `SHOW lock_timeout`).Scan(&l); err != nil {
		return 0, 0, err
	}
	return parsePgDuration(s), parsePgDuration(l), nil
}

// parsePgDuration parses SHOW's "10s" / "1500ms" / "0" spellings.
func parsePgDuration(s string) time.Duration {
	if s == "0" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return -1
	}
	return d
}

// recordingHooks logs every stage with the bounds in force.
func recordingHooks(log *stageLog) *gateMaintenanceHooks {
	return &gateMaintenanceHooks{beforeStatement: func(ctx context.Context, stage string, conn dbConn) error {
		st, lt, err := showBounds(ctx, conn)
		if err != nil {
			return err
		}
		log.add(stageEntry{stage: stage, at: time.Now(), st: st, lt: lt})
		return nil
	}}
}

// §7 — create_max = 8 with EVERY attach blocked behind a held lock, including the one started
// immediately before the 12 s boundary: the last creation statement is refused by its CLAMPED
// timeout at the boundary (not a full 2 s later), the first removal statement begins by t = 12 s
// (+ one tick of slack), and an eligible detach completes in the same pass once the lock is gone.
func TestGateMaintCreationEndsAtTheBoundaryAndRemovalBeginsByIt(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	for i := 1; i <= 7; i++ {
		dropGateDay(t, st, ctx, today.AddDate(0, 0, i))
	}
	eligible := today.AddDate(0, 0, -9)
	plantGateDay(t, st, ctx, eligible, "attached", nil)
	// SHARE UPDATE EXCLUSIVE on the parent conflicts with ATTACH (and DETACH) but not with the
	// standalone CREATE … LIKE (ACCESS SHARE) nor with decision inserts (ROW EXCLUSIVE).
	release := holdLock(t, st, ctx, "SHARE UPDATE EXCLUSIVE", "service_gate_decisions")

	log := &stageLog{}
	hooks := recordingHooks(log)
	cfg := gateMaintCfg
	cfg.CreateMax = 8
	m := &gateMetricsFake{}
	passStart := time.Now()
	go func() {
		time.Sleep(gateCreationSlice - time.Since(passStart))
		release() // the blocker leaves exactly at the boundary
	}()
	rep, _, err := st.runGateLedgerMaintenancePass(ctx, passStart, cfg, time.Now, m, hooks)
	ended := time.Since(passStart)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if ended > gateWorkDeadline+gateReleaseBudget {
		t.Fatalf("pass lifecycle took %s", ended)
	}
	if m.get("lock_timeout") < 3 {
		t.Fatalf("only %d attaches were refused by lock_timeout: %v", m.get("lock_timeout"), rep.Failures)
	}
	lastAttach, ok := log.last("attach")
	if !ok {
		t.Fatal("no attach ran")
	}
	if at := lastAttach.at.Sub(passStart); at >= gateCreationSlice {
		t.Fatalf("an attach STARTED at %s, past the creation boundary", at)
	}
	// Its bounds were clamped to the remainder: statement_timeout = remaining (< 2 s here), and
	// lock_timeout = min(2 s, that) — so the refusal landed at the boundary, not 2 s after start.
	remainingAtStart := gateCreationSlice - lastAttach.at.Sub(passStart)
	if lastAttach.lt > remainingAtStart+150*time.Millisecond || lastAttach.st > remainingAtStart+150*time.Millisecond {
		t.Fatalf("last attach started with %s left had statement_timeout %s / lock_timeout %s: not clamped to the slice",
			remainingAtStart, lastAttach.st, lastAttach.lt)
	}
	if lastAttach.lt >= gateLockTimeoutMax {
		t.Fatalf("the last attach ran with an unclamped lock_timeout %s at %s before the boundary", lastAttach.lt, remainingAtStart)
	}
	// The first removal-phase statement — the eligible day's inspection — began by 12 s + tick.
	removalStart, ok := log.first("survey")
	entries := log.snapshot()
	for _, e := range entries {
		if e.stage == "survey" && e.at.Sub(passStart) > 500*time.Millisecond { // the preamble survey is at ~0
			removalStart, ok = e, true
			break
		}
	}
	if !ok {
		t.Fatal("no removal-phase statement ran")
	}
	if at := removalStart.at.Sub(passStart); at > gateCreationSlice+time.Second {
		t.Fatalf("the first removal statement began at %s, want by 12 s (+ one tick)", at)
	}
	if rep.Detached != 1 {
		t.Fatalf("the eligible detach did not complete in the same pass: detached=%d failures=%v", rep.Detached, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, eligible); c.attached || c.state != "detached" {
		t.Fatalf("eligible day: %+v", c)
	}
	// Creation reached days under the blocker: at least most attaches were refused (a single one
	// may slip through as the blocker releases at the boundary). The next pass converges the whole
	// horizon with no refusals.
	if rep.Created == 0 {
		t.Fatalf("no day was created under the blocker: %+v", rep)
	}
	if _, _, err := runGatePass(t, st, ctx, cfg, nil, m, nil); err != nil {
		t.Fatalf("converging pass: %v", err)
	}
	for i := 1; i <= 7; i++ {
		if c := gateLookup(t, st, ctx, today.AddDate(0, 0, i)); !c.attached || c.state != "attached" {
			t.Fatalf("today+%d not attached after convergence: %+v", i, c)
		}
	}
	gateRegistryAgreesWithCatalog(t, st, ctx)
}

// §7 — a whole create transaction completes with a fake clock advanced 2 s between EVERY
// statement; each statement's statement_timeout equals the remaining slice (capped at 10 s) and
// its lock_timeout = min(2 s, that). The mutation that admits by the SUM of the two timers refuses
// the second statement (remaining 10 s < 10 s + 2 s) and fails this test.
func TestGateMaintCreateTransactionBoundsTrackTheRemainingSlice(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	dropGateDay(t, st, ctx, today.AddDate(0, 0, 7))
	clock := newFakeClock(time.Now())
	passStart := clock.Now()
	log := &stageLog{}
	hooks := &gateMaintenanceHooks{beforeStatement: func(ctx context.Context, stage string, conn dbConn) error {
		if !strings.HasPrefix(stage, "create") || stage == "create.probe" {
			return nil
		}
		st, lt, err := showBounds(ctx, conn)
		if err != nil {
			return err
		}
		log.add(stageEntry{stage: stage, at: clock.Now(), st: st, lt: lt})
		clock.Advance(2 * time.Second) // the clock moves between statements, never during one
		return nil
	}}
	cfg := gateMaintCfg
	cfg.CreateMax = 1
	rep, _, err := st.runGateLedgerMaintenancePass(ctx, passStart, cfg, clock.Now, &gateMetricsFake{}, hooks)
	if err != nil || rep.Created != 1 || rep.Attached != 1 {
		t.Fatalf("created=%d attached=%d err=%v failures=%v", rep.Created, rep.Attached, err, rep.Failures)
	}
	want := []struct {
		stage string
		st    time.Duration
	}{
		{"create", 10 * time.Second},        // remaining 12 → capped at 10
		{"create.index", 10 * time.Second},  // remaining 10
		{"create.insert", 8 * time.Second},  // remaining 8
		{"create.comment", 6 * time.Second}, // remaining 6
		{"create.commit", 4 * time.Second},  // remaining 4
	}
	got := log.snapshot()
	if len(got) != len(want) {
		t.Fatalf("create transaction ran %d bounded statements, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.stage != w.stage {
			t.Fatalf("statement %d is %q, want %q", i, g.stage, w.stage)
		}
		if g.st != w.st {
			t.Errorf("%s: statement_timeout %s, want %s (the remaining slice, capped at 10 s)", w.stage, g.st, w.st)
		}
		if g.lt != gateLockTimeoutMax {
			t.Errorf("%s: lock_timeout %s, want min(2 s, statement_timeout) = 2 s", w.stage, g.lt)
		}
	}
	if c := gateLookup(t, st, ctx, today.AddDate(0, 0, 7)); !c.attached {
		t.Fatalf("today+7 after the pass: %+v", c)
	}
}

// §7 — a statement blocked AT the boundary is bounded by the slice: its transaction is rolled back
// by t = 12 s (server bound first, the context net one tolerance behind), the day stays
// `created` standalone, and the pass's connection stays live for the removal that follows.
func TestGateMaintStatementBlockedAtTheBoundaryIsRolledBackBy12s(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	dropGateDay(t, st, ctx, today.AddDate(0, 0, 7))
	eligible := today.AddDate(0, 0, -9)
	plantGateDay(t, st, ctx, eligible, "attached", nil)
	// passStart 10.5 s ago: the creation slice has 1.5 s left; the attach blocks behind SUE on
	// the parent, released exactly at the boundary so removal can run on the live connection.
	clock := offsetClock(10500 * time.Millisecond)
	release := holdLock(t, st, ctx, "SHARE UPDATE EXCLUSIVE", "service_gate_decisions")
	passStart := clock().Add(-10500 * time.Millisecond)
	attachAt := make(chan time.Time, 1)
	hooks := &gateMaintenanceHooks{beforeStatement: func(_ context.Context, stage string, _ dbConn) error {
		if stage == "attach" {
			select {
			case attachAt <- time.Now():
			default:
			}
		}
		return nil
	}}
	m := &gateMetricsFake{}
	done := make(chan struct{})
	var rep GateMaintenanceReport
	var perr error
	go func() {
		defer close(done)
		rep, _, perr = st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock, m, hooks)
	}()
	select {
	case <-attachAt:
	case <-time.After(5 * time.Second):
		t.Fatal("the attach never started")
	}
	// By the boundary (+ net + scheduling) the attach's transaction is gone.
	waitFor(t, 5*time.Second, "the blocked ATTACH to be rolled back", func() bool {
		var inflight int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND query ILIKE '%ATTACH PARTITION%' AND state <> 'idle'
			   AND pid <> pg_backend_pid()`).Scan(&inflight); err != nil {
			t.Fatal(err)
		}
		return inflight == 0
	})
	if c := gateLookup(t, st, ctx, today.AddDate(0, 0, 7)); c.attached || c.state != "created" {
		t.Fatalf("the blocked attach's day: %+v, want standalone + created (rolled back)", c)
	}
	release() // removal, on the live connection, now completes the detach
	<-done
	if perr != nil {
		t.Fatalf("pass: %v", perr)
	}
	if rep.Attached != 0 || m.get("lock_timeout")+m.get("statement_timeout") < 1 {
		t.Fatalf("the boundary refusal: counts=%v attached=%d failures=%v", m.counts, rep.Attached, rep.Failures)
	}
	if rep.Detached != 1 {
		t.Fatalf("removal did not proceed on the live connection after the boundary: detached=%d failures=%v", rep.Detached, rep.Failures)
	}
}

// §7 — the client context deadline is the net behind the server bound: a statement whose issue is
// delayed past the boundary (the hook holds it) is refused by the transaction's context deadline,
// its transaction rolled back, and the pass continues on a live connection.
func TestGateMaintContextNetCancelsAStatementIssuedPastTheBoundary(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	dropGateDay(t, st, ctx, today.AddDate(0, 0, 7))
	eligible := today.AddDate(0, 0, -9)
	plantGateDay(t, st, ctx, eligible, "attached", nil)
	clock := offsetClock(11 * time.Second) // 1 s of creation slice left
	passStart := clock()
	hooks := &gateMaintenanceHooks{beforeStatement: func(ctx context.Context, stage string, _ dbConn) error {
		if stage == "attach" {
			<-ctx.Done() // hold the statement until the transaction's own deadline has passed
		}
		return nil
	}}
	m := &gateMetricsFake{}
	rep, _, err := st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock, m, hooks)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if rep.Attached != 0 || m.get("error") != 1 {
		t.Fatalf("attached=%d counts=%v failures=%v; want the attach refused by its context deadline", rep.Attached, m.counts, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, today.AddDate(0, 0, 7)); c.attached || c.state != "created" {
		t.Fatalf("day after the cancelled attach: %+v", c)
	}
	if rep.Detached != 1 {
		t.Fatalf("the pass did not go on to removal on its live connection: %+v", rep)
	}
}

// §7 — with fewer than 500 ms of slice left no statement is started: creation issues nothing at
// t = 11.6 s (removal still runs), and at t = 26.6 s the pass issues no work statement at all.
func TestGateMaintNoStatementStartsWithLessThan500ms(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	dropGateDay(t, st, ctx, today.AddDate(0, 0, 7))
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)

	log := &stageLog{}
	clock := newFakeClock(time.Now())
	passStart := clock.Now()
	clock.Advance(11600 * time.Millisecond)
	rep, _, err := st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock.Now, &gateMetricsFake{}, recordingHooks(log))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range log.snapshot() {
		if strings.HasPrefix(e.stage, "create") || strings.HasPrefix(e.stage, "attach") {
			t.Fatalf("a creation statement (%s) started with 400 ms of slice left", e.stage)
		}
	}
	if rep.CreationSkipped || rep.Created != 0 || rep.Detached != 1 {
		t.Fatalf("at 11.6 s: %+v (creation open but nothing startable; removal runs)", rep)
	}

	resetGateLedger(t, st, ctx)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)
	log = &stageLog{}
	clock = newFakeClock(time.Now())
	passStart = clock.Now()
	clock.Advance(26600 * time.Millisecond)
	rep, acquired, err := st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock.Now, &gateMetricsFake{}, recordingHooks(log))
	if err != nil || !acquired {
		t.Fatalf("late pass: acquired=%v err=%v", acquired, err)
	}
	if n := len(log.snapshot()); n != 0 {
		t.Fatalf("%d work statements started with 400 ms before the work deadline", n)
	}
	if !rep.CreationSkipped || !rep.RemovalSkipped || rep.GaugesValid {
		t.Fatalf("at 26.6 s: %+v, want cleanup only", rep)
	}
	if c := gateLookup(t, st, ctx, today.AddDate(0, 0, -9)); !c.attached {
		t.Fatal("removal acted with no slice left")
	}
	waitGateLockFree(t, st, ctx)
}

// §7 — pool.Acquire delayed to t = 13 s: creation is skipped, the first removal statement begins
// immediately, cleanup ends by passStart + 30 s. Delayed to t = 26.8 s: no work statement, cleanup
// by 30 s. Real time under an offset clock, so the deadlines are measured, not simulated.
func TestGateMaintLateAcquisitionSkipsCreationAndStillEndsBy30s(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	dropGateDay(t, st, ctx, today.AddDate(0, 0, 7))
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)

	// passStart is 13 s "ago" on the offset clock; the acquisition itself is immediate.
	clock := offsetClock(13 * time.Second)
	passStart := clock().Add(-13 * time.Second)
	t0 := time.Now()
	log := &stageLog{}
	rep, acquired, err := st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock, &gateMetricsFake{}, recordingHooks(log))
	ended := clock()
	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	if !rep.CreationSkipped || rep.Created != 0 {
		t.Fatalf("creation ran at t = 13 s: %+v", rep)
	}
	first, ok := log.first("rebind")
	if !ok || first.at.Sub(t0) > time.Second {
		t.Fatalf("the first removal-phase statement did not begin immediately: %+v (t0 %s)", first, t0)
	}
	if rep.Detached != 1 {
		t.Fatalf("removal did not run: %+v", rep)
	}
	if ended.Sub(passStart) > GatePassLifecycle {
		t.Fatalf("cleanup ended %s after passStart", ended.Sub(passStart))
	}

	resetGateLedger(t, st, ctx)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)
	clock = offsetClock(26800 * time.Millisecond)
	passStart = clock().Add(-26800 * time.Millisecond)
	log = &stageLog{}
	rep, acquired, err = st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock, &gateMetricsFake{}, recordingHooks(log))
	ended = clock()
	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	if len(log.snapshot()) != 0 || !rep.RemovalSkipped {
		t.Fatalf("work statements at t = 26.8 s: %+v / %+v", log.snapshot(), rep)
	}
	if ended.Sub(passStart) > GatePassLifecycle {
		t.Fatalf("cleanup ended %s after passStart", ended.Sub(passStart))
	}
	waitGateLockFree(t, st, ctx)
}

// §7 — a pass whose removal spends the FULL [12 s, 27 s) (many eligible detaches, each refused by
// lock_timeout behind a SHARE UPDATE EXCLUSIVE holder on the parent — the lock DETACH …
// CONCURRENTLY itself needs) and whose RESET blackholes measures ≤ 30 s from passStart to the end
// of cleanup — the declared total, not the cleanup alone.
func TestGateMaintFullRemovalSliceAndResetBlackholeFitThirtySeconds(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	for i := 0; i < 8; i++ {
		plantGateDay(t, st, ctx, today.AddDate(0, 0, -30+i), "attached", nil)
	}
	// SUE on the parent lets each shape read (ACCESS SHARE on a child) through but blocks the
	// DETACH … CONCURRENTLY, so every eligible day burns a full lock_timeout at its detach.
	holdLock(t, st, ctx, "SHARE UPDATE EXCLUSIVE", "service_gate_decisions")
	var pid int
	var pidOnce sync.Once
	hooks := &gateMaintenanceHooks{
		beforeStatement: func(ctx context.Context, stage string, conn dbConn) error {
			var err error
			pidOnce.Do(func() { pid, err = backendPidOf(ctx, conn) })
			return err
		},
		beforeRelease: func(ctx context.Context, _ *pgxpool.Conn) error {
			<-ctx.Done() // the RESET never answers: the hook sits on the release context until it expires
			return ctx.Err()
		},
	}
	cfg := gateMaintCfg
	cfg.PurgeMaxPartitions = 8
	m := &gateMetricsFake{}
	// The offset clock makes the pass BELIEVE it is 11 s in, so the removal slice it sees is the
	// tight [12 s, 27 s) worth ~16 s of clock budget — filled by eight 2 s lock_timeout refusals,
	// which is how removal reaches the 27 s boundary rather than exhausting its days early. Real
	// wall time is what the ≤ 30 s bound is measured in.
	clock := offsetClock(11 * time.Second)
	passStart := clock().Add(-11 * time.Second)
	start := time.Now()
	rep, acquired, err := st.runGateLedgerMaintenancePass(ctx, passStart, cfg, clock, m, hooks)
	total := time.Since(start)
	if !acquired {
		t.Fatal("not acquired")
	}
	if total > GatePassLifecycle+300*time.Millisecond {
		t.Fatalf("lifecycle measured %s from passStart to cleanup end, want ≤ 30 s", total)
	}
	if err == nil || !strings.Contains(err.Error(), "release unproven") {
		t.Fatalf("a blackholed RESET must end in an unproven release: %v", err)
	}
	// Six or more 2 s refusals is ~12 s of the 15 s removal slice: removal spent the slice on
	// refusals rather than finishing its days, which is the point of the timeline.
	if m.get("lock_timeout") < 6 {
		t.Fatalf("only %d lock_timeout refusals in the removal slice: %v", m.get("lock_timeout"), rep.Failures)
	}
	waitFor(t, 5*time.Second, "the poisoned backend to be gone", func() bool { return !backendAlive(t, st, ctx, pid) })
	waitGateLockFree(t, st, ctx)
}

// pgCounts is a small helper for messages.
func pgCounts(m *gateMetricsFake) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprint(m.counts)
}
