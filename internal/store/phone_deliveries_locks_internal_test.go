package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The locks of the phone_deliveries store, after the Task 10 review: the storm bounds read under the
// number's lock (finding 1), the monitor lock keyed by the canonical uuid (finding 2), and the
// multi-row writes locking their rows in id order (finding 3). DB-gated.

// waitLockWaiters blocks until n transactions wait on the two-key advisory lock (class, key) — that
// lock only, so another waiter in the database cannot end the wait early.
func (w phoneDeliveryWorld) waitLockWaiters(t *testing.T, class, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
			   AND classid = hashtext($1)::oid AND objid = hashtext($2)::oid`, class, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d waiters on %s; want %d", got, class, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitRowWaiters blocks until a transaction waits for blocker's transaction to end (a row lock it
// holds).
func (w phoneDeliveryWorld) waitRowWaiters(t *testing.T, blocker pgx.Tx) {
	t.Helper()
	var xid string
	if err := blocker.QueryRow(w.ctx, `SELECT xid(pg_current_xact_id())::text`).Scan(&xid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_locks
			 WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text = $1`, xid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nobody waits on the blocker's row")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// §6.2 / P13: the bounds are read inside the reservation, under the number's lock. A settings change
// committed while a reservation queues for the number is the bound that reservation applies — as
// current as the count it is compared with, whatever any process's settings snapshot says.
func TestPhoneStormBoundsReadUnderTheNumberLock(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	loose := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}

	// The stored bound is the one applied: 2 per number, then raised to 3.
	two := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 2}
	for i := 0; i < 2; i++ {
		if r := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, two)); r.Status != domain.PhoneReservationProceed {
			t.Fatalf("reservation %d: %s", i, r.Status)
		}
	}
	if r := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, two)); r.Status != domain.PhoneReservationStormLimited {
		t.Fatalf("third at a stored bound of 2: %s; want storm_limited", r.Status)
	}
	next := w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 3})
	if r := w.reserve(t, next); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("after raising the stored bound to 3: %s", r.Status)
	}

	// A reservation that queues behind the number's lock sees the bound committed while it waited.
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM phone_deliveries`); err != nil {
		t.Fatal(err)
	}
	queued := w.in(t, domain.PhoneAlertMonitorBurn, loose)
	_, hashes, err := w.st.phoneNumberHashes(pdNumber)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(w.ctx) //nolint:errcheck
	if err := lockPhoneNumberTx(w.ctx, blocker, hashes); err != nil {
		t.Fatal(err)
	}
	var res domain.PhoneReservation
	var resErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, resErr = w.st.ReservePhoneDelivery(w.ctx, queued)
	}()
	w.waitLockWaiters(t, "phone_delivery_number", hashes[0], 1)
	// The bound drops to zero slots' worth: one row already holds the only slot.
	w.setStorm(t, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 1})
	if _, err := blocker.Exec(w.ctx, `INSERT INTO phone_deliveries (project_id, channel_id, number_hash, kind, alert_kind, reason)
		VALUES ($1, $2, $3, 'sms', 'monitor_burn', 'sent')`, w.project, w.channel, hashes[0]); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(w.ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if resErr != nil {
		t.Fatal(resErr)
	}
	if res.Status != domain.PhoneReservationStormLimited {
		t.Fatalf("queued reservation: %s; want storm_limited under the bound committed while it waited", res.Status)
	}
}

// D32: the monitor's lock is keyed by the canonical uuid, so two spellings of one monitor id — an
// upper-case one here — still exclude each other.
func TestPhoneMonitorLockIsCanonical(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "canonical-lock")
	w.setSeq(t, m, 2)
	in := w.in(t, domain.PhoneAlertMonitorDown, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100})
	in.MonitorID, in.MonitorSeq = m, 2

	blocker, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(w.ctx) //nolint:errcheck
	if err := lockPhoneMonitorTx(w.ctx, blocker, strings.ToUpper(m)); err != nil {
		t.Fatal(err)
	}
	var resErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, resErr = w.st.ReservePhoneDelivery(w.ctx, in)
	}()
	w.waitAdvisoryWaiters(t, m, 1)
	if err := blocker.Rollback(w.ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if resErr != nil {
		t.Fatal(resErr)
	}
}

// pdLockPair is two `sending` DOWN rows of one monitor's outage whose natural scan order is the
// reverse of their id order: the higher id is older (the sweep's index), and its live version comes
// first in the heap and in the monitor index (both rows were rewritten, the higher one first).
func (w phoneDeliveryWorld) pdLockPair(t *testing.T, m string) (lo, hi string) {
	t.Helper()
	w.setSeq(t, m, 7)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}
	var ids []string
	for i := 0; i < 2; i++ {
		in := w.in(t, domain.PhoneAlertMonitorDown, storm)
		in.MonitorID, in.MonitorSeq = m, 7
		ids = append(ids, w.reserve(t, in).ID)
	}
	lo, hi = ids[0], ids[1]
	if hi < lo {
		lo, hi = hi, lo
	}
	w.age(t, hi, 200*time.Second, 200*time.Second)
	w.age(t, lo, 100*time.Second, 100*time.Second)
	return lo, hi
}

// lockRow holds the row's lock in a transaction of its own until the test releases it.
func (w phoneDeliveryWorld) lockRow(t *testing.T, id string) pgx.Tx {
	t.Helper()
	tx, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(w.ctx) })
	if _, err := tx.Exec(w.ctx, `SELECT 1 FROM phone_deliveries WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	return tx
}

// rowFree reports whether nobody holds the row's lock now.
func (w phoneDeliveryWorld) rowFree(t *testing.T, id string) bool {
	t.Helper()
	tx, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(w.ctx) //nolint:errcheck
	var got string
	err = tx.QueryRow(w.ctx, `SELECT id::text FROM phone_deliveries WHERE id = $1 FOR UPDATE SKIP LOCKED`, id).Scan(&got)
	if noRows(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// Task 10 review, finding 3: the D32 attachment locks its rows in id order. With the lower id held by
// a third transaction, it must be waiting for it while holding NOTHING else.
//
// FR-041a review P1-3: the leader's sweep no longer waits at all. A row another transaction holds —
// a reservation finishing it, or a cascade deleting it — is skipped and closed by a later pass, so the
// sweep can never be one side of a wait cycle. Its subtest is therefore about skipping, not about
// order; the name is kept for the attachment's subtest, which is (Group-A review M-3).
func TestPhoneDeliveryMultiRowWritesLockInIDOrder(t *testing.T) {
	t.Run("sweep skips a locked row", func(t *testing.T) {
		w := newPhoneDeliveryWorld(t)
		lo, hi := w.pdLockPair(t, w.monitor(t, "sweep-skip"))
		blocker := w.lockRow(t, lo)
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		closed, err := w.st.CloseStalePhoneDeliveries(ctx)
		if err != nil || len(closed) != 1 {
			t.Fatalf("closed %d, %v; want the unlocked row closed without waiting for the locked one", len(closed), err)
		}
		if got := w.row(t, hi).reason; got != domain.PhoneReasonOutcomeUnknown {
			t.Fatalf("the unlocked row is %q; want outcome_unknown", got)
		}
		if err := blocker.Rollback(w.ctx); err != nil {
			t.Fatal(err)
		}
		if closed, err := w.st.CloseStalePhoneDeliveries(w.ctx); err != nil || len(closed) != 1 {
			t.Fatalf("next pass closed %d, %v; want the row it skipped", len(closed), err)
		}
		if got := w.row(t, lo).reason; got != domain.PhoneReasonOutcomeUnknown {
			t.Fatalf("the skipped row is %q after the next pass; want outcome_unknown", got)
		}
	})
	t.Run("attach", func(t *testing.T) {
		w := newPhoneDeliveryWorld(t)
		m := w.monitor(t, "attach-order")
		lo, hi := w.pdLockPair(t, m)
		blocker := w.lockRow(t, lo)
		var inc domain.Incident
		var incErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			inc, incErr = w.st.CreateIncidentBySystem(w.ctx, domain.Incident{ProjectID: w.project, MonitorID: m,
				Title: "down", Status: domain.IncidentInvestigating, Source: domain.SourceAuto}, "down", "system")
		}()
		w.waitRowWaiters(t, blocker)
		free := w.rowFree(t, hi)
		if err := blocker.Rollback(w.ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if !free {
			t.Fatal("the attachment locked the higher id before the lower one")
		}
		if incErr != nil {
			t.Fatal(incErr)
		}
		for _, id := range []string{lo, hi} {
			if got := w.row(t, id).incidentID; got != inc.ID {
				t.Fatalf("row %s incident %q; want %s", id, got, inc.ID)
			}
		}
	})
}

// FR-041a review P1-3 (§11 "Code review of the backend"): the leader's stale pass against a cascading
// delete of a channel, a project and an organization — no deadlock, the delete succeeds.
//
// The delete's transaction first holds the higher-id stale row, as a cascade that met it first in its
// scan does; the sweep then runs; then the delete cascades onto the lower-id row. A sweep that locked
// in id order and waited would hold the lower row and wait for the higher one while the cascade waits
// for the lower: 40P01, and Postgres aborts one side — the API's delete answering 500 when it is the
// victim. The sweep skips the held row instead and never waits.
func TestPhoneStaleSweepAgainstCascadingDelete(t *testing.T) {
	cases := []struct {
		name string
		del  func(w phoneDeliveryWorld) (string, []any)
	}{
		{"channel", func(w phoneDeliveryWorld) (string, []any) {
			return `DELETE FROM notification_channels WHERE id = $1`, []any{w.channel}
		}},
		{"project", func(w phoneDeliveryWorld) (string, []any) {
			return `DELETE FROM projects WHERE id = $1`, []any{w.project}
		}},
		{"organization", func(w phoneDeliveryWorld) (string, []any) {
			return `DELETE FROM organizations WHERE id = $1`, []any{w.org}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newPhoneDeliveryWorld(t)
			lo, hi := w.pdLockPair(t, w.monitor(t, "cascade-"+tc.name))
			del, err := w.st.pool.Begin(w.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer del.Rollback(w.ctx) //nolint:errcheck
			if _, err := del.Exec(w.ctx, `SELECT 1 FROM phone_deliveries WHERE id = $1 FOR UPDATE`, hi); err != nil {
				t.Fatal(err)
			}

			var closed []ClosedPhoneDelivery
			var sweepErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				closed, sweepErr = w.st.CloseStalePhoneDeliveries(w.ctx)
			}()
			w.doneOrWaitingOn(t, done, del)

			sql, args := tc.del(w)
			_, delErr := del.Exec(w.ctx, sql, args...)
			if delErr == nil {
				delErr = del.Commit(w.ctx)
			}
			<-done
			if sweepErr != nil {
				t.Fatalf("the sweep failed against the cascading delete: %v", sweepErr)
			}
			if delErr != nil {
				t.Fatalf("the cascading delete of the %s failed: %v", tc.name, delErr)
			}
			if len(closed) != 1 {
				t.Fatalf("the sweep closed %d row(s); want the one the delete did not hold", len(closed))
			}
			var left int
			if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_deliveries WHERE id = ANY($1::uuid[])`,
				[]string{lo, hi}).Scan(&left); err != nil || left != 0 {
				t.Fatalf("%d of the deleted %s's rows left, %v", left, tc.name, err)
			}
		})
	}
}

// doneOrWaitingOn blocks until done is closed or a transaction waits on a row tx holds.
func (w phoneDeliveryWorld) doneOrWaitingOn(t *testing.T, done <-chan struct{}, tx pgx.Tx) {
	t.Helper()
	var xid string
	if err := tx.QueryRow(w.ctx, `SELECT xid(pg_current_xact_id())::text`).Scan(&xid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-done:
			return
		default:
		}
		var got int
		if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_locks
			 WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text = $1`, xid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep neither finished nor waits on the delete")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FR-041a review P1-3: the 90-day purge deletes in batches and skips a row another transaction holds
// rather than waiting for it; the next pass takes it.
func TestPurgePhoneDeliveriesInBatchesSkipsLockedRows(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	prev := phonePurgeBatch
	phonePurgeBatch = 2
	t.Cleanup(func() { phonePurgeBatch = prev })
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}
	var ids []string
	for i := 0; i < 5; i++ {
		r := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
		w.age(t, r.ID, 91*24*time.Hour, 91*24*time.Hour)
		ids = append(ids, r.ID)
	}
	young := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	blocker := w.lockRow(t, ids[2])

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	n, err := w.st.PurgePhoneDeliveries(ctx, 0)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("the purge waited for a row another transaction holds")
	}
	if err != nil || n != 4 {
		t.Fatalf("purged %d, %v; want the 4 unlocked old rows over several batches", n, err)
	}
	if err := blocker.Rollback(w.ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := w.st.PurgePhoneDeliveries(w.ctx, 0); err != nil || n != 1 {
		t.Fatalf("next purge %d, %v; want the row it skipped", n, err)
	}
	w.row(t, young.ID)
}

// FR-041a review P1-3: a project's (and so an organization's) cascade finds its delivery rows through
// an index whose leading column is project_id, not a scan of the 90-day table.
func TestPhoneDeliveriesHaveAProjectIndex(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	var n int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'phone_deliveries' AND indexdef LIKE '%(project_id)'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d index(es) on phone_deliveries (project_id); want 1", n)
	}
}
