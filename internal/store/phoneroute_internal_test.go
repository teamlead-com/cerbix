package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// FR-041a §4.6 / D31 rule 1 (§11 "Store — routes") — a phone channel that cannot deliver is not a
// route, in every place that asks: both halves of routablePredicate, resolveServiceRecipientsTx (the
// schedule's on-call channel and the project's channels) and the resolution of an escalation step's
// channels. The four existing types are judged exactly as before (P14): a DISABLED Slack channel on a
// step is still enqueued.

// phoneRouteWorld is an armed service (armedService) whose project's only channel is the phone
// channel under test, with telephony on under Twilio and an org admin to write the channels.
type phoneRouteWorld struct {
	phoneChannelWorld
	f armFixture
}

func newPhoneRouteWorld(t *testing.T) phoneRouteWorld {
	t.Helper()
	st, ctx := serviceSchemaStore(t)
	return newPhoneRouteWorldOn(t, st, ctx)
}

// newPhoneRouteWorldOn builds the same world on a store the caller owns (a private database).
func newPhoneRouteWorldOn(t *testing.T, st *Store, ctx context.Context) phoneRouteWorld {
	t.Helper()
	c, err := secret.New(randKey(t))
	if err != nil {
		t.Fatal(err)
	}
	st.WithCipher(c)
	f := armedService(t, st, ctx)
	// The fixture's own webhook channel goes: the phone channel under test is the project's only one.
	exec(t, st, ctx, `DELETE FROM notification_channels WHERE project_id = $1`, f.projectID)
	admin := phoneUser(t, st, ctx, "admin@example.com")
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: admin, OrgID: f.orgID, Role: domain.RoleOrgAdmin}); err != nil {
		t.Fatal(err)
	}
	w := phoneRouteWorld{f: f, phoneChannelWorld: phoneChannelWorld{st: st, ctx: ctx, c: c, org: f.orgID,
		project: f.projectID, admin: admin, actor: AuditActor{ActorUserID: admin, Label: "admin@example.com"}}}
	w.telephony(t, true, domain.TelephonyProviderTwilio)
	return w
}

// halves asks each half of routablePredicate on its own about the fixture's service.
func (w phoneRouteWorld) halves(t *testing.T) (schedule, project bool) {
	t.Helper()
	if err := w.st.pool.QueryRow(w.ctx, `SELECT `+routableScheduleSQL+`, `+routableProjectChannelsSQL+`
		  FROM services s WHERE s.id = $1`, w.f.serviceID).Scan(&schedule, &project); err != nil {
		t.Fatalf("routable halves: %v", err)
	}
	return schedule, project
}

// attachSchedule gives the service an on-call schedule whose only participant is channelID.
func (w phoneRouteWorld) attachSchedule(t *testing.T, channelID string) string {
	t.Helper()
	sched, err := w.st.CreateOnCallSchedule(w.ctx, domain.OnCallSchedule{
		ProjectID: w.f.projectID, Name: "primary", ShiftSeconds: 86400,
		Participants: []string{channelID}, AnchorAt: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	exec(t, w.st, w.ctx, `UPDATE services SET oncall_schedule_id = $2 WHERE id = $1`, w.f.serviceID, sched.ID)
	return sched.ID
}

func (w phoneRouteWorld) resolve(t *testing.T, scheduleID string) []string {
	t.Helper()
	tx, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(w.ctx) //nolint:errcheck // read-only probe
	route, err := resolveServiceRecipientsTx(w.ctx, tx, w.f.projectID, scheduleID, time.Now())
	if err != nil {
		t.Fatalf("resolve recipients: %v", err)
	}
	return route
}

// ladderOutcome is what one escalation pass did to a fresh monitor incident.
type ladderOutcome struct {
	steps         []domain.EscalationStepAlert
	step          int
	lastEscalated *time.Time
}

// runLadder opens a monitor incident whose policy has two steps due at once — the first targets the
// given channels, the second the schedule — and runs one escalation pass.
func (w phoneRouteWorld) runLadder(t *testing.T, scheduleID string, channelIDs ...string) ladderOutcome {
	t.Helper()
	var first []domain.EscalationTarget
	for _, id := range channelIDs {
		first = append(first, domain.EscalationTarget{Type: domain.EscalationTargetChannel, ID: id})
	}
	policy, err := w.st.CreateEscalationPolicy(w.ctx, domain.EscalationPolicy{
		ProjectID: w.f.projectID, Name: "ladder", Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: first},
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetSchedule, ID: scheduleID}}},
		},
	})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	mon, err := w.st.CreateMonitor(w.ctx, domain.Monitor{
		RecoveryThreshold: 1, ProjectID: w.f.projectID, Name: "ladder", Type: domain.MonitorHTTP,
		Target: "https://ladder.example.com/", IntervalSeconds: 60, TimeoutSeconds: 5, Region: "core",
		Enabled: true, AutoIncident: true, EscalationPolicyID: policy.ID,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	exec(t, w.st, w.ctx, `UPDATE monitors SET status = 'down' WHERE id = $1`, mon.ID)
	inc, err := w.st.CreateIncidentBySystem(w.ctx, domain.Incident{
		ProjectID: w.f.projectID, MonitorID: mon.ID, Title: "down",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}, "auto opened", "system")
	if err != nil {
		t.Fatalf("incident: %v", err)
	}
	if _, err := w.st.AdvanceEscalations(w.ctx); err != nil {
		t.Fatalf("advance: %v", err)
	}
	var out ladderOutcome
	if err := w.st.pool.QueryRow(w.ctx, `SELECT escalation_step, last_escalated_at FROM incidents WHERE id = $1`, inc.ID).
		Scan(&out.step, &out.lastEscalated); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	rows, err := w.st.pool.Query(w.ctx, `SELECT payload FROM outbox_events WHERE topic = $1 ORDER BY created_at, id`,
		domain.TopicEscalationStep)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var a domain.EscalationStepAlert
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		out.steps = append(out.steps, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// One pass enqueues both steps in one transaction, so created_at does not order them.
	sort.Slice(out.steps, func(i, j int) bool { return out.steps[i].Step < out.steps[j].Step })
	return out
}

// Each way a phone channel is not a route × every place that asks. A deliverable phone channel is the
// control: it is a route everywhere, so no refusal below can come from the fixture.
func TestPhoneChannelThatCannotDeliverIsNotARoute(t *testing.T) {
	cases := []struct {
		name   string
		reason string // "" for the control
		build  func(t *testing.T, w phoneRouteWorld) string
	}{
		{"deliverable", "", func(t *testing.T, w phoneRouteWorld) string {
			alice := w.member(t, "alice@example.com", domain.RoleEditor)
			w.verifiedPhone(t, alice, phoneA)
			return w.userChannel(t, domain.ChannelSMS, alice).ID
		}},
		{"number unverified", domain.DeliverableNumberUnverified, func(t *testing.T, w phoneRouteWorld) string {
			ch := w.numberChannel(t, domain.ChannelSMS, phoneB)
			// Enabled by hand — the writers refuse it — so the enabled check alone cannot be what
			// refuses the channel below.
			exec(t, w.st, w.ctx, `UPDATE notification_channels SET enabled = true WHERE id = $1`, ch.ID)
			return ch.ID
		}},
		{"user without a verified number", domain.DeliverableNoVerifiedNumber, func(t *testing.T, w phoneRouteWorld) string {
			bob := w.member(t, "bob@example.com", domain.RoleEditor)
			return w.userChannel(t, domain.ChannelSMS, bob).ID
		}},
		{"user not eligible", domain.DeliverableRecipientNotEligible, func(t *testing.T, w phoneRouteWorld) string {
			dave := w.member(t, "dave@example.com", domain.RoleViewer)
			w.verifiedPhone(t, dave, phoneA)
			id := w.userChannel(t, domain.ChannelVoice, dave).ID
			exec(t, w.st, w.ctx, `DELETE FROM memberships WHERE user_id = $1`, dave)
			return id
		}},
		{"user deleted", domain.DeliverableRecipientDeleted, func(t *testing.T, w phoneRouteWorld) string {
			carol := w.member(t, "carol@example.com", domain.RoleViewer)
			w.verifiedPhone(t, carol, phoneA)
			id := w.userChannel(t, domain.ChannelSMS, carol).ID
			if err := w.st.DeleteUser(w.ctx, carol); err != nil {
				t.Fatal(err)
			}
			// The row a user deletion leaves when nothing disabled the channel first (the foreign
			// key alone, as an older binary deletes users): no recipient, still enabled.
			exec(t, w.st, w.ctx, `UPDATE notification_channels SET enabled = true, disabled_reason = NULL WHERE id = $1`, id)
			return id
		}},
		{"telephony off", domain.DeliverableTelephonyDisabled, func(t *testing.T, w phoneRouteWorld) string {
			alice := w.member(t, "alice@example.com", domain.RoleEditor)
			w.verifiedPhone(t, alice, phoneA)
			id := w.userChannel(t, domain.ChannelSMS, alice).ID
			w.telephony(t, false, domain.TelephonyProviderTwilio)
			return id
		}},
		{"voice under generic_http", domain.DeliverableVoiceUnsupportedByProvider, func(t *testing.T, w phoneRouteWorld) string {
			alice := w.member(t, "alice@example.com", domain.RoleEditor)
			w.verifiedPhone(t, alice, phoneA)
			id := w.userChannel(t, domain.ChannelVoice, alice).ID
			w.telephony(t, true, domain.TelephonyProviderGenericHTTP)
			return id
		}},
		// FR-041a review P1-1: a Messaging Service SID sends messages only (D3).
		{"voice under twilio with a Messaging Service SID", domain.DeliverableVoiceUnsupportedByProvider, func(t *testing.T, w phoneRouteWorld) string {
			alice := w.member(t, "alice@example.com", domain.RoleEditor)
			w.verifiedPhone(t, alice, phoneA)
			id := w.userChannel(t, domain.ChannelVoice, alice).ID
			w.twilioFrom(t, testMessagingServiceSID)
			return id
		}},
		{"disabled", domain.DeliverableDisabled, func(t *testing.T, w phoneRouteWorld) string {
			alice := w.member(t, "alice@example.com", domain.RoleEditor)
			w.verifiedPhone(t, alice, phoneA)
			id := w.userChannel(t, domain.ChannelSMS, alice).ID
			if _, err := w.patch(id, PhoneChannelPatch{Enabled: ptr(false)}); err != nil {
				t.Fatal(err)
			}
			return id
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newPhoneRouteWorld(t)
			id := tc.build(t, w)
			// The case builds what it says: the DTO names the reason (or none).
			wantDeliverable(t, "built channel", w.get(t, id), tc.reason)
			isRoute := tc.reason == ""
			var wantRoute []string
			if isRoute {
				wantRoute = []string{id}
			}

			// The project half, with no schedule: and through it the delegation lookup and the badge.
			if _, project := w.halves(t); project != isRoute {
				t.Fatalf("routablePredicate project half = %v, want %v", project, isRoute)
			}
			if got := delegated(t, w.st, w.ctx, w.f, DelegationLive); got != isRoute {
				t.Fatalf("ActiveDelegation suppresses = %v, want %v", got, isRoute)
			}
			wantBadge := AlertReasonUnroutable
			if isRoute {
				wantBadge = ""
			}
			if got := badgeReason(t, w.st, w.ctx, w.f.projectID, w.f.serviceID); got != wantBadge {
				t.Fatalf("badge reason = %q, want %q", got, wantBadge)
			}
			// The resolver's project channels.
			if got := w.resolve(t, ""); !reflect.DeepEqual(got, wantRoute) {
				t.Fatalf("project channels resolved %v, want %v", got, wantRoute)
			}

			// The schedule half and the resolver's on-call check.
			sched := w.attachSchedule(t, id)
			if schedule, _ := w.halves(t); schedule != isRoute {
				t.Fatalf("routablePredicate schedule half = %v, want %v", schedule, isRoute)
			}
			if got := w.resolve(t, sched); !reflect.DeepEqual(got, wantRoute) {
				t.Fatalf("on-call channel resolved %v, want %v", got, wantRoute)
			}

			// Step resolution: a channel target and a schedule target, both due now.
			got := w.runLadder(t, sched, id)
			if got.step != 2 {
				t.Fatalf("the ladder stopped at step %d, want it advanced past both steps", got.step)
			}
			if !isRoute {
				if len(got.steps) != 0 || got.lastEscalated != nil {
					t.Fatalf("steps of non-route phones enqueued %+v (last_escalated_at %v), want nothing, "+
						"as a step with no channel", got.steps, got.lastEscalated)
				}
				return
			}
			if len(got.steps) != 2 || got.lastEscalated == nil {
				t.Fatalf("a deliverable phone channel's steps: %+v (last_escalated_at %v), want two", got.steps, got.lastEscalated)
			}
			for i, s := range got.steps {
				if s.Step != i || !reflect.DeepEqual(s.ChannelIDs, []string{id}) {
					t.Fatalf("step %d = %+v, want step %d to channel %s", i, s, i, id)
				}
			}
		})
	}
}

// P14 — the condition is for sms/voice rows only. A DISABLED Slack channel on a step is enqueued
// exactly as before, its id in the payload, and moves last_escalated_at; a non-route phone beside it
// is dropped from the same step.
func TestADisabledSlackChannelOnAStepIsStillEnqueued(t *testing.T) {
	w := newPhoneRouteWorld(t)
	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.f.projectID,
		Type: domain.ChannelSlack, Name: "slack", Config: map[string]string{"url": "https://hooks.example/x"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.SetNotificationChannelEnabled(w.ctx, slack.ID, false); err != nil {
		t.Fatal(err)
	}
	bob := w.member(t, "bob@example.com", domain.RoleEditor)
	phone := w.userChannel(t, domain.ChannelSMS, bob) // no verified number: not a route
	sched := w.attachSchedule(t, slack.ID)

	got := w.runLadder(t, sched, slack.ID, phone.ID)
	if got.step != 2 || got.lastEscalated == nil || len(got.steps) != 2 {
		t.Fatalf("ladder = %+v, want both steps enqueued and last_escalated_at moved", got)
	}
	for i, s := range got.steps {
		if s.Step != i || !reflect.DeepEqual(s.ChannelIDs, []string{slack.ID}) {
			t.Fatalf("step %d = %+v, want the disabled Slack channel %s alone", i, s, slack.ID)
		}
	}
	// The other predicates still require `enabled` of a Slack channel, as before.
	if schedule, project := w.halves(t); schedule || project {
		t.Fatalf("a disabled Slack channel made the service routable (schedule %v, project %v)", schedule, project)
	}
}

// P20 — a service whose only channel is a phone that cannot deliver withholds its onset as an
// unroutable service does; making the channel deliverable produces the announcement, to it.
func TestAServiceWhoseOnlyChannelIsAPhoneAnnouncesWhenItBecomesARoute(t *testing.T) {
	w := newPhoneRouteWorld(t)
	exec(t, w.st, w.ctx, `DELETE FROM service_alert_state`)
	f := alertFixture{projectID: w.f.projectID, serviceID: w.f.serviceID, monitorID: w.f.monitorID}
	bob := w.member(t, "bob@example.com", domain.RoleEditor)
	phone := w.userChannel(t, domain.ChannelSMS, bob) // no verified number yet

	setMemberHealth(t, w.st, w.ctx, f, true)
	evalOnce(t, w.st, w.ctx)
	setMemberHealth(t, w.st, w.ctx, f, false)
	evalOnce(t, w.st, w.ctx)
	got := evalOnce(t, w.st, w.ctx)
	if got.Onsets != 0 || got.Withheld[WithheldUnroutable] != 1 {
		t.Fatalf("a service whose only channel cannot deliver: %+v, want the onset withheld as unroutable", got)
	}
	if len(alertEvents(t, w.st, w.ctx)) != 0 {
		t.Fatal("a service alert was enqueued to a phone channel that is not a route")
	}

	w.verifiedPhone(t, bob, phoneA)
	got = evalOnce(t, w.st, w.ctx)
	if got.Onsets != 1 {
		t.Fatalf("after the channel became deliverable the evaluation reported %+v, want the onset", got)
	}
	events := alertEvents(t, w.st, w.ctx)
	if len(events) != 1 || !reflect.DeepEqual(events[0].Recipients, []string{phone.ID}) {
		t.Fatalf("announcement = %+v, want one to the phone channel %s", events, phone.ID)
	}
}

// FR-041a review P1-1 (§11 "Code review of the backend"): a voice channel under Twilio whose `from` is
// a Messaging Service SID is refused at creation and at enabling, is `deliverable: false` with
// `voice_unsupported_by_provider`, is no route at send, and a settings save that makes it so answers
// the 409 of §4.1 — one condition held beside telephony_provider (D3), read everywhere. The two
// predicates and the resolver are TestPhoneChannelThatCannotDeliverIsNotARoute's case of the same name.
func TestVoiceUnderTwilioMessagingServiceIsNotARoute(t *testing.T) {
	w := newPhoneChannelWorld(t)
	alice := w.member(t, "alice@example.com", domain.RoleEditor)
	w.verifiedPhone(t, alice, phoneA)
	voiceOff := w.userChannel(t, domain.ChannelVoice, alice)
	if _, err := w.patch(voiceOff.ID, PhoneChannelPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	voiceOn := w.userChannel(t, domain.ChannelVoice, alice)
	sms := w.userChannel(t, domain.ChannelSMS, alice)

	canPlaceCalls := func() bool {
		t.Helper()
		var v bool
		if err := w.st.pool.QueryRow(w.ctx, `SELECT telephony_can_place_calls FROM instance_settings WHERE id = true`).Scan(&v); err != nil {
			t.Fatalf("read telephony_can_place_calls: %v", err)
		}
		return v
	}

	// A number as `from`: calls are placed.
	w.twilioFrom(t, "+15005550006")
	if !canPlaceCalls() {
		t.Fatal("twilio with a number as from: telephony_can_place_calls = false")
	}
	wantDeliverable(t, "voice, from a number", w.get(t, voiceOn.ID), "")

	w.twilioFrom(t, testMessagingServiceSID)
	if canPlaceCalls() {
		t.Fatal("twilio with a Messaging Service SID: telephony_can_place_calls = true")
	}
	create := func(typ domain.ChannelType) error {
		_, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: typ,
			Name: "new " + string(typ), Recipient: domain.PhoneRecipientUser, RecipientUserID: alice, Enabled: true}, "", w.actor)
		return err
	}
	if err := create(domain.ChannelVoice); !errors.Is(err, domain.ErrPhoneVoiceUnsupported) {
		t.Fatalf("create voice under a Messaging Service SID: %v, want ErrPhoneVoiceUnsupported", err)
	}
	if err := create(domain.ChannelSMS); err != nil {
		t.Fatalf("create sms under a Messaging Service SID: %v", err)
	}
	if _, err := w.patch(voiceOff.ID, PhoneChannelPatch{Enabled: ptr(true)}); !errors.Is(err, domain.ErrPhoneVoiceUnsupported) {
		t.Fatalf("enable voice under a Messaging Service SID: %v, want ErrPhoneVoiceUnsupported", err)
	}
	wantDeliverable(t, "voice, from a Messaging Service", w.get(t, voiceOn.ID), domain.DeliverableVoiceUnsupportedByProvider)
	wantDeliverable(t, "sms, from a Messaging Service", w.get(t, sms.ID), "")

	// At send: the target's verdict is the reason the outbox records.
	targets, err := w.st.PhoneTargets(w.ctx, []string{voiceOn.ID, sms.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].NotRouteReason != domain.DeliverableVoiceUnsupportedByProvider || !targets[1].Route() {
		t.Fatalf("targets at send = %+v, want the voice channel refused with %q and the sms channel a route",
			targets, domain.DeliverableVoiceUnsupportedByProvider)
	}
}

// The 409 of §4.1 judges the PROPOSED settings: switching Twilio's `from` to a Messaging Service SID
// lists the services whose only route is a voice channel, and changes nothing.
func TestTelephonySaveToMessagingServiceAnswersServicesWithoutRoute(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	org, err := st.CreateOrganization(ctx, "routes", "Routes")
	if err != nil {
		t.Fatal(err)
	}
	routeFixture(t, st, ctx, org.ID, "Alpha", true, []string{"alpha-pager"}, "sms")
	routeFixture(t, st, ctx, org.ID, "Delta", true, []string{"delta-voice"}, "voice")
	if err := st.SaveTelephonySettings(ctx, twilioTS(true, tsAuthToken), domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	before, _ := readTelephonyRow(t, st, ctx)
	mg := twilioTS(true, "")
	mg.Twilio.From = testMessagingServiceSID
	err = st.SaveTelephonySettings(ctx, mg, domain.TelephonySaveOptions{})
	var sw *domain.ServicesWithoutRouteError
	if !errors.As(err, &sw) {
		t.Fatalf("save with a Messaging Service SID: err = %v, want ServicesWithoutRouteError", err)
	}
	want := []domain.ServiceWithoutRoute{{Project: "Delta", Service: "DELTA-VOICE", Slug: "delta-voice"}}
	if sw.Total != 1 || !reflect.DeepEqual(sw.Services, want) {
		t.Fatalf("services = %v (total %d), want %v", sw.Services, sw.Total, want)
	}
	if after, _ := readTelephonyRow(t, st, ctx); !reflect.DeepEqual(before, after) {
		t.Fatal("the 409 changed the stored settings")
	}
	if err := st.SaveTelephonySettings(ctx, mg, domain.TelephonySaveOptions{ConfirmUnroutable: true}); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
}
