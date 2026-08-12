package runtime

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
)

func newThrottleProvider() *Provider {
	return &Provider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

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
		p.warnThrottled(fmt.Sprintf("cold-%d", i), "file_provider_file_rejected")
		if i%8 == 0 {
			p.warnThrottled(hot, "file_provider_file_rejected") // suppressed touch keeps it hot
		}
	}

	if got := len(p.throttle); got > warnThrottleMax {
		t.Fatalf("throttle map grew to %d, want <= %d", got, warnThrottleMax)
	}
	el2 := p.throttle[hotID]
	if el2 == nil {
		t.Fatal("hot key was evicted despite being re-touched under churn")
	}
	if !el2.Value.(*throttleEntry).last.Equal(first) {
		t.Fatal("suppressed touch reset the throttle window; hot key would re-log within errorLogEvery")
	}
}
