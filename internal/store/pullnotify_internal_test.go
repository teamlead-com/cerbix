package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestPullNotifierWakesOnEnqueue verifies the real Postgres LISTEN/NOTIFY path: a waiter
// for a region returns promptly when a job is enqueued (rather than blocking to max hold).
func TestPullNotifierWakesOnEnqueue(t *testing.T) {
	st, ctx := outboxTestStore(t)
	notifier := st.NewPullNotifier(slog.New(slog.NewTextHandler(io.Discard, nil)))
	nctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go notifier.Run(nctx)
	time.Sleep(300 * time.Millisecond) // let the LISTEN connection establish

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		notifier.Wait(nctx, "geo3", 5*time.Second) // max hold well above the expected wake
		done <- time.Since(start)
	}()
	time.Sleep(100 * time.Millisecond) // ensure the waiter is registered before NOTIFY

	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{}`), 60, 0, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case d := <-done:
		if d > 2*time.Second {
			t.Fatalf("Wait took %v — NOTIFY not delivered, fell back to max hold", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return")
	}

	// A waiter for a different region is not woken by geo3's job (it hits its own timeout).
	start := time.Now()
	notifier.Wait(nctx, "other", 300*time.Millisecond)
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("waiter for another region should not have been woken by geo3")
	}
}
