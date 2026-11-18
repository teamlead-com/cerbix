package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// FR-041a D33, §6.2, D32 — the phone_deliveries store: the reservation committed before a provider
// call, the storm guard counted inside it, and the attachment of a monitor's DOWN rows to the
// auto-incident that opens for that outage. DB-gated; the concurrency cases run real transactions
// on separate pool connections.

const (
	pdNumber = "+905551236869"
	pdOther  = "+447700900199"
)

type phoneDeliveryWorld struct {
	phoneChannelWorld
	channel string
}

func newPhoneDeliveryWorld(t *testing.T) phoneDeliveryWorld {
	t.Helper()
	w := newPhoneChannelWorld(t)
	ch := w.numberChannel(t, domain.ChannelSMS, pdNumber)
	return phoneDeliveryWorld{phoneChannelWorld: w, channel: ch.ID}
}

func (w phoneDeliveryWorld) monitor(t *testing.T, name string) string {
	t.Helper()
	m, err := w.st.CreateMonitor(w.ctx, domain.Monitor{
		RecoveryThreshold: 1, ProjectID: w.project, Name: name, Type: domain.MonitorHTTP,
		Target: "https://" + name + ".example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	return m.ID
}

func (w phoneDeliveryWorld) setSeq(t *testing.T, monitorID string, seq int64) {
	t.Helper()
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE monitors SET state_sequence = $2 WHERE id = $1`, monitorID, seq); err != nil {
		t.Fatal(err)
	}
}

func (w phoneDeliveryWorld) openAuto(t *testing.T, monitorID string) domain.Incident {
	t.Helper()
	inc, err := w.st.CreateIncidentBySystem(w.ctx, domain.Incident{
		ProjectID: w.project, MonitorID: monitorID, Title: "down",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}, "monitor reported down", "system")
	if err != nil {
		t.Fatalf("open auto incident: %v", err)
	}
	return inc
}

func (w phoneDeliveryWorld) uuid(t *testing.T) string {
	t.Helper()
	var id string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// in is a delivery of a fresh outbox event to the world's channel, with the instance's storm bounds
// set to storm first: the reservation reads them from instance_settings (§6.2).
func (w phoneDeliveryWorld) in(t *testing.T, alertKind string, storm domain.TelephonyStorm) domain.PhoneReserveInput {
	t.Helper()
	w.setStorm(t, storm)
	return domain.PhoneReserveInput{
		ProjectID: w.project, ChannelID: w.channel, OutboxEventID: w.uuid(t),
		Kind: domain.ChannelSMS, AlertKind: alertKind, Number: pdNumber,
	}
}

// setStorm stores the storm bounds in the telephony group as a settings save leaves them; a zero
// bound is stored as written and read as §6.2's default.
func (w phoneDeliveryWorld) setStorm(t *testing.T, storm domain.TelephonyStorm) {
	t.Helper()
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE instance_settings
		   SET telephony = jsonb_set(telephony, '{storm}', jsonb_build_object(
		       'per_incident_per_hour', $1::int, 'per_number_per_hour', $2::int))
		 WHERE id = true`, storm.PerIncidentPerHour, storm.PerNumberPerHour); err != nil {
		t.Fatal(err)
	}
}

func (w phoneDeliveryWorld) reserve(t *testing.T, in domain.PhoneReserveInput) domain.PhoneReservation {
	t.Helper()
	r, err := w.st.ReservePhoneDelivery(w.ctx, in)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}

// pdRow is a phone_deliveries row as the tests assert it.
type pdRow struct {
	reason, incidentID, numberHash, numberMasked string
	providerStatus                               *int
	providerCode, providerRef                    *string
	notes                                        []string
	segments, attempts                           int
	monitorSeq                                   *int64
	createdAt, updatedAt                         time.Time
}

func (w phoneDeliveryWorld) row(t *testing.T, id string) pdRow {
	t.Helper()
	var r pdRow
	if err := w.st.pool.QueryRow(w.ctx, `SELECT reason, COALESCE(incident_id::text, ''), COALESCE(number_hash, ''),
		       COALESCE(number_masked, ''), provider_status, provider_code, provider_ref, notes, segments,
		       attempts, monitor_seq, created_at, updated_at
		  FROM phone_deliveries WHERE id = $1`, id).Scan(&r.reason, &r.incidentID, &r.numberHash, &r.numberMasked,
		&r.providerStatus, &r.providerCode, &r.providerRef, &r.notes, &r.segments, &r.attempts, &r.monitorSeq,
		&r.createdAt, &r.updatedAt); err != nil {
		t.Fatalf("row %s: %v", id, err)
	}
	return r
}

func (w phoneDeliveryWorld) age(t *testing.T, id string, created, updated time.Duration) {
	t.Helper()
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE phone_deliveries
		   SET created_at = now() - make_interval(secs => $2), updated_at = now() - make_interval(secs => $3)
		 WHERE id = $1`, id, created.Seconds(), updated.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func (w phoneDeliveryWorld) finish(t *testing.T, id, reason string) {
	t.Helper()
	if err := w.st.FinishPhoneDelivery(w.ctx, id, domain.PhoneOutcome{Reason: reason, HTTPStatus: 503}); err != nil {
		t.Fatalf("finish %s: %v", reason, err)
	}
}

func countReasons(t *testing.T, rs []domain.PhoneReservation) map[domain.PhoneReservationStatus]int {
	t.Helper()
	out := map[domain.PhoneReservationStatus]int{}
	for _, r := range rs {
		out[r.Status]++
	}
	return out
}

// P13: N reservations to one number at once, each its own transaction on its own connection —
// exactly the allowed number proceed, per number and per incident.
func TestPhoneStormConcurrentReservations(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	inc := w.openAuto(t, w.monitor(t, "storm-concurrent"))

	cases := []struct {
		name    string
		kind    string
		incID   string
		storm   domain.TelephonyStorm
		allowed int
	}{
		{"per number", domain.PhoneAlertMonitorBurn, "", domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 4}, 4},
		{"per incident", domain.PhoneAlertEscalationStep, inc.ID, domain.TelephonyStorm{PerIncidentPerHour: 3, PerNumberPerHour: 100}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM phone_deliveries`); err != nil {
				t.Fatal(err)
			}
			const n = 16
			ins := make([]domain.PhoneReserveInput, n)
			for i := range ins {
				ins[i] = w.in(t, c.kind, c.storm)
				ins[i].IncidentID = c.incID
			}
			out := make([]domain.PhoneReservation, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range ins {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					out[i], errs[i] = w.st.ReservePhoneDelivery(w.ctx, ins[i])
				}(i)
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("reservation %d: %v", i, err)
				}
			}
			got := countReasons(t, out)
			if got[domain.PhoneReservationProceed] != c.allowed || got[domain.PhoneReservationStormLimited] != n-c.allowed {
				t.Fatalf("statuses = %v; want %d proceed, %d storm_limited", got, c.allowed, n-c.allowed)
			}
			var sending, limited int
			if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FILTER (WHERE reason = 'sending'),
				count(*) FILTER (WHERE reason = 'storm_limited') FROM phone_deliveries`).Scan(&sending, &limited); err != nil {
				t.Fatal(err)
			}
			if sending != c.allowed || limited != n-c.allowed {
				t.Fatalf("rows: %d sending, %d storm_limited; want %d, %d", sending, limited, c.allowed, n-c.allowed)
			}
		})
	}
}

// §6.2: the per-incident and per-number bounds, which reasons hold a slot, the hour, another number.
func TestPhoneStormLimits(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "storm-limits")
	incA := w.openAuto(t, m)
	incB := openManualFixture(t, w.st, w.ctx, domain.Incident{ProjectID: w.project, Title: "b",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual})
	storm := domain.TelephonyStorm{PerIncidentPerHour: 2, PerNumberPerHour: 4}
	step := func(incID string) domain.PhoneReservation {
		in := w.in(t, domain.PhoneAlertEscalationStep, storm)
		in.IncidentID = incID
		return w.reserve(t, in)
	}
	want := func(r domain.PhoneReservation, s domain.PhoneReservationStatus) {
		t.Helper()
		if r.Status != s {
			t.Fatalf("status %s; want %s", r.Status, s)
		}
		if row := w.row(t, r.ID); row.reason != r.Reason {
			t.Fatalf("row reason %s; reservation says %s", row.reason, r.Reason)
		}
	}

	a1 := step(incA.ID)
	want(a1, domain.PhoneReservationProceed)
	want(step(incA.ID), domain.PhoneReservationProceed)
	want(step(incA.ID), domain.PhoneReservationStormLimited) // per incident
	want(step(incB.ID), domain.PhoneReservationProceed)      // another incident, same number
	b2 := step(incB.ID)
	want(b2, domain.PhoneReservationProceed)
	want(step(incB.ID), domain.PhoneReservationStormLimited)
	// Four slots held on the number: a third incident and a burn alert are refused per number.
	incC := openManualFixture(t, w.st, w.ctx, domain.Incident{ProjectID: w.project, Title: "c",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual})
	want(step(incC.ID), domain.PhoneReservationStormLimited)
	want(w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm)), domain.PhoneReservationStormLimited)
	// Another number is untouched.
	other := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	other.Number = pdOther
	want(w.reserve(t, other), domain.PhoneReservationProceed)

	// A call that ended provider_unreachable frees its slot; sent, rejected and outcome_unknown hold.
	w.finish(t, a1.ID, domain.PhoneReasonProviderUnreachable)
	want(step(incC.ID), domain.PhoneReservationProceed)
	want(step(incC.ID), domain.PhoneReservationStormLimited) // per number again (4 held)
	w.finish(t, b2.ID, domain.PhoneReasonProviderRejected)
	want(step(incC.ID), domain.PhoneReservationStormLimited)

	// Rows older than an hour no longer count: with every slot aged out, the number is free again.
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE phone_deliveries SET created_at = now() - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	want(step(incA.ID), domain.PhoneReservationProceed)

	// The zero bounds are §6.2's defaults (5 per incident).
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM phone_deliveries`); err != nil {
		t.Fatal(err)
	}
	storm = domain.TelephonyStorm{}
	for i := 0; i < 5; i++ {
		want(step(incA.ID), domain.PhoneReservationProceed)
	}
	want(step(incA.ID), domain.PhoneReservationStormLimited)
}

// D35: the guard counts rows hashed under every configured key, so a rotation does not reset it.
func TestPhoneStormCountsEveryKey(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	oldKey := randKey(t)
	before, err := secret.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	w.st.WithCipher(before)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 2}
	old := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))

	// Restarted with a new primary key and the old one in previous_keys, before `cerbix reencrypt`.
	rotated, err := secret.New(randKey(t), oldKey)
	if err != nil {
		t.Fatal(err)
	}
	w.st.WithCipher(rotated)
	r := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	if r.Status != domain.PhoneReservationStormLimited {
		t.Fatalf("after rotation: %s; want storm_limited (old-key rows count)", r.Status)
	}
	if w.row(t, r.ID).numberHash == w.row(t, old.ID).numberHash {
		t.Fatal("a new row is hashed under the new primary key")
	}
}

// §6.2: a monitor DOWN without its incident yet is counted against the monitor; a burn alert of the
// same monitor and another monitor's rows are not; once the incident opens, against the incident.
func TestPhoneStormMonitorAsSubject(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "storm-subject")
	other := w.monitor(t, "storm-other")
	w.setSeq(t, m, 3)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 2, PerNumberPerHour: 100}
	down := func(monitorID string) domain.PhoneReservation {
		in := w.in(t, domain.PhoneAlertMonitorDown, storm)
		in.MonitorID, in.MonitorSeq = monitorID, 3
		return w.reserve(t, in)
	}
	burn := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	burn.MonitorID = m
	br := w.reserve(t, burn)
	w.finish(t, br.ID, domain.PhoneReasonSent)
	if r := w.reserve(t, burn); r.Status != domain.PhoneReservationExisting { // not a second row
		t.Fatalf("the burn event again: %s", r.Status)
	}
	if r := down(other); r.Status != domain.PhoneReservationProceed {
		t.Fatal("another monitor's row")
	}

	first := down(m)
	if first.Status != domain.PhoneReservationProceed || first.IncidentID != "" {
		t.Fatalf("first DOWN: %+v", first)
	}
	if r := down(m); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("second DOWN: %s", r.Status)
	}
	if r := down(m); r.Status != domain.PhoneReservationStormLimited {
		t.Fatalf("third DOWN: %s; want storm_limited against the monitor", r.Status)
	}

	// The incident opens: D32 attaches the three DOWN rows; the next DOWN finds it and is counted
	// against it (two slots held, one storm-limited row holds none).
	inc := w.openAuto(t, m)
	r := down(m)
	if r.IncidentID != inc.ID || r.Status != domain.PhoneReservationStormLimited {
		t.Fatalf("DOWN after opening: %+v; want storm_limited on incident %s", r, inc.ID)
	}
	if got := w.row(t, first.ID).incidentID; got != inc.ID {
		t.Fatalf("first row incident %q; want %s", got, inc.ID)
	}
}

// FR-041a review P2-7, §6.2: a service's health or recovery alert whose incident is not found is
// counted against the SERVICE, as a monitor's is against the monitor — named storm_per_incident. A
// service burn alert has no such subject: the number's limit alone.
func TestPhoneStormServiceAsSubject(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	svc, other := w.uuid(t), w.uuid(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 2, PerNumberPerHour: 100}
	alert := func(kind, serviceID string) domain.PhoneReservation {
		in := w.in(t, kind, storm)
		in.ServiceID = serviceID
		return w.reserve(t, in)
	}
	if r := alert(domain.PhoneAlertServiceHealth, other); r.Status != domain.PhoneReservationProceed {
		t.Fatal("another service's row")
	}
	if r := alert(domain.PhoneAlertServiceHealth, svc); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("first onset: %s", r.Status)
	}
	if r := alert(domain.PhoneAlertServiceRecovered, svc); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("recovery: %s", r.Status)
	}
	limited := alert(domain.PhoneAlertServiceHealth, svc)
	if limited.Status != domain.PhoneReservationStormLimited {
		t.Fatalf("third alert of the service: %s; want storm_limited against the service", limited.Status)
	}
	if notes := w.row(t, limited.ID).notes; !reflect.DeepEqual(notes, []string{domain.PhoneNoteStormPerIncident}) {
		t.Fatalf("notes = %v; want the per-incident limit named", notes)
	}
	// A burn alert of the same service is limited by the number alone, and holds no service slot.
	if r := alert(domain.PhoneAlertServiceBurn, svc); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("service burn: %s; want the number's limit alone", r.Status)
	}
}

// D33: a redelivered event meets its row. Young `sending` → in flight; stale → outcome_unknown;
// final → nothing; provider_unreachable → the same row re-armed.
func TestPhoneReservationRetries(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}

	in := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	r := w.reserve(t, in)
	if r.Status != domain.PhoneReservationProceed || r.Reason != domain.PhoneReasonSending || r.Attempts != 1 {
		t.Fatalf("first reservation: %+v", r)
	}
	for _, at := range []time.Duration{0, 10 * time.Second, 30 * time.Second} {
		w.age(t, r.ID, at, at)
		if _, err := w.st.ReservePhoneDelivery(w.ctx, in); !errors.Is(err, domain.ErrPhoneDeliveryInFlight) {
			t.Fatalf("retry at +%s: %v; want ErrPhoneDeliveryInFlight", at, err)
		}
		if got := w.row(t, r.ID).reason; got != domain.PhoneReasonSending {
			t.Fatalf("retry at +%s changed the row to %s", at, got)
		}
	}
	w.age(t, r.ID, 70*time.Second, 70*time.Second)
	closed := w.reserve(t, in)
	if closed.Status != domain.PhoneReservationClosedStale || closed.ID != r.ID || closed.Reason != domain.PhoneReasonOutcomeUnknown {
		t.Fatalf("retry at +70s: %+v; want the row closed as outcome_unknown", closed)
	}
	again := w.reserve(t, in)
	if again.Status != domain.PhoneReservationExisting || again.Reason != domain.PhoneReasonOutcomeUnknown {
		t.Fatalf("retry of a closed row: %+v", again)
	}

	// A final row is left alone.
	sent := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	sr := w.reserve(t, sent)
	w.finish(t, sr.ID, domain.PhoneReasonSent)
	if got := w.reserve(t, sent); got.Status != domain.PhoneReservationExisting || got.Reason != domain.PhoneReasonSent || got.ID != sr.ID {
		t.Fatalf("retry of a sent row: %+v", got)
	}

	// provider_unreachable: the same row back to sending, attempts grown, updated_at now — and the
	// storm re-check excludes the row itself (a bound of one, held by nobody else, still proceeds).
	tight := domain.TelephonyStorm{PerIncidentPerHour: 1, PerNumberPerHour: 1}
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM phone_deliveries`); err != nil {
		t.Fatal(err)
	}
	un := w.in(t, domain.PhoneAlertMonitorBurn, tight)
	ur := w.reserve(t, un)
	// The first attempt's answer: a status, a code and a reference, none of which belongs to the next.
	if err := w.st.FinishPhoneDelivery(w.ctx, ur.ID, domain.PhoneOutcome{Reason: domain.PhoneReasonProviderUnreachable,
		HTTPStatus: 503, Code: "20429", Ref: "SMfirstattempt"}); err != nil {
		t.Fatal(err)
	}
	w.age(t, ur.ID, 40*time.Second, 40*time.Second)
	re := w.reserve(t, un)
	if re.Status != domain.PhoneReservationProceed || re.ID != ur.ID || re.Attempts != 2 || re.Reason != domain.PhoneReasonSending {
		t.Fatalf("re-armed: %+v", re)
	}
	row := w.row(t, ur.ID)
	if row.reason != domain.PhoneReasonSending || row.attempts != 2 || time.Since(row.updatedAt) > 5*time.Second {
		t.Fatalf("re-armed row: %+v", row)
	}
	// A re-armed row has not been answered yet: the previous attempt's answer is cleared, so a row
	// later closed as outcome_unknown never shows a status beside it (Task 10 review, finding 4).
	if row.providerStatus != nil || row.providerCode != nil || row.providerRef != nil {
		t.Fatalf("re-armed row keeps the previous answer: status %v, code %v, ref %v",
			row.providerStatus, row.providerCode, row.providerRef)
	}
	var n int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_deliveries`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want the one row", n, err)
	}

	// Re-armed while the slots are full of other rows: storm_limited, on the same row.
	w.finish(t, ur.ID, domain.PhoneReasonProviderUnreachable)
	w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, tight)) // takes the only slot
	lim := w.reserve(t, un)
	if lim.Status != domain.PhoneReservationStormLimited || lim.ID != ur.ID {
		t.Fatalf("re-arm under a full storm: %+v", lim)
	}
}

// FR-041a review P2-6: a retry of an event whose attempt ended provider_unreachable writes THIS
// attempt's person, number and notes into the row — a `user` channel retargeted meanwhile, an incident
// found since — and a retry that finds the channel no longer a route turns the row not_deliverable with
// this attempt's facts and no provider answer.
func TestRetriedPhoneDeliveryCarriesThisAttempt(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}
	who := func(id string) (userID string) {
		t.Helper()
		if err := w.st.pool.QueryRow(w.ctx, `SELECT COALESCE(user_id::text, '') FROM phone_deliveries WHERE id = $1`,
			id).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		return userID
	}
	hashOf := func(number string) string {
		t.Helper()
		_, hashes, err := w.st.phoneNumberHashes(number)
		if err != nil {
			t.Fatal(err)
		}
		return hashes[0]
	}
	u1, u2, u3 := w.uuid(t), w.uuid(t), w.uuid(t)

	first := w.in(t, domain.PhoneAlertServiceHealth, storm)
	first.UserID, first.Notes = u1, []string{domain.PhoneNoteNoIncidentRef}
	r := w.reserve(t, first)
	if err := w.st.FinishPhoneDelivery(w.ctx, r.ID, domain.PhoneOutcome{Reason: domain.PhoneReasonProviderUnreachable,
		HTTPStatus: 503, Code: "20429", Ref: "SMfirst"}); err != nil {
		t.Fatal(err)
	}

	second := first
	second.UserID, second.Number, second.Notes = u2, pdOther, []string{domain.PhoneNoteLinkOmitted}
	if re := w.reserve(t, second); re.Status != domain.PhoneReservationProceed || re.ID != r.ID {
		t.Fatalf("re-armed: %+v", re)
	}
	row := w.row(t, r.ID)
	if got := who(r.ID); got != u2 {
		t.Errorf("user_id = %s; want the second attempt's %s", got, u2)
	}
	if row.numberHash != hashOf(pdOther) || row.numberMasked != domain.MaskPhone(pdOther) {
		t.Errorf("number = %s %s; want the second attempt's", row.numberHash, row.numberMasked)
	}
	if !reflect.DeepEqual(row.notes, []string{domain.PhoneNoteLinkOmitted}) {
		t.Errorf("notes = %v; want the second attempt's alone", row.notes)
	}

	// The second attempt is unreachable too; the third finds the channel no route.
	if err := w.st.FinishPhoneDelivery(w.ctx, r.ID, domain.PhoneOutcome{Reason: domain.PhoneReasonProviderUnreachable,
		HTTPStatus: 502, Code: "30001", Ref: "SMsecond"}); err != nil {
		t.Fatal(err)
	}
	third := first
	third.UserID, third.Number, third.Notes = u3, "", []string{domain.PhoneNoteNoVerifiedNumber}
	if ok, reason, err := w.st.RecordUnsentPhoneDelivery(w.ctx, third, domain.PhoneReasonNotDeliverable); err != nil || !ok ||
		reason != domain.PhoneReasonNotDeliverable {
		t.Fatalf("unsent over unreachable: %v %q %v", ok, reason, err)
	}
	row = w.row(t, r.ID)
	if got := who(r.ID); got != u3 {
		t.Errorf("not_deliverable user_id = %s; want the third attempt's %s", got, u3)
	}
	if row.numberHash != "" || row.numberMasked != "" {
		t.Errorf("not_deliverable number = %q %q; want none, as the third attempt had", row.numberHash, row.numberMasked)
	}
	if !reflect.DeepEqual(row.notes, []string{domain.PhoneNoteNoVerifiedNumber}) {
		t.Errorf("not_deliverable notes = %v; want the third attempt's alone", row.notes)
	}
	if row.providerStatus != nil || row.providerCode != nil || row.providerRef != nil {
		t.Errorf("not_deliverable keeps a provider answer: %v %v %v", row.providerStatus, row.providerCode, row.providerRef)
	}
}

// D33: the leader's pass closes an orphaned `sending` row by updated_at, not by created_at, so a row
// on its fourth attempt whose call is in flight is left alone.
func TestCloseStalePhoneDeliveries(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}

	orphan := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	w.age(t, orphan.ID, 2*time.Minute, 2*time.Minute)

	fourth := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	fr := w.reserve(t, fourth)
	for i := 0; i < 3; i++ {
		w.finish(t, fr.ID, domain.PhoneReasonProviderUnreachable)
		w.age(t, fr.ID, 3*time.Minute, 3*time.Minute)
		fr = w.reserve(t, fourth)
	}
	if fr.Attempts != 4 {
		t.Fatalf("attempts %d; want 4", fr.Attempts)
	}

	sent := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	w.finish(t, sent.ID, domain.PhoneReasonSent)
	w.age(t, sent.ID, 2*time.Minute, 2*time.Minute)

	closed, err := w.st.CloseStalePhoneDeliveries(w.ctx)
	// Each row closed comes back with its channel kind and the provider in force: the labels the
	// leader counts it under in cerbix_phone_deliveries_total{reason="outcome_unknown"} (P7).
	want := []ClosedPhoneDelivery{{Kind: domain.ChannelSMS, Provider: domain.TelephonyProviderTwilio}}
	if err != nil || !reflect.DeepEqual(closed, want) {
		t.Fatalf("closed %+v, %v; want %+v", closed, err, want)
	}
	if got := w.row(t, orphan.ID).reason; got != domain.PhoneReasonOutcomeUnknown {
		t.Fatalf("orphan: %s", got)
	}
	if got := w.row(t, fr.ID).reason; got != domain.PhoneReasonSending {
		t.Fatalf("fourth attempt in flight: %s; want sending", got)
	}
	if got := w.row(t, sent.ID).reason; got != domain.PhoneReasonSent {
		t.Fatalf("sent row: %s", got)
	}

	// No provider stored: the provider is "" (the leader labels it `none`, as the worker does).
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE instance_settings SET telephony_provider = NULL WHERE id = true`); err != nil {
		t.Fatal(err)
	}
	none := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	w.age(t, none.ID, 2*time.Minute, 2*time.Minute)
	closed, err = w.st.CloseStalePhoneDeliveries(w.ctx)
	if want := []ClosedPhoneDelivery{{Kind: domain.ChannelSMS}}; err != nil || !reflect.DeepEqual(closed, want) {
		t.Fatalf("closed without a provider %+v, %v; want %+v", closed, err, want)
	}
	if closed, err = w.st.CloseStalePhoneDeliveries(w.ctx); err != nil || len(closed) != 0 {
		t.Fatalf("a second pass closed %+v, %v; want nothing", closed, err)
	}
}

// D34 / Task 5: the provider's empty code and reference are stored as NULL (the column refuses ”).
func TestFinishPhoneDelivery(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}

	in := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	in.Notes = []string{domain.PhoneNoteNoPublicURL}
	r := w.reserve(t, in)
	if err := w.st.FinishPhoneDelivery(w.ctx, r.ID, domain.PhoneOutcome{
		Reason: domain.PhoneReasonSent, HTTPStatus: 201, Segments: 2,
		Notes: []string{domain.PhoneNoteLinkOmitted, domain.PhoneNoteNoPublicURL},
	}); err != nil {
		t.Fatalf("finish sent with empty code: %v", err)
	}
	row := w.row(t, r.ID)
	if row.reason != domain.PhoneReasonSent || row.providerStatus == nil || *row.providerStatus != 201 ||
		row.providerCode != nil || row.providerRef != nil || row.segments != 2 {
		t.Fatalf("sent row: %+v", row)
	}
	if len(row.notes) != 2 || row.notes[0] != domain.PhoneNoteNoPublicURL || row.notes[1] != domain.PhoneNoteLinkOmitted {
		t.Fatalf("notes %v; want [no_public_url link_omitted]", row.notes)
	}
	if row.numberMasked != "+90•••••6869" || row.numberHash == "" {
		t.Fatalf("number columns: %q %q", row.numberHash, row.numberMasked)
	}

	// A transport failure: no HTTP status, no code.
	r2 := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	if err := w.st.FinishPhoneDelivery(w.ctx, r2.ID, domain.PhoneOutcome{Reason: domain.PhoneReasonProviderUnreachable}); err != nil {
		t.Fatal(err)
	}
	if row := w.row(t, r2.ID); row.providerStatus != nil || row.providerCode != nil {
		t.Fatalf("unreachable row: %+v", row)
	}
	// A rejection keeps its code and reference.
	r3 := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	if err := w.st.FinishPhoneDelivery(w.ctx, r3.ID, domain.PhoneOutcome{
		Reason: domain.PhoneReasonProviderRejected, HTTPStatus: 400, Code: "21211", Ref: "SM0123"}); err != nil {
		t.Fatal(err)
	}
	if row := w.row(t, r3.ID); *row.providerCode != "21211" || *row.providerRef != "SM0123" {
		t.Fatalf("rejected row: %+v", row)
	}

	// Only a `sending` row is finished; a reason outside the three is refused.
	// ErrNotFound for the store's callers, domain.ErrPhoneDeliveryClosed for notify, which cannot
	// import the store: "closed meanwhile", final, not an event failure.
	if err := w.st.FinishPhoneDelivery(w.ctx, r.ID, domain.PhoneOutcome{Reason: domain.PhoneReasonSent}); !errors.Is(err, ErrNotFound) ||
		!errors.Is(err, domain.ErrPhoneDeliveryClosed) {
		t.Fatalf("finish a final row: %v; want ErrNotFound and domain.ErrPhoneDeliveryClosed", err)
	}
	r4 := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	if err := w.st.FinishPhoneDelivery(w.ctx, r4.ID, domain.PhoneOutcome{Reason: domain.PhoneReasonStormLimited}); err == nil {
		t.Fatal("finish with storm_limited accepted")
	}
}

// §4.5: a channel that is not a route at send gets its final row, once per event.
func TestRecordUnsentPhoneDelivery(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	in := w.in(t, domain.PhoneAlertEscalationStep, domain.TelephonyStorm{})
	in.Number = "" // R9: a user without a verified number
	in.Notes = []string{domain.PhoneNoteNoVerifiedNumber}
	ok, rowReason, err := w.st.RecordUnsentPhoneDelivery(w.ctx, in, domain.PhoneReasonNotDeliverable)
	if err != nil || !ok || rowReason != domain.PhoneReasonNotDeliverable {
		t.Fatalf("record unsent: %v, %q, %v", ok, rowReason, err)
	}
	ok, rowReason, err = w.st.RecordUnsentPhoneDelivery(w.ctx, in, domain.PhoneReasonNotDeliverable)
	if err != nil || ok || rowReason != domain.PhoneReasonNotDeliverable {
		t.Fatalf("second record of the same event: %v, %q, %v; want false and the existing row's reason", ok, rowReason, err)
	}
	var id string
	var n int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT min(id::text), count(*) FROM phone_deliveries WHERE outbox_event_id = $1`,
		in.OutboxEventID).Scan(&id, &n); err != nil || n != 1 {
		t.Fatalf("rows %d, %v", n, err)
	}
	row := w.row(t, id)
	if row.reason != domain.PhoneReasonNotDeliverable || row.numberHash != "" || row.numberMasked != "" ||
		len(row.notes) != 1 || row.notes[0] != domain.PhoneNoteNoVerifiedNumber {
		t.Fatalf("unsent row: %+v", row)
	}

	// With a number, the hash and mask are kept; telephony_disabled and voice_unsupported are accepted.
	for _, reason := range []string{domain.PhoneReasonTelephonyDisabled, domain.PhoneReasonVoiceUnsupportedByProvider} {
		in := w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{})
		if ok, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, in, reason); err != nil || !ok {
			t.Fatalf("%s: %v, %v", reason, ok, err)
		}
	}
	// A monitor DOWN without its monitor cannot take D32's lock: refused on both writers.
	noMonitor := w.in(t, domain.PhoneAlertMonitorDown, domain.TelephonyStorm{})
	if _, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, noMonitor, domain.PhoneReasonTelephonyDisabled); err == nil {
		t.Fatal("unsent monitor DOWN without a monitor accepted")
	}
	if _, err := w.st.ReservePhoneDelivery(w.ctx, noMonitor); err == nil {
		t.Fatal("monitor DOWN reservation without a monitor accepted")
	}
	// A reason that is a provider outcome or the guard's is refused.
	for _, reason := range []string{domain.PhoneReasonSent, domain.PhoneReasonStormLimited, "nope"} {
		if _, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{}), reason); err == nil {
			t.Fatalf("%s accepted", reason)
		}
	}

	// A retry that finds the channel no longer a route settles an unreachable row instead of leaving
	// it retryable for ever; a final row is left as it is.
	un := w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{})
	ur := w.reserve(t, un)
	w.finish(t, ur.ID, domain.PhoneReasonProviderUnreachable)
	if ok, rowReason, err := w.st.RecordUnsentPhoneDelivery(w.ctx, un, domain.PhoneReasonTelephonyDisabled); err != nil || !ok ||
		rowReason != domain.PhoneReasonTelephonyDisabled {
		t.Fatalf("unsent over unreachable: %v, %q, %v", ok, rowReason, err)
	}
	if got := w.row(t, ur.ID).reason; got != domain.PhoneReasonTelephonyDisabled {
		t.Fatalf("unreachable row now %s", got)
	}
	sent := w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{})
	sr := w.reserve(t, sent)
	w.finish(t, sr.ID, domain.PhoneReasonSent)
	// The caller learns that the person was reached on an earlier attempt (Task 11 review, finding 3).
	if ok, rowReason, err := w.st.RecordUnsentPhoneDelivery(w.ctx, sent, domain.PhoneReasonTelephonyDisabled); err != nil || ok ||
		rowReason != domain.PhoneReasonSent {
		t.Fatalf("unsent over sent: %v, %q, %v; want false and %q", ok, rowReason, err, domain.PhoneReasonSent)
	}
	if got := w.row(t, sr.ID).reason; got != domain.PhoneReasonSent {
		t.Fatalf("sent row now %s", got)
	}
}

// FR-041a review P3-4: a delivery to a channel deleted after its targets were read — the channel, or
// its project with it — finds the channel gone: both writers answer domain.ErrPhoneChannelGone, not
// the foreign key's violation, so the caller can end that channel's delivery quietly.
func TestPhoneDeliveryToADeletedChannel(t *testing.T) {
	for _, del := range []string{"channel", "project"} {
		t.Run(del, func(t *testing.T) {
			w := newPhoneDeliveryWorld(t)
			reserve := w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100})
			unsent := w.in(t, domain.PhoneAlertMonitorBurn, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100})
			if del == "channel" {
				exec(t, w.st, w.ctx, `DELETE FROM notification_channels WHERE id = $1`, w.channel)
			} else {
				exec(t, w.st, w.ctx, `DELETE FROM projects WHERE id = $1`, w.project)
			}
			if _, err := w.st.ReservePhoneDelivery(w.ctx, reserve); !errors.Is(err, domain.ErrPhoneChannelGone) {
				t.Fatalf("reserve: %v; want ErrPhoneChannelGone", err)
			}
			if _, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, unsent, domain.PhoneReasonTelephonyDisabled); !errors.Is(err, domain.ErrPhoneChannelGone) {
				t.Fatalf("record unsent: %v; want ErrPhoneChannelGone", err)
			}
			var n int
			if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_deliveries`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("rows %d, %v; want none", n, err)
			}
		})
	}
}

// waitAdvisoryWaiters blocks until n transactions wait on monitorID's D32 lock — that lock only, by
// its class and key, so a waiter on any other advisory lock in the database (another test, another
// package) cannot end the wait early.
func (w phoneDeliveryWorld) waitAdvisoryWaiters(t *testing.T, monitorID string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
			   AND classid = hashtext('phone_delivery_monitor')::oid
			   AND objid = hashtext($1::uuid::text)::oid`, monitorID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d advisory waiters; want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// D32 / P21: a monitor DOWN reservation and the opening of that monitor's auto-incident run as two
// concurrent transactions, in both orders. Under READ COMMITTED neither sees the other's
// uncommitted work; the shared monitor lock serialises them, and the row ends with the incident.
//
// The order is forced: a third transaction holds the monitor's lock while the first contender, then
// the second, queue behind it; releasing it lets them run in queue order, each still concurrent with
// the other's start.
func TestPhoneDeliveryAttachRace(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}

	for _, reservationFirst := range []bool{true, false} {
		name := "incident first"
		if reservationFirst {
			name = "reservation first"
		}
		t.Run(name, func(t *testing.T) {
			m := w.monitor(t, "race-"+map[bool]string{true: "r", false: "i"}[reservationFirst])
			w.setSeq(t, m, 5)
			in := w.in(t, domain.PhoneAlertMonitorDown, storm)
			in.MonitorID, in.MonitorSeq = m, 5

			blocker, err := w.st.pool.Begin(w.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(w.ctx) //nolint:errcheck
			if err := lockPhoneMonitorTx(w.ctx, blocker, m); err != nil {
				t.Fatal(err)
			}

			var res domain.PhoneReservation
			var inc domain.Incident
			var resErr, incErr error
			var wg sync.WaitGroup
			reserve := func() {
				defer wg.Done()
				res, resErr = w.st.ReservePhoneDelivery(w.ctx, in)
			}
			open := func() {
				defer wg.Done()
				inc, incErr = w.st.CreateIncidentBySystem(w.ctx, domain.Incident{ProjectID: w.project, MonitorID: m,
					Title: "down", Status: domain.IncidentInvestigating, Source: domain.SourceAuto}, "down", "system")
			}
			first, second := open, reserve
			if reservationFirst {
				first, second = reserve, open
			}
			wg.Add(2)
			go first()
			w.waitAdvisoryWaiters(t, m, 1)
			go second()
			w.waitAdvisoryWaiters(t, m, 2)
			if err := blocker.Rollback(w.ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if resErr != nil || incErr != nil {
				t.Fatalf("reserve: %v; open: %v", resErr, incErr)
			}
			if got := w.row(t, res.ID).incidentID; got != inc.ID {
				t.Fatalf("row incident %q; want %s", got, inc.ID)
			}
			if !reservationFirst && res.IncidentID != inc.ID {
				t.Fatalf("reservation after the opening returned incident %q; want %s", res.IncidentID, inc.ID)
			}
		})
	}
}

// D32: what the opening must NOT attach — a row of an earlier outage (older seq), a burn alert of the
// same monitor, another monitor's DOWN; and only the system door attaches (a manual incident on the
// monitor does not).
func TestPhoneDeliveryAttachScope(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}
	m := w.monitor(t, "attach-scope")
	other := w.monitor(t, "attach-other")
	reserveKind := func(monitorID, kind string, seq int64) string {
		in := w.in(t, kind, storm)
		in.MonitorID, in.MonitorSeq = monitorID, seq
		return w.reserve(t, in).ID
	}

	w.setSeq(t, m, 4)
	earlier := reserveKind(m, domain.PhoneAlertMonitorDown, 4)
	w.setSeq(t, m, 6) // the outage ended (5) and a new one began (6)
	w.setSeq(t, other, 6)
	current := reserveKind(m, domain.PhoneAlertMonitorDown, 6)
	burn := reserveKind(m, domain.PhoneAlertMonitorBurn, 6)
	otherDown := reserveKind(other, domain.PhoneAlertMonitorDown, 6)
	unsent := w.in(t, domain.PhoneAlertMonitorDown, storm)
	unsent.MonitorID, unsent.MonitorSeq, unsent.Number = m, 6, ""
	if _, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, unsent, domain.PhoneReasonNotDeliverable); err != nil {
		t.Fatal(err)
	}

	// A manual incident anchored to the monitor attaches nothing.
	openManualFixture(t, w.st, w.ctx, domain.Incident{ProjectID: w.project, MonitorID: m, Title: "manual",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual})
	if got := w.row(t, current).incidentID; got != "" {
		t.Fatalf("a manual incident attached the row: %s", got)
	}

	inc := w.openAuto(t, m)
	if got := w.row(t, current).incidentID; got != inc.ID {
		t.Fatalf("current outage's row: %q; want %s", got, inc.ID)
	}
	var unsentInc string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT COALESCE(incident_id::text, '') FROM phone_deliveries
		WHERE outbox_event_id = $1`, unsent.OutboxEventID).Scan(&unsentInc); err != nil || unsentInc != inc.ID {
		t.Fatalf("unsent DOWN row of the outage: %q, %v; want %s", unsentInc, err, inc.ID)
	}
	for name, id := range map[string]string{"earlier outage": earlier, "burn alert": burn, "other monitor": otherDown} {
		if got := w.row(t, id).incidentID; got != "" {
			t.Fatalf("%s attached to %s", name, got)
		}
	}
	if seq := w.row(t, current).monitorSeq; seq == nil || *seq != 6 {
		t.Fatalf("monitor_seq %v; want 6", seq)
	}
}

// The retention passes (Task 14 schedules them): deliveries after 90 days, verification codes after
// 24 hours by default; younger rows stay.
func TestPurgePhoneRows(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}
	oldRow := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	w.age(t, oldRow.ID, 91*24*time.Hour, 91*24*time.Hour)
	young := w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm))
	n, err := w.st.PurgePhoneDeliveries(w.ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v; want 1", n, err)
	}
	w.row(t, young.ID)

	// Codes sent to the world's number channel: fresh ones stay, a day-old one goes.
	for i := 0; i < 2; i++ {
		if _, _, err := w.st.SendChannelVerificationCode(w.ctx, w.channel, w.admin); err != nil {
			t.Fatal(err)
		}
	}
	var codes int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_verifications`).Scan(&codes); err != nil || codes == 0 {
		t.Fatalf("verification rows %d, %v", codes, err)
	}
	if n, err := w.st.PurgePhoneVerifications(w.ctx, 0); err != nil || n != 0 {
		t.Fatalf("purged fresh codes: %d, %v", n, err)
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE phone_verifications SET created_at = now() - interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	if n, err := w.st.PurgePhoneVerifications(w.ctx, 0); err != nil || n != codes {
		t.Fatalf("purged %d, %v; want %d", n, err, codes)
	}
}
