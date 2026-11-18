package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-041a revision 7, §11 row "API — what the SPA draws": what the SPA of part (a) reads that the
// first build of the store did not return — the recipient's name and `only_route_for` on a channel
// (§4.3), the subject, service state, escalation step and recipient's name on a delivery and the
// storm limit in `notes` (§4.5), a new code to a stored number and the instance's voice language
// (§4.2). DB-gated.

// renameUser gives a user a display name that is not their email, so a test can tell the two apart.
func renameUser(t *testing.T, w phoneChannelWorld, userID, name string) {
	t.Helper()
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE users SET display_name = $2 WHERE id = $1`, userID, name); err != nil {
		t.Fatal(err)
	}
}

func wantRecipientName(t *testing.T, label string, ch domain.NotificationChannel, want *string) {
	t.Helper()
	got := ch.RecipientDisplayName
	switch {
	case want == nil && (!got.Present || got.Name != nil):
		t.Fatalf("%s: recipient_display_name = %+v; want present and null", label, got)
	case want != nil && (!got.Present || got.Name == nil || *got.Name != *want):
		t.Fatalf("%s: recipient_display_name = %+v; want %q", label, got, *want)
	}
}

func channelJSON(t *testing.T, ch domain.NotificationChannel) string {
	t.Helper()
	b, err := json.Marshal(ch.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// §4.3: every channel read names a `user` channel's recipient by display name — never an email —
// also once the person no longer reaches the project, and null once they are deleted. A `number`
// channel and the four other types carry no such key.
func TestPhoneChannelRecipientDisplayName(t *testing.T) {
	w := newPhoneChannelWorld(t)
	bob := w.member(t, "bob@example.com", domain.RoleEditor)
	renameUser(t, w, bob, "Bob Builder")
	name := "Bob Builder"

	created := w.userChannel(t, domain.ChannelSMS, bob)
	wantRecipientName(t, "create", created, &name)
	wantRecipientName(t, "get", w.get(t, created.ID), &name)
	updated, err := w.patch(created.ID, PhoneChannelPatch{Name: ptr("pager")})
	if err != nil {
		t.Fatal(err)
	}
	wantRecipientName(t, "update", updated, &name)
	list, err := w.st.ListChannelsByProject(w.ctx, w.project)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	wantRecipientName(t, "list", list[0], &name)
	if js := channelJSON(t, list[0]); !strings.Contains(js, `"recipient_display_name":"Bob Builder"`) ||
		strings.Contains(js, "bob@example.com") {
		t.Fatalf("channel JSON = %s; want the name and no email", js)
	}

	// The person loses access to the project: the channel stops being a route and still says whose.
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM memberships WHERE user_id = $1`, bob); err != nil {
		t.Fatal(err)
	}
	gone := w.get(t, created.ID)
	wantRecipientName(t, "no access", gone, &name)
	wantDeliverable(t, "no access", gone, domain.DeliverableRecipientNotEligible)

	number := w.numberChannel(t, domain.ChannelVoice, phoneB)
	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSlack,
		Name: "slack", Config: map[string]string{"url": "https://hooks.example/x"}})
	if err != nil {
		t.Fatal(err)
	}
	for label, ch := range map[string]domain.NotificationChannel{"number": w.get(t, number.ID), "slack": w.get(t, slack.ID)} {
		if ch.RecipientDisplayName.Present {
			t.Fatalf("%s channel carries recipient_display_name %+v", label, ch.RecipientDisplayName)
		}
		if js := channelJSON(t, ch); strings.Contains(js, "recipient_display_name") {
			t.Fatalf("%s channel JSON = %s; want no recipient_display_name key", label, js)
		}
	}

	if err := w.st.DeleteUser(w.ctx, bob); err != nil {
		t.Fatal(err)
	}
	deleted := w.get(t, created.ID)
	wantRecipientName(t, "deleted", deleted, nil)
	if js := channelJSON(t, deleted); !strings.Contains(js, `"recipient_display_name":null`) {
		t.Fatalf("deleted recipient's channel JSON = %s; want recipient_display_name null", js)
	}
}

// expectedOnlyRouteFor is the route predicate's own answer about a channel: the paging services that
// are routable now and are not once the channel is gone — asked through ownsPagingRoutableTx in a
// transaction that deletes the channel and rolls back.
func expectedOnlyRouteFor(t *testing.T, w phoneRouteWorld, channelID string) ([]domain.ServiceWithoutRoute, int) {
	t.Helper()
	tx, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(w.ctx) //nolint:errcheck // a probe: never committed
	before, err := ownsPagingRoutableTx(w.ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(w.ctx, `DELETE FROM notification_channels WHERE id = $1`, channelID); err != nil {
		t.Fatal(err)
	}
	after, err := ownsPagingRoutableTx(w.ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	return servicesLosingRoute(before, after)
}

func (w phoneRouteWorld) onlyRouteFor(t *testing.T, channelID string) domain.OnlyRouteFor {
	t.Helper()
	got, err := w.st.PhoneChannelsOnlyRouteFor(w.ctx, []string{channelID})
	if err != nil {
		t.Fatal(err)
	}
	o, ok := got[channelID]
	if !ok {
		t.Fatalf("no only_route_for for phone channel %s: %v", channelID, got)
	}
	return o
}

// §4.3, §9: `only_route_for` is the route predicate of D31, in SQL — it agrees with the predicate for
// a channel that is a service's only route, one that is not (another route exists), and one that
// stops being one (the recipient's number goes, the channel is paused).
func TestPhoneChannelOnlyRouteFor(t *testing.T) {
	w := newPhoneRouteWorld(t)
	w.verifiedPhone(t, w.admin, phoneA)
	ch := w.userChannel(t, domain.ChannelSMS, w.admin)
	var slug, svcName, projName string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT s.slug, s.name, p.name FROM services s JOIN projects p ON p.id = s.project_id
		 WHERE s.id = $1`, w.f.serviceID).Scan(&slug, &svcName, &projName); err != nil {
		t.Fatal(err)
	}
	agree := func(label string, want []domain.ServiceWithoutRoute) {
		t.Helper()
		got := w.onlyRouteFor(t, ch.ID)
		exp, total := expectedOnlyRouteFor(t, w, ch.ID)
		if exp == nil {
			exp = []domain.ServiceWithoutRoute{}
		}
		if !reflect.DeepEqual(got.Services, exp) || got.Total != total {
			t.Fatalf("%s: only_route_for = %+v (total %d); the route predicate says %+v (total %d)", label, got.Services, got.Total, exp, total)
		}
		if !reflect.DeepEqual(got.Services, want) {
			t.Fatalf("%s: only_route_for = %+v; want %+v", label, got.Services, want)
		}
	}
	only := []domain.ServiceWithoutRoute{{Project: projName, Service: svcName, Slug: slug}}

	agree("the only route", only)

	// Another route in the project: the phone channel is no longer the only one, nor is the webhook.
	hook, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelWebhook,
		Name: "hook", Config: map[string]string{"url": "https://hooks.example/x"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	agree("a second route", []domain.ServiceWithoutRoute{})
	if got, err := w.st.PhoneChannelsOnlyRouteFor(w.ctx, []string{hook.ID}); err != nil || len(got) != 0 {
		t.Fatalf("a webhook channel's only_route_for = %v, %v; want none (phone channels only)", got, err)
	}
	if err := w.st.SetNotificationChannelEnabled(w.ctx, hook.ID, false); err != nil {
		t.Fatal(err)
	}
	agree("the second route paused", only)

	// The channel stops being a route: its recipient's number goes, then the channel is paused.
	if err := w.st.DeleteUserPhone(w.ctx, w.admin); err != nil {
		t.Fatal(err)
	}
	agree("no verified number", []domain.ServiceWithoutRoute{})
	w.verifiedPhone(t, w.admin, phoneA)
	agree("verified again", only)
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	agree("paused", []domain.ServiceWithoutRoute{})
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	// A service that does not own paging is never listed.
	exec(t, w.st, w.ctx, `UPDATE services SET owns_paging = false WHERE id = $1`, w.f.serviceID)
	agree("not paging", []domain.ServiceWithoutRoute{})
	exec(t, w.st, w.ctx, `UPDATE services SET owns_paging = true WHERE id = $1`, w.f.serviceID)

	// The schedule half: the channel as the schedule's only live participant, the project's channels
	// all paused but this one.
	w.attachSchedule(t, ch.ID)
	agree("on the schedule", only)

	// At most 50, ordered by project then slug, with the total beside them. The rows are inserted
	// directly: the store's per-project service cap would stop a fixture at 50.
	for i := 0; i < 51; i++ {
		exec(t, w.st, w.ctx, `INSERT INTO services (project_id, slug, name, owns_paging) VALUES ($1, $2, $3, true)`,
			w.project, fmt.Sprintf("bulk-%02d", i), fmt.Sprintf("Bulk %02d", i))
	}
	got := w.onlyRouteFor(t, ch.ID)
	exp, total := expectedOnlyRouteFor(t, w, ch.ID)
	if got.Total != 52 || total != 52 || len(got.Services) != 50 || !reflect.DeepEqual(got.Services, exp) {
		t.Fatalf("bulk: %d listed, total %d; predicate %d listed, total %d", len(got.Services), got.Total, len(exp), total)
	}
	if got.Services[0].Slug != "bulk-00" || got.Services[49].Slug != "bulk-49" {
		t.Fatalf("bulk order: first %s, last %s", got.Services[0].Slug, got.Services[49].Slug)
	}
}

// Revision-7 review M-4: only_route_for is ordered like the telephony settings' 409 — byte order, as
// Go compares strings — whatever collation the database gives the columns. A linguistic collation
// (glibc's, ICU's) orders "ab" before "Ba"; byte order puts "Ba" first. The test DB is bytewise by
// default, so the column is given an ICU collation — in a PRIVATE database made for this test (review
// C m-2): an ALTER in the shared schema would lock and reorder the queries of the packages running in
// parallel against it. Skipped where the server has no ICU collation.
func TestPhoneChannelOnlyRouteForOrdersByBytes(t *testing.T) {
	shared, ctx := serviceSchemaStore(t)
	var icu bool
	if err := shared.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_collation WHERE collprovider = 'i' LIMIT 1)`).Scan(&icu); err != nil {
		t.Fatalf("probe ICU: %v", err)
	}
	if !icu {
		t.Skip("the server has no ICU collation: a linguistic order cannot be provoked")
	}
	u, err := url.Parse(os.Getenv("CERBIX_TEST_DATABASE_DSN"))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL: %v", err)
	}
	name := fmt.Sprintf("cerbix_test_onlyroute_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := shared.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create private database (this gate requires CREATEDB): %v", err)
	}
	t.Cleanup(func() { dropProbeDatabase(t, shared.pool, name) })
	u.Path = "/" + name
	if err := Migrate(ctx, u.String()); err != nil {
		t.Fatalf("migrate private database: %v", err)
	}
	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open private database: %v", err)
	}
	t.Cleanup(st.Close) // runs before the drop: Cleanup is last-in first-out
	w := newPhoneRouteWorldOn(t, st, ctx)
	w.verifiedPhone(t, w.admin, phoneA)
	ch := w.userChannel(t, domain.ChannelSMS, w.admin)
	for _, slug := range []string{"ab", "Ba"} {
		exec(t, w.st, w.ctx, `INSERT INTO services (project_id, slug, name, owns_paging) VALUES ($1, $2, $3, true)`,
			w.project, slug, "svc "+slug)
	}
	exec(t, w.st, w.ctx, `ALTER TABLE services ALTER COLUMN slug SET DATA TYPE text COLLATE "en-US-x-icu"`)
	got := w.onlyRouteFor(t, ch.ID)
	exp, _ := expectedOnlyRouteFor(t, w, ch.ID)
	var gotSlugs, expSlugs []string
	for _, s := range got.Services {
		gotSlugs = append(gotSlugs, s.Slug)
	}
	for _, s := range exp {
		expSlugs = append(expSlugs, s.Slug)
	}
	if !reflect.DeepEqual(gotSlugs, expSlugs) {
		t.Fatalf("only_route_for orders %v; the settings 409 orders %v", gotSlugs, expSlugs)
	}
	if i, j := slices.Index(gotSlugs, "Ba"), slices.Index(gotSlugs, "ab"); i < 0 || j < 0 || i > j {
		t.Fatalf("order %v; want Ba before ab (bytes)", gotSlugs)
	}
}

// §4.5 "What a delivery says it was about": the subject, the service state, the escalation step and
// the recipient's name in both reads; subject null once the monitor is deleted, the name null for a
// `number` channel and once the user is deleted.
func TestPhoneDeliveryReadsSaySubjectStateStepRecipient(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	bob := w.member(t, "bob@example.com", domain.RoleEditor)
	renameUser(t, w.phoneChannelWorld, bob, "Bob Builder")
	w.verifiedPhone(t, bob, phoneA)
	userCh := w.userChannel(t, domain.ChannelVoice, bob)
	m := w.monitor(t, "checkout")
	w.setSeq(t, m, 4)
	inc := w.openAuto(t, m)
	svc, err := w.st.CreateService(w.ctx, domain.Service{ProjectID: w.project, Slug: "payments", Name: "Payments"})
	if err != nil {
		t.Fatal(err)
	}
	storm := domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}

	down := w.in(t, domain.PhoneAlertMonitorDown, storm)
	down.ChannelID, down.UserID, down.Number, down.MonitorID, down.MonitorSeq = userCh.ID, bob, phoneA, m, 4
	downRes := w.reserve(t, down)

	step := w.in(t, domain.PhoneAlertEscalationStep, storm)
	step.IncidentID, step.MonitorID, step.EscalationStep = inc.ID, m, 2
	stepRes := w.reserve(t, step)

	health := w.in(t, domain.PhoneAlertServiceHealth, storm)
	health.ServiceID, health.ServiceState, health.IncidentID = svc.ID, string(domain.ServiceAlertDegraded), inc.ID
	healthRes := w.reserve(t, health)

	unsent := w.in(t, domain.PhoneAlertEscalationStep, storm)
	unsent.ChannelID, unsent.UserID, unsent.Number = userCh.ID, bob, ""
	unsent.IncidentID, unsent.ServiceID, unsent.EscalationStep = inc.ID, svc.ID, 3
	if _, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, unsent, domain.PhoneReasonNotDeliverable); err != nil {
		t.Fatal(err)
	}
	unsentHealth := w.in(t, domain.PhoneAlertServiceHealth, storm)
	unsentHealth.ServiceID, unsentHealth.ServiceState, unsentHealth.IncidentID = svc.ID, string(domain.ServiceAlertDown), inc.ID
	if _, _, err := w.st.RecordUnsentPhoneDelivery(w.ctx, unsentHealth, domain.PhoneReasonTelephonyDisabled); err != nil {
		t.Fatal(err)
	}

	byID := func(views []domain.PhoneDeliveryView) map[string]domain.PhoneDeliveryView {
		out := map[string]domain.PhoneDeliveryView{}
		for _, v := range views {
			out[v.ID] = v
		}
		return out
	}
	incViews, err := w.st.ListIncidentPhoneDeliveries(w.ctx, inc.ID)
	if err != nil || len(incViews) != 5 {
		t.Fatalf("incident list = %d rows, %v; want 5", len(incViews), err)
	}
	userViews, err := w.st.ListChannelPhoneDeliveries(w.ctx, userCh.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	numViews, err := w.st.ListChannelPhoneDeliveries(w.ctx, w.channel, 50)
	if err != nil {
		t.Fatal(err)
	}
	all := byID(incViews)
	chanViews := byID(append(userViews, numViews...))
	if len(chanViews) != 5 {
		t.Fatalf("channel lists = %d rows; want 5", len(chanViews))
	}

	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	type want struct {
		subject *domain.PhoneDeliverySubject
		state   *string
		step    *int
		name    *string
	}
	monitorSubject := &domain.PhoneDeliverySubject{Kind: domain.PhoneDeliverySubjectMonitor, Name: "checkout"}
	serviceSubject := &domain.PhoneDeliverySubject{Kind: domain.PhoneDeliverySubjectService, Name: "Payments"}
	check := func(label string, views map[string]domain.PhoneDeliveryView, id string, wt want) {
		t.Helper()
		v, ok := views[id]
		if !ok {
			t.Fatalf("%s: row %s missing", label, id)
		}
		if !reflect.DeepEqual(v.Subject, wt.subject) || !reflect.DeepEqual(v.ServiceState, wt.state) ||
			!reflect.DeepEqual(v.EscalationStep, wt.step) || !reflect.DeepEqual(v.RecipientDisplayName, wt.name) {
			t.Fatalf("%s: subject %+v state %v step %v name %v; want %+v %v %v %v", label, v.Subject, v.ServiceState,
				v.EscalationStep, v.RecipientDisplayName, wt.subject, wt.state, wt.step, wt.name)
		}
	}
	unsentID := func(in domain.PhoneReserveInput) string {
		t.Helper()
		var id string
		if err := w.st.pool.QueryRow(w.ctx, `SELECT id::text FROM phone_deliveries WHERE outbox_event_id = $1`, in.OutboxEventID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cases := []struct {
		label string
		id    string
		wt    want
	}{
		{"monitor down", downRes.ID, want{subject: monitorSubject, name: str("Bob Builder")}},
		{"escalation step", stepRes.ID, want{subject: monitorSubject, step: num(2)}},
		{"service health", healthRes.ID, want{subject: serviceSubject, state: str("degraded")}},
		{"unsent step", unsentID(unsent), want{subject: serviceSubject, step: num(3), name: str("Bob Builder")}},
		{"unsent health", unsentID(unsentHealth), want{subject: serviceSubject, state: str("down")}},
	}
	for _, c := range cases {
		check("incident list, "+c.label, all, c.id, c.wt)
		check("channel list, "+c.label, chanViews, c.id, c.wt)
	}
	js, err := json.Marshal(all[downRes.ID])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"subject":{"kind":"monitor","name":"checkout"}`, `"service_state":null`, `"escalation_step":null`,
		`"recipient_display_name":"Bob Builder"`} {
		if !strings.Contains(string(js), key) {
			t.Fatalf("delivery JSON %s lacks %s", js, key)
		}
	}
	if strings.Contains(string(js), "bob@example.com") {
		t.Fatalf("delivery JSON carries an email: %s", js)
	}

	// The monitor and the user are deleted: the rows stay, without a subject or a name.
	if err := w.st.DeleteMonitor(w.ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := w.st.DeleteUser(w.ctx, bob); err != nil {
		t.Fatal(err)
	}
	userViews, err = w.st.ListChannelPhoneDeliveries(w.ctx, userCh.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	numViews, err = w.st.ListChannelPhoneDeliveries(w.ctx, w.channel, 50)
	if err != nil {
		t.Fatal(err)
	}
	after := byID(append(userViews, numViews...))
	check("after deletion, monitor down", after, downRes.ID, want{})
	check("after deletion, escalation step", after, stepRes.ID, want{step: num(2)})
	check("after deletion, unsent step", after, unsentID(unsent), want{subject: serviceSubject, step: num(3)})
	if js, _ := json.Marshal(after[downRes.ID]); !strings.Contains(string(js), `"subject":null`) ||
		!strings.Contains(string(js), `"recipient_display_name":null`) {
		t.Fatalf("after deletion: %s; want subject and recipient_display_name null", js)
	}
}

// Revision-7 review M-2: the subject is the SERVICE when the row names one — an escalation step of a
// service incident can name its monitor too — and null once that service is deleted, never the
// monitor in its place.
func TestPhoneDeliverySubjectIsTheServiceWhenTheRowNamesOne(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "checkout")
	svc, err := w.st.CreateService(w.ctx, domain.Service{ProjectID: w.project, Slug: "payments", Name: "Payments"})
	if err != nil {
		t.Fatal(err)
	}
	in := w.in(t, domain.PhoneAlertEscalationStep, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100})
	in.MonitorID, in.ServiceID, in.EscalationStep = m, svc.ID, 1
	r := w.reserve(t, in)
	subject := func() *domain.PhoneDeliverySubject {
		t.Helper()
		views, err := w.st.ListChannelPhoneDeliveries(w.ctx, w.channel, 50)
		if err != nil || len(views) != 1 || views[0].ID != r.ID {
			t.Fatalf("views %+v, %v", views, err)
		}
		return views[0].Subject
	}
	if got := subject(); !reflect.DeepEqual(got, &domain.PhoneDeliverySubject{Kind: domain.PhoneDeliverySubjectService, Name: "Payments"}) {
		t.Fatalf("subject %+v; want the service", got)
	}
	if err := w.st.DeleteService(w.ctx, w.project, svc.ID); err != nil {
		t.Fatal(err)
	}
	if got := subject(); got != nil {
		t.Fatalf("subject after the service's deletion %+v; want null, not the monitor", got)
	}
}

// §4.5: a service state outside the closed vocabulary is not stored (the column's CHECK would refuse
// the row, and the page must not be lost for a label).
func TestPhoneDeliveryServiceStateIsClosed(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	in := w.in(t, domain.PhoneAlertServiceHealth, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100})
	in.ServiceState = "on-fire"
	r := w.reserve(t, in)
	var state *string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT service_state FROM phone_deliveries WHERE id = $1`, r.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("service_state = %q; want NULL for a state outside the vocabulary", *state)
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE phone_deliveries SET service_state = 'on-fire' WHERE id = $1`, r.ID); err == nil {
		t.Fatal("the column accepted a state outside the vocabulary")
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE phone_deliveries SET escalation_step = 0 WHERE id = $1`, r.ID); err == nil {
		t.Fatal("the column accepted escalation step 0 (steps are 1-based)")
	}
}

// §4.5: a `storm_limited` row names the limit that refused it in `notes` — storm_per_incident, the
// monitor-counted form of §6.2 included, or storm_per_number — and never the limit's value.
func TestPhoneStormLimitNamedInNotes(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	m := w.monitor(t, "storm-notes")
	w.setSeq(t, m, 2)
	inc := openManualFixture(t, w.st, w.ctx, domain.Incident{ProjectID: w.project, Title: "a",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual})
	notes := func(r domain.PhoneReservation) []string {
		t.Helper()
		if r.Status != domain.PhoneReservationStormLimited {
			t.Fatalf("status %s; want storm_limited", r.Status)
		}
		return w.row(t, r.ID).notes
	}
	wantNotes := func(label string, got []string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: notes = %v; want %v", label, got, want)
		}
	}

	// Per incident.
	storm := domain.TelephonyStorm{PerIncidentPerHour: 1, PerNumberPerHour: 100}
	step := func(n domain.TelephonyStorm) domain.PhoneReservation {
		in := w.in(t, domain.PhoneAlertEscalationStep, n)
		in.IncidentID = inc.ID
		return w.reserve(t, in)
	}
	if r := step(storm); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("first step: %s", r.Status)
	}
	wantNotes("per incident", notes(step(storm)), domain.PhoneNoteStormPerIncident)

	// Per monitor (a DOWN whose incident is not open yet), which is the per-incident limit.
	down := func() domain.PhoneReservation {
		in := w.in(t, domain.PhoneAlertMonitorDown, storm)
		in.MonitorID, in.MonitorSeq = m, 2
		return w.reserve(t, in)
	}
	if r := down(); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("first DOWN: %s", r.Status)
	}
	wantNotes("per monitor", notes(down()), domain.PhoneNoteStormPerIncident)

	// Per number: a burn alert has no subject, and the number's bound is reached.
	exec(t, w.st, w.ctx, `DELETE FROM phone_deliveries`)
	storm = domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 1}
	burn := func() domain.PhoneReservation { return w.reserve(t, w.in(t, domain.PhoneAlertMonitorBurn, storm)) }
	if r := burn(); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("first burn: %s", r.Status)
	}
	wantNotes("per number", notes(burn()), domain.PhoneNoteStormPerNumber)

	// Both bounds reached: the number's is named, the one checked first. One step holds the
	// incident's only slot and the number's only slot, so the next step of the incident is over both
	// (revision-7 review M-1: the bounds of one each, and the incident alone shown to be reached).
	exec(t, w.st, w.ctx, `DELETE FROM phone_deliveries`)
	if r := step(domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100}); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("the step holding both slots: %s", r.Status)
	}
	wantNotes("the incident's alone", notes(step(domain.TelephonyStorm{PerIncidentPerHour: 1, PerNumberPerHour: 100})),
		domain.PhoneNoteStormPerIncident)
	wantNotes("both", notes(step(domain.TelephonyStorm{PerIncidentPerHour: 1, PerNumberPerHour: 1})), domain.PhoneNoteStormPerNumber)

	// A re-armed provider_unreachable row that the guard refuses names the limit too, beside the notes
	// it already had.
	exec(t, w.st, w.ctx, `DELETE FROM phone_deliveries`)
	storm = domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 1}
	retry := w.in(t, domain.PhoneAlertMonitorBurn, storm)
	retry.Notes = []string{domain.PhoneNoteLinkOmitted}
	first := w.reserve(t, retry)
	w.finish(t, first.ID, domain.PhoneReasonProviderUnreachable)
	if r := burn(); r.Status != domain.PhoneReservationProceed {
		t.Fatalf("burn in the freed slot: %s", r.Status)
	}
	again := w.reserve(t, retry)
	if again.ID != first.ID {
		t.Fatalf("the retry wrote a new row %s; want %s re-armed", again.ID, first.ID)
	}
	wantNotes("re-armed", notes(again), domain.PhoneNoteLinkOmitted, domain.PhoneNoteStormPerNumber)

	// The value of a limit is in no row.
	var leaked int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_deliveries
		  WHERE EXISTS (SELECT 1 FROM unnest(notes) n WHERE n ~ '[0-9]')`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("%d rows carry a number in their notes (%v)", leaked, err)
	}
}

// §4.2: a new code to the STORED unverified number, under the limits of the first send; refused
// without a number and once verified.
func TestSendUserPhoneCode(t *testing.T) {
	w := newPhoneChannelWorld(t)
	bob := w.member(t, "bob@example.com", domain.RoleViewer)
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, bob); !errors.Is(err, domain.ErrPhoneNoNumber) {
		t.Fatalf("without a number: %v; want ErrPhoneNoNumber", err)
	}
	scriptPhoneCodes(t, "111111", "222222", "333333")
	first := mustSetPhone(t, w.st, w.ctx, bob, phoneA)
	number, second, err := w.st.SendUserPhoneCode(w.ctx, bob)
	if err != nil || number != phoneA || second != "222222" {
		t.Fatalf("resend = %q, %q, %v; want the stored number and a new code", number, second, err)
	}
	if _, err := w.st.VerifyUserPhone(w.ctx, bob, first); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("the first code after a resend: %v; want code_incorrect (replaced)", err)
	}
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, bob); err != nil {
		t.Fatalf("third code of the hour: %v", err)
	}
	// R6: the PUT's code and the two resends are the hour's three.
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, bob); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("fourth code of the hour: %v; want ErrPhoneSendLimited", err)
	}
	if n := verificationRows(t, w.st, w.ctx, domain.PhoneSubjectUser, bob); n != 3 {
		t.Fatalf("%d code rows; want 3", n)
	}
	if _, err := w.st.VerifyUserPhone(w.ctx, bob, "000000"); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("wrong code: %v", err)
	}
	// The latest code verifies the stored number.
	exec(t, w.st, w.ctx, `DELETE FROM phone_verifications WHERE subject_id = $1`, bob)
	scriptPhoneCodes(t, "444444")
	if _, code, err := w.st.SendUserPhoneCode(w.ctx, bob); err != nil || code != "444444" {
		t.Fatalf("resend after the hour: %q, %v", code, err)
	}
	if p, err := w.st.VerifyUserPhone(w.ctx, bob, "444444"); err != nil || !p.Verified {
		t.Fatalf("verify the resent code: %+v, %v", p, err)
	}
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, bob); !errors.Is(err, domain.ErrPhoneAlreadyVerified) {
		t.Fatalf("once verified: %v; want ErrPhoneAlreadyVerified", err)
	}

	// The per-number limit counts every subject's codes to the number (R6): ten a day.
	carol := w.member(t, "carol@example.com", domain.RoleViewer)
	exec(t, w.st, w.ctx, `DELETE FROM phone_verifications`)
	mustSetPhone(t, w.st, w.ctx, carol, phoneB)
	exec(t, w.st, w.ctx, `UPDATE phone_verifications SET created_at = now() - interval '2 hours'`)
	for i := 0; i < 9; i++ {
		exec(t, w.st, w.ctx, `INSERT INTO phone_verifications (subject_kind, subject_id, requested_by, number_hash, code_hash, expires_at, created_at)
			SELECT 'channel', gen_random_uuid(), gen_random_uuid()::text, number_hash, 'x', now(), now() - interval '2 hours' FROM user_phones WHERE user_id = $1`, carol)
	}
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, carol); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("eleventh code to the number in a day: %v; want ErrPhoneSendLimited", err)
	}

	// Without the at-rest key nothing is opened.
	w.st.WithCipher(nil)
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, carol); !errors.Is(err, domain.ErrPhoneKeyMissing) {
		t.Fatalf("without the key: %v", err)
	}
}

// §4.2: GET /api/v1/me's telephony_voice_language is the instance's voice_language — §4.1's default
// when the group was never saved or names none.
func TestTelephonyVoiceLanguage(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	if got, err := st.TelephonyVoiceLanguage(ctx); err != nil || got != "en-US" {
		t.Fatalf("never saved: %q, %v; want en-US", got, err)
	}
	ts := twilioTS(true, "tok")
	ts.VoiceLanguage = "tr-TR"
	ts.Normalize()
	if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{ConfirmUnroutable: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.TelephonyVoiceLanguage(ctx); err != nil || got != "tr-TR" {
		t.Fatalf("saved: %q, %v; want tr-TR", got, err)
	}
	// Read without opening a secret: a key that cannot open the token still answers.
	st.WithCipher(nil)
	if got, err := st.TelephonyVoiceLanguage(ctx); err != nil || got != "tr-TR" {
		t.Fatalf("without the key: %q, %v", got, err)
	}
}

// FR-041a revision 9, §4.2: GET /api/v1/me's telephony_can_call is the instance's "can place calls"
// (D3) — true only while telephony is on AND the provider can place calls (twilio whose `from` is a
// number), so the form offers the call only when it can be made. Never saved, off, generic_http and
// twilio with a Messaging Service SID are all false. Read without opening a secret.
func TestTelephonyCanCall(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	if got, err := st.TelephonyCanCall(ctx); err != nil || got {
		t.Fatalf("never saved: %v, %v; want false", got, err)
	}
	mg := twilioTS(true, "tok")
	mg.Twilio.From = "MG0123456789abcdef0123456789abcdef"
	mg.Normalize()
	cases := []struct {
		name string
		ts   domain.TelephonySettings
		want bool
	}{
		{"twilio with a number", twilioTS(true, "tok"), true},
		{"twilio with a number, telephony off", twilioTS(false, "tok"), false},
		{"twilio with a Messaging Service SID", mg, false},
		{"generic_http", genericTS(true, nil), false},
		{"twilio with a number again", twilioTS(true, "tok"), true},
	}
	for _, tc := range cases {
		if err := st.SaveTelephonySettings(ctx, tc.ts, domain.TelephonySaveOptions{ConfirmUnroutable: true}); err != nil {
			t.Fatalf("%s: save: %v", tc.name, err)
		}
		if got, err := st.TelephonyCanCall(ctx); err != nil || got != tc.want {
			t.Fatalf("%s: TelephonyCanCall = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	st.WithCipher(nil)
	if got, err := st.TelephonyCanCall(ctx); err != nil || !got {
		t.Fatalf("without the key: %v, %v; want true, read without opening a secret", got, err)
	}
}

// §4.3, §4.5 "never an email": a display name that holds an email (the OIDC preferred_username
// fallback, or a person who typed one) is answered by its part before the first '@' — on the channel
// read and on both delivery reads — and by null when that part is empty.
func TestRecipientDisplayNameNeverCarriesAnEmail(t *testing.T) {
	w := newPhoneDeliveryWorld(t)
	dana := w.member(t, "dana@example.com", domain.RoleEditor)
	renameUser(t, w.phoneChannelWorld, dana, "dana.ops@corp.example")
	w.verifiedPhone(t, dana, phoneA)
	ch := w.userChannel(t, domain.ChannelSMS, dana)
	m := w.monitor(t, "checkout")
	w.setSeq(t, m, 1)
	inc := w.openAuto(t, m)
	in := w.in(t, domain.PhoneAlertMonitorDown, domain.TelephonyStorm{PerIncidentPerHour: 100, PerNumberPerHour: 100})
	in.ChannelID, in.UserID, in.Number, in.MonitorID, in.MonitorSeq = ch.ID, dana, phoneA, m, 1
	w.reserve(t, in)

	noEmail := func(label string, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, "@") || strings.Contains(s, "corp.example") {
			t.Fatalf("%s carries an email: %s", label, s)
		}
		return string(b)
	}
	reads := func() (domain.NotificationChannel, domain.PhoneDeliveryView, domain.PhoneDeliveryView) {
		t.Helper()
		got := w.get(t, ch.ID)
		byInc, err := w.st.ListIncidentPhoneDeliveries(w.ctx, inc.ID)
		if err != nil || len(byInc) != 1 {
			t.Fatalf("incident read: %d, %v", len(byInc), err)
		}
		byCh, err := w.st.ListChannelPhoneDeliveries(w.ctx, ch.ID, 50)
		if err != nil || len(byCh) != 1 {
			t.Fatalf("channel read: %d, %v", len(byCh), err)
		}
		return got, byInc[0], byCh[0]
	}
	got, d1, d2 := reads()
	if js := noEmail("channel", got.Redacted()); !strings.Contains(js, `"recipient_display_name":"dana.ops"`) {
		t.Fatalf("channel = %s; want the part before '@'", js)
	}
	for label, d := range map[string]domain.PhoneDeliveryView{"incident read": d1, "channel read": d2} {
		if js := noEmail(label, d); !strings.Contains(js, `"recipient_display_name":"dana.ops"`) {
			t.Fatalf("%s = %s; want the part before '@'", label, js)
		}
	}

	// Nothing before the '@': null, not an empty name and not the domain.
	renameUser(t, w.phoneChannelWorld, dana, "@corp.example")
	got, d1, d2 = reads()
	wantRecipientName(t, "leading '@'", got, nil)
	noEmail("channel", got.Redacted())
	for label, d := range map[string]domain.PhoneDeliveryView{"incident read": d1, "channel read": d2} {
		noEmail(label, d)
		if d.RecipientDisplayName != nil {
			t.Fatalf("%s: recipient_display_name = %q; want null", label, *d.RecipientDisplayName)
		}
	}

	// An empty display name (the column's default) is null too, never "" (revision-7 review M-7).
	renameUser(t, w.phoneChannelWorld, dana, "")
	got, d1, d2 = reads()
	wantRecipientName(t, "empty name", got, nil)
	for label, d := range map[string]domain.PhoneDeliveryView{"incident read": d1, "channel read": d2} {
		if d.RecipientDisplayName != nil {
			t.Fatalf("%s: recipient_display_name = %q for an empty name; want null", label, *d.RecipientDisplayName)
		}
	}
}
