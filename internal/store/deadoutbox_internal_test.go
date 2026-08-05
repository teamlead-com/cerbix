package store

import (
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestDeadOutboxListAndReplay(t *testing.T) {
	st, ctx := outboxTestStore(t)

	seed := func(status string) string {
		var id string
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO outbox_events (topic, payload, status, attempts, last_error)
			 VALUES ($1, '{}', $2, 10, 'boom') RETURNING id`,
			domain.TopicIncidentEvent, status).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
		return id
	}
	dead1 := seed("dead")
	dead2 := seed("dead")
	pending := seed("pending")

	// List returns only dead events.
	dead, err := st.ListDeadOutbox(ctx, 100)
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dead) != 2 {
		t.Fatalf("dead count = %d, want 2", len(dead))
	}
	for _, e := range dead {
		if e.Status != "dead" || e.LastError != "boom" {
			t.Fatalf("unexpected dead event: %+v", e)
		}
	}

	// Replaying a non-dead (pending) id is a no-op → ErrNotFound.
	if err := st.ReplayDeadOutbox(ctx, pending); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay pending err = %v, want ErrNotFound", err)
	}

	// Replaying a dead event resets it to a fresh pending row.
	if err := st.ReplayDeadOutbox(ctx, dead1); err != nil {
		t.Fatalf("replay dead1: %v", err)
	}
	var status string
	var attempts int
	var due bool
	if err := st.pool.QueryRow(ctx,
		`SELECT status, attempts, next_attempt_at <= now() FROM outbox_events WHERE id = $1`, dead1).
		Scan(&status, &attempts, &due); err != nil {
		t.Fatalf("reload dead1: %v", err)
	}
	if status != "pending" || attempts != 0 || !due {
		t.Fatalf("after replay: status=%s attempts=%d due=%v, want pending/0/true", status, attempts, due)
	}

	// Replay-all requeues the remaining dead event (dead2) and no more.
	n, err := st.ReplayAllDeadOutbox(ctx)
	if err != nil {
		t.Fatalf("replay all: %v", err)
	}
	if n != 1 {
		t.Fatalf("replay-all count = %d, want 1 (only dead2)", n)
	}
	remaining, _ := st.ListDeadOutbox(ctx, 100)
	if len(remaining) != 0 {
		t.Fatalf("dead remaining = %d, want 0", len(remaining))
	}
	_ = dead2
}
