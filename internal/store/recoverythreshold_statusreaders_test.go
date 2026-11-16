package store_test

// FR-043 — the readers of `status` that the recovery threshold reaches without reading the new
// columns (func-recovery-threshold §4.4, NFR-036 statement (c), revision 6). These tests PIN the
// described behaviour so that it cannot change unnoticed; neither is a defect to be fixed:
//
//   - the confirm phase requires `status = up` (Monitor.InConfirmPhase), so a monitor the threshold
//     holds in `down` gets no accelerated check where a threshold-1 monitor, already `up` again,
//     does — the "recovered and failed again" case, not only "during a run";
//   - a composite parent computes its own check from its children's statuses, so a child mid-run
//     counts as not-up and the parent's heartbeats depend on the child's threshold. NFR-036 is about
//     a monitor's OWN checks.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/prober"
	"github.com/teamlead-com/cerbix/internal/scheduler"
	"github.com/teamlead-com/cerbix/internal/store"
)

// statusReaderMonitor creates a TCP monitor and returns a function that records one fresh verdict
// for it through RecordScheduledResult, with strictly increasing timestamps and the current
// revision, and returns the status it left.
func statusReaderMonitor(t *testing.T, ctx context.Context, st *store.Store, m domain.Monitor) (string, func(up bool) domain.MonitorStatus) {
	t.Helper()
	created, err := st.CreateMonitor(ctx, m)
	if err != nil {
		t.Fatalf("create %s: %v", m.Name, err)
	}
	next := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	return created.ID, func(up bool) domain.MonitorStatus {
		t.Helper()
		cur, err := st.GetMonitor(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		next = next.Add(time.Second)
		o, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
			MonitorID: created.ID, Ts: next, Up: up, ExecutionRevision: cur.ExecutionRevision,
		})
		if err != nil || !o.Applied {
			t.Fatalf("record %s up=%v: %+v %v", m.Name, up, o, err)
		}
		return o.Cur
	}
}

// NFR-036 (c), §4.4 — with the confirm phase on, the scenario "down, one success, failure again"
// leaves the threshold-1 monitor `up` mid-confirmation and the threshold-3 monitor `down`; the leader
// then schedules an accelerated check for the first and none for the second. The number of checks,
// and with it the SLA, may therefore differ between the two for the same results. D4 rules out the
// alternative of accelerating the way up.
func TestRecoveryThresholdHoldsTheConfirmPhaseOffWhileDown(t *testing.T) {
	st, ctx := testStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, threshold int) (string, func(bool) domain.MonitorStatus) {
		// An hour's interval: within the test window a second job can only be the confirm
		// phase's. failure_threshold 2 is what gives the confirm phase anything to confirm.
		id, feed := statusReaderMonitor(t, ctx, st, domain.Monitor{
			RecoveryThreshold: threshold,
			ProjectID:         proj.ID, Name: name, Type: domain.MonitorTCP, Target: "10.0.0.1:80",
			IntervalSeconds: 3600, TimeoutSeconds: 5, FailureThreshold: 2, ConfirmIntervalSeconds: 5,
			Enabled: true,
		})
		// One second rather than the form's minimum of five keeps the test short; the store
		// writes the value as given and the scheduler reads it the same way.
		conn, err := pgx.Connect(ctx, os.Getenv("CERBIX_TEST_DATABASE_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, `UPDATE monitors SET confirm_interval_seconds = 1 WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		return id, feed
	}
	id1, feed1 := mk("t1", 1)
	id3, feed3 := mk("t3", 3)

	// down (two failures), one success, a failure again.
	script := []bool{false, false, true, false}
	want1 := []domain.MonitorStatus{"pending", "down", "up", "up"}
	want3 := []domain.MonitorStatus{"pending", "down", "down", "down"}
	for i, up := range script {
		if got := feed1(up); got != want1[i] {
			t.Fatalf("threshold 1, result %d: %s, want %s", i, got, want1[i])
		}
		if got := feed3(up); got != want3[i] {
			t.Fatalf("threshold 3, result %d: %s, want %s", i, got, want3[i])
		}
	}
	m1, err := st.GetMonitor(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	m3, err := st.GetMonitor(ctx, id3)
	if err != nil {
		t.Fatal(err)
	}
	if !m1.InConfirmPhase() {
		t.Fatalf("threshold 1 after down/success/failure: %+v — want up mid-confirmation", m1)
	}
	if m3.InConfirmPhase() || m3.Status != domain.StatusDown || m3.ConsecutiveSuccesses != 0 {
		t.Fatalf("threshold 3 after down/success/failure: status %s run %d confirm=%v — want down, no run, no confirm phase",
			m3.Status, m3.ConsecutiveSuccesses, m3.InConfirmPhase())
	}

	// The real leader over the real store, from this state: its first snapshot puts the monitor
	// that is mid-confirmation on the accelerated rhythm (the fallback path of a missed signal).
	inner := dispatch.NewInProc(64)
	var mu sync.Mutex
	jobs := map[string]int{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sch := scheduler.New(scheduler.NewStoreAdapter(st), inner, logger)
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	leaderDone := make(chan struct{})
	go func() { defer close(leaderDone); sch.Run(runCtx) }()
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case d := <-inner.Jobs():
				mu.Lock()
				jobs[d.Job.Monitor.ID]++
				mu.Unlock()
			}
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n1 := jobs[id1]
		mu.Unlock()
		if n1 >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-leaderDone
	<-drainDone

	mu.Lock()
	defer mu.Unlock()
	if jobs[id1] < 3 {
		t.Fatalf("threshold 1 (up, mid-confirmation): %d jobs in the window, want the confirm phase's accelerated checks (≥ 3)", jobs[id1])
	}
	if jobs[id3] > 1 {
		t.Fatalf("threshold 3 (held down by the threshold): %d jobs in the window, want at most its one ordinary check — "+
			"a down monitor gets no accelerated check (InConfirmPhase requires up; D4)", jobs[id3])
	}
}

// §4.4 — a composite parent reads its children's statuses: a child mid-run is not up for `all`,
// `any` and `quorum`, so the parent's own check — and its heartbeats — depend on the child's
// threshold. The same results at threshold 1 leave the child up and the parent up.
func TestRecoveryRunChildCountsAsNotUpForACompositeParent(t *testing.T) {
	st, ctx := testStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatal(err)
	}
	child := func(name string, threshold int) (string, func(bool) domain.MonitorStatus) {
		return statusReaderMonitor(t, ctx, st, domain.Monitor{
			RecoveryThreshold: threshold,
			ProjectID:         proj.ID, Name: name, Type: domain.MonitorTCP, Target: "10.0.0.1:80",
			IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
		})
	}
	running, feedRunning := child("running", 3)
	quick, feedQuick := child("quick", 1)
	healthy, feedHealthy := child("healthy", 1)
	for _, up := range []bool{false, true} { // down, then the first success of a run
		feedRunning(up)
		feedQuick(up)
	}
	feedHealthy(true)

	if s, err := st.MonitorStatuses(ctx, []string{running, quick, healthy}); err != nil ||
		s[running] != domain.StatusDown || s[quick] != domain.StatusUp || s[healthy] != domain.StatusUp {
		t.Fatalf("child statuses = %v (err %v), want running down mid-run, quick up, healthy up", s, err)
	}
	m, err := st.GetMonitor(ctx, running)
	if err != nil || m.ConsecutiveSuccesses != 1 {
		t.Fatalf("running child: run %d (err %v), want 1", m.ConsecutiveSuccesses, err)
	}

	runner := prober.NewRunner().WithChildStatus(st.MonitorStatuses)
	parent := func(children, mode, quorum string) domain.Monitor {
		cfg := map[string]string{"children": children, "mode": mode}
		if quorum != "" {
			cfg["quorum"] = quorum
		}
		return domain.Monitor{ID: "parent", ProjectID: proj.ID, Name: "parent", Type: domain.MonitorComposite,
			IntervalSeconds: 60, TimeoutSeconds: 5, RecoveryThreshold: 1, Config: cfg}
	}
	for _, c := range []struct {
		name               string
		children, mode, qu string
		wantUp             bool
	}{
		{"all over a child mid-run", running + "," + healthy, "all", "", false},
		{"any over a child mid-run alone", running, "any", "", false},
		// quorum M: the parent is down once M children are not up (D-0101).
		{"quorum 1 over a child mid-run and a healthy one", running + "," + healthy, "quorum", "1", false},
		{"the same results at threshold 1: all", quick + "," + healthy, "all", "", true},
		{"the same results at threshold 1: quorum 1", quick + "," + healthy, "quorum", "1", true},
	} {
		hb := runner.Run(ctx, parent(c.children, c.mode, c.qu))
		// A parent that could not read its children is down too; that must not pass for the run.
		if strings.Contains(hb.Msg, "monitor statuses") {
			t.Fatalf("%s: the parent did not read its children: %s", c.name, hb.Msg)
		}
		if hb.Up != c.wantUp {
			t.Errorf("%s: parent up=%v (%s), want %v", c.name, hb.Up, hb.Msg, c.wantUp)
		}
	}
}
