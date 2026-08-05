package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfirmChannel is the pg_notify channel RecordCheckStatus signals on when a
// monitor enters (or continues) its failure-confirmation phase; the payload is
// the monitor id.
const ConfirmChannel = "monitor_confirm"

// ConfirmNotifier turns LISTEN/NOTIFY on monitor_confirm into an in-process
// stream of monitor ids for the scheduler leader, so confirmation probes can be
// rescheduled the moment a failure is counted instead of waiting for the next
// snapshot refresh. One background goroutine holds a single LISTEN connection
// (the PullNotifier pattern) and fans out to subscribers; a dropped or missed
// signal is harmless — the scheduler's snapshot fallback catches up.
type ConfirmNotifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu   sync.Mutex
	subs []chan string
}

// NewConfirmNotifier builds a notifier bound to the store's pool.
func (s *Store) NewConfirmNotifier(logger *slog.Logger) *ConfirmNotifier {
	return &ConfirmNotifier{pool: s.pool, logger: logger}
}

// Run holds the LISTEN connection and dispatches notifications until ctx is done.
func (n *ConfirmNotifier) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := n.listen(ctx); err != nil && ctx.Err() == nil {
			if n.logger != nil {
				n.logger.Warn("confirm_notifier_reconnect", "error", err.Error())
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (n *ConfirmNotifier) listen(ctx context.Context) error {
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+ConfirmChannel); err != nil {
		return err
	}
	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		n.mu.Lock()
		for _, ch := range n.subs {
			select {
			case ch <- notif.Payload:
			default: // subscriber is slow — drop; the snapshot fallback catches up
			}
		}
		n.mu.Unlock()
	}
}

// Subscribe returns a buffered stream of monitor ids and a cancel func.
func (n *ConfirmNotifier) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 64)
	n.mu.Lock()
	n.subs = append(n.subs, ch)
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, c := range n.subs {
			if c == ch {
				n.subs = append(n.subs[:i], n.subs[i+1:]...)
				break
			}
		}
	}
}
