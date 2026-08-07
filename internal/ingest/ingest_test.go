package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

type fakeStore struct {
	mu          sync.Mutex
	hbs         []domain.Heartbeat
	statuses    map[string]domain.MonitorStatus
	monitors    map[string]domain.Monitor
	incidents   map[string]domain.Incident
	updates     map[string][]domain.IncidentUpdate
	consecutive map[string]int       // live failure counter (confirmations)
	maint       map[string]bool      // monitor id → currently in a maintenance window
	lastTs      map[string]time.Time // freshness watermark (last applied result ts)
	seq         int64                // monotonic synthetic clock for zero-Ts heartbeats
	forceReason string               // if set, RecordScheduledResult returns this non-applied outcome
	nextInc     int
	// createErrs is a queue of errors CreateIncident returns on successive calls
	// (nil = success); createCalls counts how many times it was invoked.
	createErrs  []error
	createCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		statuses:    map[string]domain.MonitorStatus{},
		monitors:    map[string]domain.Monitor{},
		incidents:   map[string]domain.Incident{},
		updates:     map[string][]domain.IncidentUpdate{},
		consecutive: map[string]int{},
		maint:       map[string]bool{},
		lastTs:      map[string]time.Time{},
	}
}

// RecordScheduledResult mirrors store.RecordScheduledResult enough for the handle-level
// orchestration tests: dedup + freshness before applying the status change. Zero-Ts
// heartbeats (the common test shape) get a monotonic synthetic timestamp so distinct
// probes stay distinct and forward-moving — these tests simulate valid scheduled results.
// forceReason lets a test drive a non-applied outcome (quarantine/reject/…) to verify the
// handle() metric mapping. The full timestamp/missing/bounds pipeline is contract-tested
// against the real DB in store.TestRecordScheduledResultPipeline.
func (f *fakeStore) RecordScheduledResult(_ context.Context, hb domain.Heartbeat) (store.ResultOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceReason != "" {
		return store.ResultOutcome{Reason: f.forceReason}, nil // not applied, not inserted
	}
	ts := hb.Ts
	if ts.IsZero() {
		f.seq++
		ts = time.Unix(0, f.seq)
	}
	for _, e := range f.hbs {
		if e.MonitorID == hb.MonitorID && e.Ts.Equal(ts) {
			return store.ResultOutcome{Reason: store.ReasonDuplicate}, nil // already recorded
		}
	}
	stored := hb
	stored.Ts = ts
	f.hbs = append(f.hbs, stored)
	if last, ok := f.lastTs[hb.MonitorID]; ok && !ts.After(last) {
		return store.ResultOutcome{Inserted: true, Reason: store.ReasonOutOfOrder}, nil // SLA-only
	}

	prev, ok := f.statuses[hb.MonitorID]
	if !ok {
		prev = domain.StatusPending
	}
	threshold := f.monitors[hb.MonitorID].FailureThreshold
	if threshold < 1 {
		threshold = 1
	}
	var cur domain.MonitorStatus
	if hb.Up {
		f.consecutive[hb.MonitorID] = 0
		cur = domain.StatusUp
	} else {
		f.consecutive[hb.MonitorID]++
		if f.consecutive[hb.MonitorID] >= threshold {
			cur = domain.StatusDown
		} else {
			cur = prev // not yet confirmed — hold the current status
		}
	}
	f.statuses[hb.MonitorID] = cur
	f.lastTs[hb.MonitorID] = ts
	suppressed := f.maint[hb.MonitorID]
	if prev == cur {
		suppressed = false // no transition
	}
	return store.ResultOutcome{Applied: true, Inserted: true, Prev: prev, Cur: cur, Suppressed: suppressed}, nil
}

func (f *fakeStore) GetMonitor(_ context.Context, id string) (domain.Monitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.monitors[id]
	if !ok {
		return domain.Monitor{}, store.ErrNotFound
	}
	return m, nil
}

func (f *fakeStore) FindOpenAutoIncidentByMonitor(_ context.Context, monitorID string) (domain.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, inc := range f.incidents {
		if inc.MonitorID == monitorID && inc.Source == domain.SourceAuto && inc.Status != domain.IncidentResolved {
			return inc, nil
		}
	}
	return domain.Incident{}, store.ErrNotFound
}

func (f *fakeStore) CreateIncident(_ context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return domain.Incident{}, err
		}
	}
	f.nextInc++
	inc.ID = "inc-" + string(rune('0'+f.nextInc))
	f.incidents[inc.ID] = inc
	f.updates[inc.ID] = []domain.IncidentUpdate{{IncidentID: inc.ID, Status: inc.Status, Body: openingBody, Author: author}}
	return inc, nil
}

func (f *fakeStore) AddIncidentUpdate(_ context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates[upd.IncidentID] = append(f.updates[upd.IncidentID], upd)
	if inc, ok := f.incidents[upd.IncidentID]; ok {
		inc.Status = upd.Status
		f.incidents[upd.IncidentID] = inc
	}
	return upd, nil
}

func (f *fakeStore) statusOf(monitorID string) domain.MonitorStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[monitorID]
}

func (f *fakeStore) hbCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hbs)
}

func (f *fakeStore) openAutoIncidentCount(monitorID string) (open, resolved int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, inc := range f.incidents {
		if inc.MonitorID != monitorID || inc.Source != domain.SourceAuto {
			continue
		}
		if inc.Status == domain.IncidentResolved {
			resolved++
		} else {
			open++
		}
	}
	return
}

type fakeRecorder struct {
	mu            sync.Mutex
	up, down      int
	incidentsOpen int
	outcomes      map[string]int // reason → count
}

func (r *fakeRecorder) RecordCheck(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if up {
		r.up++
	} else {
		r.down++
	}
}

func (r *fakeRecorder) RecordResultOutcome(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcomes == nil {
		r.outcomes = map[string]int{}
	}
	r.outcomes[reason]++
}

func (r *fakeRecorder) outcomeCount(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[reason]
}

func (r *fakeRecorder) RecordIncidentOpened() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.incidentsOpen++
}

// TestHandleMapsNonAppliedOutcome proves handle() routes a non-applied store outcome to
// the outcome metric and does NOT count it as a check (RecordCheck only on an inserted
// heartbeat) or drive a transition.
func TestHandleMapsNonAppliedOutcome(t *testing.T) {
	fs := newFakeStore()
	fs.forceReason = store.ReasonFutureTimestamp
	fs.monitors["m1"] = domain.Monitor{ID: "m1"}
	rec := &fakeRecorder{}
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true, Ts: time.Now()})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if rec.outcomeCount(store.ReasonFutureTimestamp) == 1 {
			rec.mu.Lock()
			up, down := rec.up, rec.down
			rec.mu.Unlock()
			if up != 0 || down != 0 {
				t.Fatalf("non-applied outcome must not count a check: up=%d down=%d", up, down)
			}
			fs.mu.Lock()
			n := len(fs.hbs)
			fs.mu.Unlock()
			if n != 0 {
				t.Fatalf("quarantined result must not insert: hbs=%d", n)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle did not record the outcome metric")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConsumerPersistsResults(t *testing.T) {
	fs := newFakeStore()
	rec := &fakeRecorder{}
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true, Code: 200})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Code: 500})

	deadline := time.Now().Add(2 * time.Second)
	for {
		fs.mu.Lock()
		n := len(fs.hbs)
		status := fs.statuses["m1"]
		fs.mu.Unlock()
		if n == 2 {
			if status != domain.StatusDown {
				t.Fatalf("last status = %q, want down", status)
			}
			rec.mu.Lock()
			up, down := rec.up, rec.down
			rec.mu.Unlock()
			if up != 1 || down != 1 {
				t.Fatalf("recorder up=%d down=%d, want 1/1", up, down)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumer recorded %d heartbeats, want 2", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAutoIncidentOpenAndResolve(t *testing.T) {
	fs := newFakeStore()
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "api-health", AutoIncident: true}
	rec := &fakeRecorder{}
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// First a healthy check (pending→up, no incident), then two failures
	// (up→down opens one; down→down must NOT open a second), then recovery.
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true, Code: 200})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Code: 500, Msg: "500"})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Code: 500, Msg: "500"})

	if !waitFor(func() bool { o, _ := fs.openAutoIncidentCount("m1"); return o == 1 }) {
		o, _ := fs.openAutoIncidentCount("m1")
		t.Fatalf("expected exactly 1 open auto-incident, got %d", o)
	}
	rec.mu.Lock()
	opened := rec.incidentsOpen
	rec.mu.Unlock()
	if opened != 1 {
		t.Fatalf("recorder incidentsOpen = %d, want 1", opened)
	}

	// Recovery resolves the open incident. (The webhook/notification events are now
	// enqueued transactionally by the store — see the store integration test — not
	// emitted from the ingest pipeline.)
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true, Code: 200})
	if !waitFor(func() bool { o, r := fs.openAutoIncidentCount("m1"); return o == 0 && r == 1 }) {
		o, r := fs.openAutoIncidentCount("m1")
		t.Fatalf("after recovery: open=%d resolved=%d, want 0/1", o, r)
	}
}

// TestAutoIncidentOpenRetries proves a transient CreateIncident failure is retried
// rather than losing the incident — critical for escalation-policy monitors, whose
// only alert is the ladder paging over that incident.
func TestAutoIncidentOpenRetries(t *testing.T) {
	fs := newFakeStore()
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "api", AutoIncident: true, EscalationPolicyID: "ep1"}
	fs.createErrs = []error{errors.New("db blip"), errors.New("db blip")} // fail twice, then succeed
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, &fakeRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Code: 500, Msg: "500"})
	if !waitFor(func() bool { o, _ := fs.openAutoIncidentCount("m1"); return o == 1 }) {
		o, _ := fs.openAutoIncidentCount("m1")
		t.Fatalf("expected the incident to open after retries, got %d open", o)
	}
	fs.mu.Lock()
	calls := fs.createCalls
	fs.mu.Unlock()
	if calls != 3 {
		t.Fatalf("CreateIncident called %d times, want 3 (2 failures + success)", calls)
	}
}

// TestAutoIncidentAlreadyOpenIsBenign proves ErrAlreadyOpen (the unique-index race)
// is treated as success: no retry, no error, no second incident.
func TestAutoIncidentAlreadyOpenIsBenign(t *testing.T) {
	fs := newFakeStore()
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "api", AutoIncident: true}
	fs.createErrs = []error{store.ErrAlreadyOpen}
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, &fakeRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Code: 500, Msg: "500"})
	// Give the pipeline time to process; ErrAlreadyOpen must NOT retry.
	if !waitFor(func() bool { fs.mu.Lock(); n := fs.createCalls; fs.mu.Unlock(); return n >= 1 }) {
		t.Fatal("CreateIncident was never called")
	}
	time.Sleep(300 * time.Millisecond) // longer than one backoff — a retry would show here
	fs.mu.Lock()
	calls := fs.createCalls
	fs.mu.Unlock()
	if calls != 1 {
		t.Fatalf("CreateIncident called %d times, want 1 (ErrAlreadyOpen must not retry)", calls)
	}
	if o, _ := fs.openAutoIncidentCount("m1"); o != 0 {
		t.Fatalf("no incident should be recorded on ErrAlreadyOpen, got %d open", o)
	}
}

// TestAutoIncidentDisabledSkipsOpen verifies the per-monitor opt-out: a monitor
// with AutoIncident=false does not open an incident on going down, yet an
// already-open incident still resolves on recovery (resolve is unconditional).
func TestAutoIncidentDisabledSkipsOpen(t *testing.T) {
	fs := newFakeStore()
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "quiet", AutoIncident: false}
	rec := &fakeRecorder{}
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// up → down: with auto-incidents off, no incident should open.
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true, Code: 200})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Code: 500, Msg: "500"})
	// Wait for the down transition to be processed, then assert nothing opened.
	if !waitFor(func() bool { return fs.statusOf("m1") == domain.StatusDown }) {
		t.Fatal("monitor never reached down")
	}
	if o, _ := fs.openAutoIncidentCount("m1"); o != 0 {
		t.Fatalf("auto-incident should be suppressed, got %d open", o)
	}

	// A pre-existing open auto-incident must still resolve on recovery even with
	// the flag off (e.g. it was opened before the operator disabled the feature).
	fs.mu.Lock()
	fs.incidents["pre"] = domain.Incident{ID: "pre", MonitorID: "m1", Status: domain.IncidentInvestigating, Source: domain.SourceAuto}
	fs.mu.Unlock()
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true, Code: 200})
	if !waitFor(func() bool { o, r := fs.openAutoIncidentCount("m1"); return o == 0 && r == 1 }) {
		o, r := fs.openAutoIncidentCount("m1")
		t.Fatalf("pre-existing incident should resolve on recovery: open=%d resolved=%d", o, r)
	}
}

// TestConfirmationsBeforeDown verifies failure_threshold: the monitor stays up
// (and opens no incident) until N consecutive failures, then flips down and pages.
func TestConfirmationsBeforeDown(t *testing.T) {
	fs := newFakeStore()
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "api", AutoIncident: true, FailureThreshold: 3}
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, &fakeRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false})
	if !waitFor(func() bool { return fs.hbCount() == 3 }) {
		t.Fatal("first three checks not processed")
	}
	// Two failures < threshold(3): still up, no incident.
	if fs.statusOf("m1") != domain.StatusUp {
		t.Fatalf("after 2 failures status = %q, want up (unconfirmed)", fs.statusOf("m1"))
	}
	if o, _ := fs.openAutoIncidentCount("m1"); o != 0 {
		t.Fatalf("no incident before the threshold, got %d", o)
	}
	// Third consecutive failure confirms down and opens the incident.
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Msg: "down"})
	if !waitFor(func() bool {
		o, _ := fs.openAutoIncidentCount("m1")
		return fs.statusOf("m1") == domain.StatusDown && o == 1
	}) {
		o, _ := fs.openAutoIncidentCount("m1")
		t.Fatalf("3rd failure should confirm down + open incident: status=%q open=%d", fs.statusOf("m1"), o)
	}
}

// TestMaintenanceSuppressesIncident verifies that a down flip inside a maintenance
// window still updates status but opens no auto-incident.
func TestMaintenanceSuppressesIncident(t *testing.T) {
	fs := newFakeStore()
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "api", AutoIncident: true, FailureThreshold: 1}
	fs.maint["m1"] = true // active maintenance window
	disp := dispatch.NewInProc(8)
	c := New(fs, disp, &fakeRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true})
	_ = disp.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: false, Msg: "down"})
	if !waitFor(func() bool { return fs.statusOf("m1") == domain.StatusDown }) {
		t.Fatal("monitor should still reach down in maintenance")
	}
	// Give the pipeline a moment; no incident should be opened.
	if !waitFor(func() bool { return fs.hbCount() == 2 }) {
		t.Fatal("checks not processed")
	}
	if o, _ := fs.openAutoIncidentCount("m1"); o != 0 {
		t.Fatalf("maintenance should suppress the incident, got %d", o)
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}
