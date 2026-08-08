package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestConfigNotifierWakesOnConfigChange verifies the real Postgres LISTEN/NOTIFY path for the
// scheduler wake signal (spec §12): a subscriber returns promptly when monitor_config_changed
// fires (as a committed file-provider apply does), rather than the scheduler waiting out its
// slow snapshot refresh.
func TestConfigNotifierWakesOnConfigChange(t *testing.T) {
	st, ctx := outboxTestStore(t)
	n := st.NewConfigNotifier(slog.New(slog.NewTextHandler(io.Discard, nil)))
	nctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go n.Run(nctx)
	time.Sleep(300 * time.Millisecond) // let the LISTEN connection establish

	ch, unsub := n.Subscribe()
	defer unsub()

	if _, err := st.pool.Exec(ctx, `SELECT pg_notify($1, '{}')`, FileConfigChannel); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("config notifier did not deliver the wake signal")
	}
}
