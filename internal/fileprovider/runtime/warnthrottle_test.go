package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

func newThrottleProvider() *Provider {
	return &Provider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// countingHandler counts emitted records, separating the aggregate suppressed-summary from
// ordinary warnings, so tests can assert on the actual log volume.
type countingHandler struct {
	warns     int
	summaries int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "file_provider_warnings_suppressed" {
		h.summaries++
	} else {
		h.warns++
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// TestWarnThrottledBounded proves the throttle map/LRU never grows past the cap even when fed
// far more distinct keys than the cap (the #7 unbounded-growth regression).
func TestWarnThrottledBounded(t *testing.T) {
	p := newThrottleProvider()
	for i := 0; i < warnThrottleMax*3; i++ {
		p.warnThrottled(fmt.Sprintf("path-%d", i), "file_provider_file_rejected", "i", i)
	}
	if got := len(p.throttle); got > warnThrottleMax {
		t.Fatalf("throttle map grew to %d, want <= %d", got, warnThrottleMax)
	}
	if got := p.throttleLRU.Len(); got > warnThrottleMax {
		t.Fatalf("throttle LRU grew to %d, want <= %d", got, warnThrottleMax)
	}
	if len(p.throttle) != p.throttleLRU.Len() {
		t.Fatalf("map (%d) and LRU (%d) diverged", len(p.throttle), p.throttleLRU.Len())
	}
}

// TestWarnThrottledRetainsHotKey proves that a repeatedly-touched key survives cap-exceeding
// churn AND stays rate-limited (its throttle window is not reset by a suppressed touch).
func TestWarnThrottledRetainsHotKey(t *testing.T) {
	p := newThrottleProvider()
	const hot = "hot-path"
	hotID := "file_provider_file_rejected|" + hot

	p.warnThrottled(hot, "file_provider_file_rejected") // logs once, inserts
	el := p.throttle[hotID]
	if el == nil {
		t.Fatal("hot key not inserted")
	}
	first := el.Value.(*throttleEntry).last

	for i := 0; i < warnThrottleMax*3; i++ {
		if i%8 == 0 {
			p.warnThrottled(hot, "file_provider_file_rejected") // suppressed touch keeps it hot
		}
		p.warnThrottled(fmt.Sprintf("cold-%d", i), "file_provider_file_rejected")
	}

	el2 := p.throttle[hotID]
	if el2 == nil {
		t.Fatal("hot key was evicted despite being re-touched under churn")
	}
	if !el2.Value.(*throttleEntry).last.Equal(first) {
		t.Fatal("suppressed touch reset the throttle window; hot key would re-log within errorLogEvery")
	}
}

// TestWarnThrottledEmissionBudget proves the provider-wide log-volume bound (#7 rework): a
// working set wider than the LRU cap cannot flood the log — emissions stay within
// warnEmitBudget per window — and when the window rolls, a single suppressed-summary is
// emitted and ordinary logging resumes.
func TestWarnThrottledEmissionBudget(t *testing.T) {
	h := &countingHandler{}
	cur := time.Unix(1_700_000_000, 0)
	p := &Provider{logger: slog.New(h), nowFn: func() time.Time { return cur }}

	// A wide round-robin working set within one window (clock is frozen).
	for i := 0; i < warnThrottleMax*3; i++ {
		p.warnThrottled(fmt.Sprintf("p-%d", i), "file_provider_file_rejected")
	}
	if h.warns == 0 {
		t.Fatal("expected some warnings emitted in the first window")
	}
	if h.warns > warnEmitBudget {
		t.Fatalf("emitted %d warnings in one window, budget is %d", h.warns, warnEmitBudget)
	}
	if h.summaries != 0 {
		t.Fatalf("no window has rolled yet, got %d suppressed-summaries", h.summaries)
	}

	// Roll the window: the suppressed count is summarized once and logging resumes.
	cur = cur.Add(errorLogEvery + time.Second)
	warnsBefore := h.warns
	p.warnThrottled("after-window", "file_provider_file_rejected")
	if h.summaries != 1 {
		t.Fatalf("expected exactly 1 suppressed-summary after the window rolled, got %d", h.summaries)
	}
	if h.warns != warnsBefore+1 {
		t.Fatalf("logging did not resume in the new window (warns %d -> %d)", warnsBefore, h.warns)
	}
}

// TestWarnThrottledEvictsAtCap drives the LRU past its cap by ADMITTING more than
// warnThrottleMax distinct keys — which, given the per-window emission budget, takes many
// windows (an injected clock advances between batches). It exercises the real Back()/eviction
// branch and asserts the map and list stay in lockstep at the cap.
func TestWarnThrottledEvictsAtCap(t *testing.T) {
	h := &countingHandler{}
	cur := time.Unix(1_700_000_000, 0)
	p := &Provider{logger: slog.New(h), nowFn: func() time.Time { return cur }}

	id := 0
	admitted := 0
	for admitted <= warnThrottleMax+warnEmitBudget { // enough windows to overflow the cap
		for j := 0; j < warnEmitBudget; j++ { // exactly the budget of fresh keys per window
			p.warnThrottled(fmt.Sprintf("k-%d", id), "file_provider_file_rejected")
			id++
			admitted++
		}
		cur = cur.Add(errorLogEvery + time.Second) // roll to the next window
	}

	if got := p.throttleLRU.Len(); got != warnThrottleMax {
		t.Fatalf("LRU should be full at the cap %d after admitting %d keys, got %d", warnThrottleMax, admitted, got)
	}
	if len(p.throttle) != p.throttleLRU.Len() {
		t.Fatalf("map (%d) and LRU (%d) diverged after eviction", len(p.throttle), p.throttleLRU.Len())
	}
	// The most-recently-admitted key is resident; the oldest was evicted.
	newest := fmt.Sprintf("file_provider_file_rejected|k-%d", id-1)
	if _, ok := p.throttle[newest]; !ok {
		t.Fatal("most-recently-admitted key must be resident")
	}
	if _, ok := p.throttle["file_provider_file_rejected|k-0"]; ok {
		t.Fatal("oldest key should have been evicted at the cap")
	}
}
