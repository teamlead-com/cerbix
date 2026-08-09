package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigNotifier turns LISTEN/NOTIFY on FileConfigChannel (monitor_config_changed) into an
// in-process stream of wake signals for the scheduler leader, so a file-provider apply that
// changed execution config is picked up within a tick instead of waiting out the slow snapshot
// refresh (spec §12). It mirrors ConfirmNotifier: one background goroutine holds a single LISTEN
// connection and fans out to subscribers; the payload is discarded (any signal forces a full
// snapshot reload), and a dropped signal is harmless because the periodic refresh is the
// fallback.
type ConfigNotifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu   sync.Mutex
	subs []chan struct{}
}

// NewConfigNotifier builds a notifier bound to the store's pool.
func (s *Store) NewConfigNotifier(logger *slog.Logger) *ConfigNotifier {
	return &ConfigNotifier{pool: s.pool, logger: logger}
}

// Run holds the LISTEN connection and dispatches notifications until ctx is done.
func (n *ConfigNotifier) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := n.listen(ctx); err != nil && ctx.Err() == nil {
			if n.logger != nil {
				n.logger.Warn("config_notifier_reconnect", "error", err.Error())
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (n *ConfigNotifier) listen(ctx context.Context) error {
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+FileConfigChannel); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		n.mu.Lock()
		for _, ch := range n.subs {
			select {
			case ch <- struct{}{}:
			default: // subscriber already has a pending wake — coalesce
			}
		}
		n.mu.Unlock()
	}
}

// Subscribe returns a buffered wake stream and a cancel func.
func (n *ConfigNotifier) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
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
