package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.4a — the destructive paths. Each of these ends a FIRING announcement by editing away
// the thing that fired, which is precisely the case an evaluator cannot notice: after the commit
// there is nothing left to evaluate. The close therefore has to be enqueued by the removal itself,
// and it has to be deliverable afterwards.

// allServiceAlerts reads every published service alert in enqueue order, whatever its signal.
func allServiceAlerts(t *testing.T, st *Store, ctx context.Context) []domain.ServiceAlert {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT payload FROM outbox_events WHERE topic = 'service_alert' ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("read service alerts: %v", err)
	}
	defer rows.Close()
	var out []domain.ServiceAlert
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan service alert: %v", err)
		}
		var a domain.ServiceAlert
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decode service alert: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// Deleting a service while a burn rule is FIRING must end the announcement exactly once, reach the
// people the onset reached, and stay deliverable — the episode outlives the service by design, and
// the delivery fence lets a close through when the latch it names is gone.
func TestDeletingAFiringServiceClosesItsAnnouncementOnce(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60) // ~16.7×, over the rule's threshold of 14

	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want the rule to fire before we delete anything", got.Onsets)
	}
	onset := burnEventsFor(t, st, ctx, f.targetID)
	if len(onset) != 1 || !onset[0].Firing {
		t.Fatalf("expected exactly one onset, got %+v", onset)
	}

	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}

	events := allServiceAlerts(t, st, ctx)
	if len(events) != 2 {
		t.Fatalf("%d events after deleting a firing service, want the onset and ONE close", len(events))
	}
	got := events[1]
	switch {
	case got.Firing:
		t.Fatal("the deletion published another onset")
	case got.CloseReason != domain.CloseServiceDeleted:
		t.Fatalf("close reason = %q, want service_deleted — a deletion is not a recovery", got.CloseReason)
	case got.Signal != domain.ServiceSignalBurn || got.RuleKey != oneBurnRuleKey:
		t.Fatalf("close identity = %q/%q", got.Signal, got.RuleKey)
	case got.SLATargetID != f.targetID || got.Window != "30d":
		t.Fatalf("close target = %q/%q, want the deleted target's own identity", got.SLATargetID, got.Window)
	case len(got.Recipients) == 0 || len(got.Recipients) != len(onset[0].Recipients):
		t.Fatalf("close recipients = %v, want the onset's snapshot %v", got.Recipients, onset[0].Recipients)
	case got.Seq <= onset[0].Seq:
		t.Fatalf("close seq %d does not follow the onset's %d", got.Seq, onset[0].Seq)
	case got.EpisodeID != onset[0].EpisodeID:
		t.Fatalf("the close ends episode %q, want the onset's %q", got.EpisodeID, onset[0].EpisodeID)
	}

	// The episode survives its service, with everything the close needed: the DB nulled ONLY
	// `service_id`, which is what keeps this row from being a dangling reference.
	var nulled, kept bool
	var reason, name string
	var recipients []string
	if err := st.pool.QueryRow(ctx, `
		SELECT service_id IS NULL, project_id = $1, close_reason, service_name,
		       ARRAY(SELECT jsonb_array_elements_text(recipients))
		  FROM service_alert_episodes`, f.projectID).
		Scan(&nulled, &kept, &reason, &name, &recipients); err != nil {
		t.Fatalf("read episode: %v", err)
	}
	if !nulled || !kept || reason != string(domain.CloseServiceDeleted) || name == "" || len(recipients) == 0 {
		t.Fatalf("episode after deletion: nulled=%v tenant_kept=%v reason=%q name=%q recipients=%v",
			nulled, kept, reason, name, recipients)
	}

	// Delivery must let it through. The latch cascaded away with the service, and a close whose
	// latch is gone is exactly the case the fence answers with ErrNotFound so the ending is still
	// announced.
	if _, err := st.ServiceAlertSequence(ctx, got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fence for the close = %v, want ErrNotFound so delivery proceeds", err)
	}
}

// The same act while NOTHING is firing must stay silent: a close with no announcement behind it
// would page people about a service they were never told was broken.
func TestDeletingAQuietServiceAnnouncesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, 0) // no bad time at all: burn rate 0

	if got := burnEvalOnce(t, st, ctx); got.Onsets != 0 {
		t.Fatalf("onsets = %d, want a quiet service", got.Onsets)
	}
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	if events := allServiceAlerts(t, st, ctx); len(events) != 0 {
		t.Fatalf("deleting a quiet service published %+v, want silence", events)
	}
}

// The close and the removal are ONE transaction, and this is the proof that costs something to
// fake: make the DELETE itself fail AFTER the close has been staged, and nothing may survive — no
// outbox row, no closed episode, no cleared latch. Without a shared transaction the close would
// already be enqueued, and subscribers would be told an announcement ended for a service that is
// still there and still burning.
func TestAFailedDeletionAnnouncesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want the rule to fire first", got.Onsets)
	}

	// A trigger that refuses the DELETE. It stands in for every way the removal can fail after the
	// close is staged — a RESTRICT reference, a serialization failure, a lost connection.
	if _, err := st.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION refuse_service_delete() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'refused by test'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER refuse_service_delete_trg BEFORE DELETE ON services
		    FOR EACH ROW EXECUTE FUNCTION refuse_service_delete();`); err != nil {
		t.Fatalf("install refusing trigger: %v", err)
	}
	defer func() {
		if _, err := st.pool.Exec(ctx,
			`DROP TRIGGER IF EXISTS refuse_service_delete_trg ON services`); err != nil {
			t.Fatalf("drop trigger: %v", err)
		}
	}()

	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err == nil {
		t.Fatal("the refused deletion reported success")
	}

	// The onset, and ONLY the onset.
	if events := allServiceAlerts(t, st, ctx); len(events) != 1 || !events[0].Firing {
		t.Fatalf("after a refused deletion the events are %+v, want only the onset", events)
	}
	var open, firing bool
	if err := st.pool.QueryRow(ctx,
		`SELECT closed_at IS NULL FROM service_alert_episodes`).Scan(&open); err != nil {
		t.Fatalf("read episode: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT firing FROM service_burn_alert_state`).Scan(&firing); err != nil {
		t.Fatalf("read latch: %v", err)
	}
	if !open || !firing {
		t.Fatalf("a refused deletion left episode_open=%v latch_firing=%v, want both intact", open, firing)
	}
}
