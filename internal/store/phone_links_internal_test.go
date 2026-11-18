package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-041a Task 12: what the outbox's phone path reads beside the payload — the incident a phone alert
// links to (D32 for a monitor, R15 for a service). DB-gated.

// D32: a monitor's link is its OPEN auto-incident; for a recovery, failing that, its latest
// auto-incident resolved at or after the event was enqueued — never an older outage's, never a manual
// incident.
func TestMonitorPhoneLink(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "api")
	other := w.monitor(t, "web")
	ref := func(id string) string {
		t.Helper()
		r, err := w.st.IncidentRef(w.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	resolveAt := func(id string, at time.Time) {
		t.Helper()
		if _, err := w.st.pool.Exec(w.ctx, `UPDATE incidents SET status = 'resolved', resolved_at = $2 WHERE id = $1`,
			id, at); err != nil {
			t.Fatal(err)
		}
	}
	link := func(monitorID string, since time.Time) PhoneLink {
		t.Helper()
		l, err := w.st.MonitorPhoneLink(w.ctx, monitorID, since)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	var dbNow time.Time
	if err := w.st.pool.QueryRow(w.ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	enqueued := dbNow.Add(-time.Minute)

	// Nothing yet: no link, for the DOWN (no time) and the recovery alike.
	if l := link(m, time.Time{}); l != (PhoneLink{}) {
		t.Fatalf("no incident: %+v", l)
	}
	// The previous outage, resolved BEFORE the recovery event was enqueued, is never linked.
	old := w.openAuto(t, m)
	resolveAt(old.ID, enqueued.Add(-time.Second))
	if l := link(m, enqueued); l != (PhoneLink{}) {
		t.Fatalf("an older outage was linked: %+v", l)
	}
	// A manual incident anchored to the monitor is not its auto-incident.
	openManualFixture(t, w.st, w.ctx, domain.Incident{ProjectID: w.project, MonitorID: m, Title: "manual",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual})
	if l := link(m, enqueued); l != (PhoneLink{}) {
		t.Fatalf("a manual incident was linked: %+v", l)
	}

	// The outage's own incident: open → linked for the DOWN and the recovery alike.
	cur := w.openAuto(t, m)
	want := PhoneLink{IncidentID: cur.ID, Ref: ref(cur.ID)}
	if l := link(m, time.Time{}); l != want {
		t.Fatalf("open incident, DOWN: %+v; want %+v", l, want)
	}
	if l := link(m, enqueued); l != want {
		t.Fatalf("open incident, recovery: %+v; want %+v", l, want)
	}
	// Resolved by the reconciler after the recovery was enqueued: still the recovery's link, and no
	// longer the DOWN's (a DOWN links only an open incident).
	resolveAt(cur.ID, enqueued.Add(time.Second))
	if l := link(m, enqueued); l != want {
		t.Fatalf("resolved after the event: %+v; want %+v", l, want)
	}
	if l := link(m, time.Time{}); l != (PhoneLink{}) {
		t.Fatalf("a DOWN linked a resolved incident: %+v", l)
	}
	// Exactly at the enqueue time counts ("at or after").
	resolveAt(cur.ID, enqueued)
	if l := link(m, enqueued); l != want {
		t.Fatalf("resolved at the event's time: %+v; want %+v", l, want)
	}
	// Another monitor's incident is never this one's.
	if l := link(other, enqueued); l != (PhoneLink{}) {
		t.Fatalf("another monitor: %+v", l)
	}
}

// Pre-review 6: the link's choice among several candidates, pinned before its predicate was built from
// the shared open-auto-incident fragments. Of two auto-incidents resolved inside the recovery window
// the later resolution wins; an open auto-incident wins over both.
func TestMonitorPhoneLinkPrefersOpenThenLatestResolution(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "api")
	var dbNow time.Time
	if err := w.st.pool.QueryRow(w.ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	enqueued := dbNow.Add(-time.Hour)
	resolveAt := func(id string, at time.Time) {
		t.Helper()
		if _, err := w.st.pool.Exec(w.ctx, `UPDATE incidents SET status = 'resolved', resolved_at = $2 WHERE id = $1`,
			id, at); err != nil {
			t.Fatal(err)
		}
	}
	link := func() string {
		t.Helper()
		l, err := w.st.MonitorPhoneLink(w.ctx, m, enqueued)
		if err != nil {
			t.Fatal(err)
		}
		return l.IncidentID
	}
	// Opened in this order, resolved in the other: the later resolution belongs to the earlier one.
	first := w.openAuto(t, m)
	resolveAt(first.ID, enqueued.Add(20*time.Minute))
	second := w.openAuto(t, m)
	resolveAt(second.ID, enqueued.Add(10*time.Minute))
	if got := link(); got != first.ID {
		t.Fatalf("two resolved in the window: linked %s; want the later resolution %s", got, first.ID)
	}
	open := w.openAuto(t, m)
	if got := link(); got != open.ID {
		t.Fatalf("an open one and two resolved: linked %s; want the open %s", got, open.ID)
	}
}

// R15: a service alert's link is the service's open auto-incident, else its latest auto-incident
// resolved at or after the event was enqueued, else none.
func TestServicePhoneLink(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	svc, err := w.st.CreateService(w.ctx, domain.Service{ProjectID: w.project, Slug: "checkout", Name: "Checkout"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := w.st.CreateService(w.ctx, domain.Service{ProjectID: w.project, Slug: "search", Name: "Search"})
	if err != nil {
		t.Fatal(err)
	}
	var dbNow time.Time
	if err := w.st.pool.QueryRow(w.ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	enqueued := dbNow.Add(-time.Minute)
	link := func(serviceID string) PhoneLink {
		t.Helper()
		l, err := w.st.ServicePhoneLink(w.ctx, serviceID, enqueued)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if l := link(svc.ID); l != (PhoneLink{}) {
		t.Fatalf("no incident: %+v", l)
	}
	old := openServiceFixture(t, w.st, w.ctx, w.project, svc.ID, "old", domain.ImpactMajor)
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE incidents SET status = 'resolved', resolved_at = $2 WHERE id = $1`,
		old.ID, enqueued.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if l := link(svc.ID); l != (PhoneLink{}) {
		t.Fatalf("an older episode was linked: %+v", l)
	}
	cur := openServiceFixture(t, w.st, w.ctx, w.project, svc.ID, "now", domain.ImpactMajor)
	r, err := w.st.IncidentRef(w.ctx, cur.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := PhoneLink{IncidentID: cur.ID, Ref: r}
	if l := link(svc.ID); l != want {
		t.Fatalf("open incident: %+v; want %+v", l, want)
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE incidents SET status = 'resolved', resolved_at = $2 WHERE id = $1`,
		cur.ID, enqueued.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if l := link(svc.ID); l != want {
		t.Fatalf("resolved after the event: %+v; want %+v", l, want)
	}
	if l := link(other.ID); l != (PhoneLink{}) {
		t.Fatalf("another service: %+v", l)
	}
}

// Task 12 re-review N-1: the phone-id-only reads the outbox falls back to when the text half's channel
// read failed. One Slack channel whose config cannot be decrypted fails the whole list; these reads
// open no config, so they still name the enabled sms and voice channels — a monitor's newest first,
// a snapshot's or a step's in the order given.
func TestPhoneChannelIDReadsPastAnUndecryptableConfig(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "api")
	alice := w.member(t, "alice@example.com", domain.RoleEditor)
	w.verifiedPhone(t, alice, "+905551110001")
	sms := w.userChannel(t, domain.ChannelSMS, alice)
	voice := w.userChannel(t, domain.ChannelVoice, alice)
	off := w.userChannel(t, domain.ChannelSMS, alice)
	disabled := false
	if _, err := w.patch(off.ID, PhoneChannelPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project,
		Type: domain.ChannelSlack, Name: "slack", Enabled: true, Config: map[string]string{"url": "https://hooks.example/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE notification_channels SET config = '{"url":"enc:v1:!!not-a-ciphertext"}' WHERE id = $1`,
		slack.ID); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{slack.ID, sms.ID, voice.ID, off.ID, w.channel} {
		if err := w.st.LinkMonitorChannel(w.ctx, m, ch); err != nil {
			t.Fatal(err)
		}
	}
	// The text reads fail on the Slack config: what the outbox's text half meets.
	if _, err := w.st.ListEnabledChannelsForMonitor(w.ctx, m); err == nil {
		t.Fatal("the monitor's channel list opened an undecryptable config")
	}
	if _, err := w.st.EnabledChannelsByIDs(w.ctx, []string{slack.ID, sms.ID}); err == nil {
		t.Fatal("the snapshot's channel list opened an undecryptable config")
	}

	got, err := w.st.MonitorPhoneChannelIDs(w.ctx, m)
	if want := []string{voice.ID, sms.ID}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("monitor phone channels %v, %v; want the enabled ones newest first %v", got, err, want)
	}
	if got, err := w.st.MonitorPhoneChannelIDs(w.ctx, w.monitor(t, "web")); err != nil || len(got) != 0 {
		t.Fatalf("a monitor without links: %v, %v", got, err)
	}
	ids := []string{slack.ID, sms.ID, off.ID, voice.ID, w.channel}
	if got, err := w.st.PhoneChannelIDs(w.ctx, ids); err != nil || !reflect.DeepEqual(got, []string{sms.ID, voice.ID}) {
		t.Fatalf("phone channels among %v: %v, %v; want [sms voice]", ids, got, err)
	}
	if got, err := w.st.PhoneChannelIDs(w.ctx, []string{voice.ID, slack.ID, sms.ID}); err != nil ||
		!reflect.DeepEqual(got, []string{voice.ID, sms.ID}) {
		t.Fatalf("the order given is kept: %v, %v", got, err)
	}
	if got, err := w.st.PhoneChannelIDs(w.ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no ids: %v, %v", got, err)
	}
}
