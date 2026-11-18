package store

import (
	"io"
	"log/slog"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a §11 "Outbox — reasons", on PostgreSQL: notify.DeliverPhone over the real store — the rule-1
// verdict and the verified numbers read by PhoneTargets, the reservation committed before the (fake)
// provider is called, the answer stored — and a retry of the same event calling nobody again (D19).

type fixedTelephony struct {
	ts       domain.TelephonySettings
	provider telephony.Provider
}

func (f fixedTelephony) Settings() domain.TelephonySettings    { return f.ts }
func (f fixedTelephony) Provider() (telephony.Provider, error) { return f.provider, nil }

func TestDeliverPhoneOnPostgres(t *testing.T) {
	w := newPhoneChannelWorld(t)
	duty := w.verifyNumberChannel(t, w.numberChannel(t, domain.ChannelSMS, "+447700900123").ID)
	if _, err := w.patch(duty.ID, PhoneChannelPatch{Enabled: ptr(true)}); err != nil { // created disabled (§4.2)
		t.Fatal(err)
	}
	a := w.member(t, "alice@example.com", domain.RoleViewer)
	w.verifiedPhone(t, a, "+905551110001")
	aliceVoice := w.userChannel(t, domain.ChannelVoice, a)
	c := w.member(t, "carol@example.com", domain.RoleViewer)
	carolSMS := w.userChannel(t, domain.ChannelSMS, c) // no number at all

	ts := domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
	ts.Normalize()
	fake := telephony.NewFakeProvider()
	d := notify.New(w.st, nil).WithPhone(w.st, fixedTelephony{ts: ts, provider: fake}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	var event string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT gen_random_uuid()::text`).Scan(&event); err != nil {
		t.Fatal(err)
	}
	alert := telephony.PhoneAlert{Kind: telephony.KindEscalationStep, Step: 1, SubjectName: "payments-api", ProjectName: "API"}
	ids := []string{duty.ID, aliceVoice.ID, carolSMS.ID}
	ev := domain.PhoneEventRef{OutboxEventID: event, Notes: []string{domain.PhoneNoteNoIncidentRef}}

	res, err := d.DeliverPhone(w.ctx, ids, alert, ev)
	if err != nil {
		t.Fatalf("DeliverPhone: %v", err)
	}
	if res.Resolved != 2 || res.Delivered != 2 {
		t.Fatalf("delivery = %+v; want 2 resolved (carol is no route), 2 delivered", res)
	}
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d; want 2", len(calls))
	}
	to := map[string]string{}
	for _, c := range calls {
		to[c.Kind] = c.To
	}
	if to["sms"] != "+447700900123" || to["voice"] != "+905551110001" {
		t.Fatalf("destinations = %v", to)
	}

	rows := map[string]string{}
	r, err := w.st.pool.Query(w.ctx, `SELECT channel_id::text, reason || ':' || array_to_string(notes, ',')
		  FROM phone_deliveries WHERE outbox_event_id = $1`, event)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var ch, v string
		if err := r.Scan(&ch, &v); err != nil {
			t.Fatal(err)
		}
		rows[ch] = v
	}
	r.Close()
	want := map[string]string{
		duty.ID:       "sent:no_incident_ref",
		aliceVoice.ID: "sent:no_incident_ref",
		carolSMS.ID:   "not_deliverable:no_incident_ref,no_verified_number",
	}
	for ch, v := range want {
		if rows[ch] != v {
			t.Fatalf("rows = %v; want %v", rows, want)
		}
	}

	// D19: the outbox retries the whole event; nobody is called again, and the sent rows still count.
	again, err := d.DeliverPhone(w.ctx, ids, alert, ev)
	if err != nil || again.Resolved != 2 || again.Delivered != 2 {
		t.Fatalf("retry: %+v, %v", again, err)
	}
	if n := len(fake.Calls()); n != 2 {
		t.Fatalf("provider calls after the retry = %d; want still 2", n)
	}
}
