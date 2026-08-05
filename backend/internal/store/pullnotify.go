package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PullNotifier turns Postgres LISTEN/NOTIFY on the pull_jobs channel into an in-process
// wake signal, so the agent long-poll (GET /agent/jobs) can hold a request open until a
// job appears for its region instead of hammering the queue. One background goroutine
// holds a single LISTEN connection and fans out to in-process waiters; on connection
// loss it reconnects. A missed notification is harmless — the long-poll's max-hold
// re-poll still delivers.
type PullNotifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

// NewPullNotifier builds a notifier bound to the store's pool.
func (s *Store) NewPullNotifier(logger *slog.Logger) *PullNotifier {
	return &PullNotifier{pool: s.pool, logger: logger, waiters: map[string][]chan struct{}{}}
}

// Run holds the LISTEN connection and dispatches notifications until ctx is cancelled.
func (n *PullNotifier) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := n.listen(ctx); err != nil && ctx.Err() == nil {
			if n.logger != nil {
				n.logger.Warn("pull_notifier_reconnect", "error", err.Error())
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (n *PullNotifier) listen(ctx context.Context) error {
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+PullChannel); err != nil {
		return err
	}
	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		n.signal(notif.Payload)
	}
}

func (n *PullNotifier) signal(region string) {
	n.mu.Lock()
	chans := n.waiters[region]
	delete(n.waiters, region)
	n.mu.Unlock()
	for _, ch := range chans {
		close(ch) // closing wakes every waiter for this region
	}
}

// Wait blocks until a job is enqueued for region, max elapses, or ctx is done.
func (n *PullNotifier) Wait(ctx context.Context, region string, max time.Duration) {
	ch := make(chan struct{})
	n.mu.Lock()
	n.waiters[region] = append(n.waiters[region], ch)
	n.mu.Unlock()

	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
		n.remove(region, ch)
	case <-ctx.Done():
		n.remove(region, ch)
	}
}

// remove drops a waiter channel that timed out (so idle regions don't leak waiters).
func (n *PullNotifier) remove(region string, ch chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	w := n.waiters[region]
	for i, c := range w {
		if c == ch {
			n.waiters[region] = append(w[:i], w[i+1:]...)
			break
		}
	}
	if len(n.waiters[region]) == 0 {
		delete(n.waiters, region)
	}
}
