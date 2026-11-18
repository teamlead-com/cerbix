package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// FR-041a §4.2/§4.3 — phone channels in the store: the typed columns, the gates, the eligibility of
// a `user` channel's recipient, a `number` channel's verification, the audit rows, D31 rule 1 as the
// DTO's `deliverable`, user deletion, reencrypt and the lock order of every phone-number write.

type phoneChannelWorld struct {
	st      *Store
	ctx     context.Context
	c       *secret.Cipher
	org     string
	project string
	admin   string // org admin of the project's org: the actor of the writes
	actor   AuditActor
}

func newPhoneChannelWorld(t *testing.T) phoneChannelWorld {
	t.Helper()
	st, ctx, c := telephonyStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatal(err)
	}
	admin := phoneUser(t, st, ctx, "admin@example.com")
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: admin, OrgID: org.ID, Role: domain.RoleOrgAdmin}); err != nil {
		t.Fatal(err)
	}
	w := phoneChannelWorld{st: st, ctx: ctx, c: c, org: org.ID, project: proj.ID, admin: admin,
		actor: AuditActor{ActorUserID: admin, Label: "admin@example.com"}}
	w.telephony(t, true, domain.TelephonyProviderTwilio)
	return w
}

// telephony sets the two typed columns D31 reads, as a settings save leaves them.
func (w phoneChannelWorld) telephony(t *testing.T, enabled bool, provider string) {
	t.Helper()
	if _, err := w.st.pool.Exec(w.ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE instance_settings SET telephony_enabled = $1, telephony_provider = $2 WHERE id = true`,
		enabled, provider); err != nil {
		t.Fatal(err)
	}
}

// testMessagingServiceSID is a Twilio Messaging Service SID: it sends messages and places no call (D3).
const testMessagingServiceSID = "MG0123456789abcdef0123456789abcdef"

// twilioFrom puts telephony on under Twilio with from as the stored `from`, by hand in the document as
// a settings save leaves it — the condition "the provider can place calls" is read from the row.
func (w phoneChannelWorld) twilioFrom(t *testing.T, from string) {
	t.Helper()
	w.telephony(t, true, domain.TelephonyProviderTwilio)
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE instance_settings
		   SET telephony = jsonb_set(telephony, '{twilio}', jsonb_build_object('from', $1::text)) WHERE id = true`,
		from); err != nil {
		t.Fatal(err)
	}
}

func (w phoneChannelWorld) member(t *testing.T, email string, role domain.Role) string {
	t.Helper()
	uid := phoneUser(t, w.st, w.ctx, email)
	if _, err := w.st.CreateProjectMembership(w.ctx, w.project, uid, role); err != nil {
		t.Fatal(err)
	}
	return uid
}

func (w phoneChannelWorld) verifiedPhone(t *testing.T, userID, number string) {
	t.Helper()
	code := mustSetPhone(t, w.st, w.ctx, userID, number)
	if _, err := w.st.VerifyUserPhone(w.ctx, userID, code); err != nil {
		t.Fatal(err)
	}
}

func (w phoneChannelWorld) userChannel(t *testing.T, typ domain.ChannelType, recipient string) domain.NotificationChannel {
	t.Helper()
	ch, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: typ,
		Name: string(typ) + " " + recipient, Recipient: domain.PhoneRecipientUser, RecipientUserID: recipient, Enabled: true}, "", w.actor)
	if err != nil {
		t.Fatalf("create user channel: %v", err)
	}
	return ch
}

func (w phoneChannelWorld) numberChannel(t *testing.T, typ domain.ChannelType, number string) domain.NotificationChannel {
	t.Helper()
	ch, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: typ,
		Name: "duty " + string(typ), Recipient: domain.PhoneRecipientNumber, Enabled: true}, number, w.actor)
	if err != nil {
		t.Fatalf("create number channel: %v", err)
	}
	return ch
}

// verifyNumberChannel sends the channel a code and checks it, as the two routes do.
func (w phoneChannelWorld) verifyNumberChannel(t *testing.T, id string) domain.NotificationChannel {
	t.Helper()
	_, code, err := w.st.SendChannelVerificationCode(w.ctx, id, w.admin)
	if err != nil {
		t.Fatalf("send channel code: %v", err)
	}
	ch, err := w.st.VerifyChannelNumber(w.ctx, id, code, w.actor)
	if err != nil {
		t.Fatalf("verify channel: %v", err)
	}
	return ch
}

func (w phoneChannelWorld) patch(id string, p PhoneChannelPatch) (domain.NotificationChannel, error) {
	return w.st.UpdatePhoneChannel(w.ctx, id, p, w.actor, nil)
}

func (w phoneChannelWorld) get(t *testing.T, id string) domain.NotificationChannel {
	t.Helper()
	ch, err := w.st.GetNotificationChannel(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func (w phoneChannelWorld) audits(t *testing.T, action string) []string {
	t.Helper()
	rows, err := w.st.pool.Query(w.ctx, `SELECT target FROM audit_logs WHERE action = $1 ORDER BY created_at, id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatal(err)
		}
		out = append(out, target)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func wantDeliverable(t *testing.T, label string, ch domain.NotificationChannel, reason string) {
	t.Helper()
	if ch.Deliverable == nil {
		t.Fatalf("%s: a phone channel without deliverable: %+v", label, ch)
	}
	if want := (domain.ChannelDeliverable{Route: reason == "", Reason: reason}); *ch.Deliverable != want {
		t.Fatalf("%s: deliverable = %+v, want %+v", label, *ch.Deliverable, want)
	}
}

func TestPhoneChannelNumberCreatedDisabledAndNotEnabledBeforeVerification(t *testing.T) {
	w := newPhoneChannelWorld(t)
	scriptPhoneCodes(t, "482193")

	ch := w.numberChannel(t, domain.ChannelSMS, "+90 532 111 68 69")
	if ch.Enabled || ch.DisabledReason != domain.DeliverableNumberUnverified || ch.Recipient != domain.PhoneRecipientNumber ||
		ch.NumberMasked != "+90•••••6869" || ch.NumberVerified == nil || *ch.NumberVerified || ch.RecipientUserID != "" {
		t.Fatalf("created number channel = %+v; want disabled, number_unverified, masked, unverified", ch)
	}
	wantDeliverable(t, "created", ch, domain.DeliverableNumberUnverified)

	var enc, hash string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT number_enc, number_hash FROM notification_channels WHERE id = $1`, ch.ID).
		Scan(&enc, &hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, phoneADigits) || strings.Contains(hash, phoneADigits) {
		t.Fatal("a stored column carries the number")
	}
	if pt, err := w.c.DecryptBytes(enc, secret.CanonicalAAD("channel_number", ch.ID)); err != nil || string(pt) != phoneA {
		t.Fatalf("number_enc opens to %q, %v; want the number under AAD(channel_number, id)", pt, err)
	}
	if want, _ := w.c.KeyedHash("phone-number", []byte(phoneA)); hash != want {
		t.Fatal("number_hash is not KeyedHash(phone-number, number)")
	}

	if _, err := w.patch(ch.ID, PhoneChannelPatch{Enabled: ptr(true)}); !errors.Is(err, domain.ErrPhoneNumberUnverified) {
		t.Fatalf("enable before verification: err = %v, want ErrPhoneNumberUnverified", err)
	}
	if got := w.get(t, ch.ID); got.Enabled {
		t.Fatal("the refused enable enabled the channel")
	}

	number, code, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin)
	if err != nil || number != phoneA || code != "482193" {
		t.Fatalf("send = %q, %q, %v; want the stored number and the scripted code", number, code, err)
	}
	if n := verificationRows(t, w.st, w.ctx, domain.PhoneSubjectChannel, ch.ID); n != 1 {
		t.Fatalf("channel verification rows = %d", n)
	}
	if _, err := w.st.VerifyChannelNumber(w.ctx, ch.ID, "000000", w.actor); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("wrong code: %v", err)
	}
	verified, err := w.st.VerifyChannelNumber(w.ctx, ch.ID, "482193", w.actor)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Enabled || verified.NumberVerified == nil || !*verified.NumberVerified || verified.DisabledReason != "" {
		t.Fatalf("verified = %+v; want verified, still disabled, no disabled_reason", verified)
	}
	wantDeliverable(t, "verified, disabled", verified, domain.DeliverableDisabled)

	enabled, err := w.patch(ch.ID, PhoneChannelPatch{Enabled: ptr(true)})
	if err != nil || !enabled.Enabled {
		t.Fatalf("enable after verification: %+v, %v", enabled, err)
	}
	wantDeliverable(t, "enabled", enabled, "")

	target := "channel " + ch.ID + " · sms"
	for action, want := range map[string]int{"notification_channel.create": 1, "notification_channel.verify": 1,
		"notification_channel.update": 1} {
		got := w.audits(t, action)
		if len(got) != want {
			t.Fatalf("%s rows = %v, want %d", action, got, want)
		}
		for _, tg := range got {
			if tg != target {
				t.Fatalf("%s target = %q, want %q", action, tg, target)
			}
		}
	}
}

func TestPhoneChannelNumberChangeClearsVerificationAndDisables(t *testing.T) {
	w := newPhoneChannelWorld(t)
	ch := w.numberChannel(t, domain.ChannelVoice, phoneA)
	w.verifyNumberChannel(t, ch.ID)
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}

	// The same number in another spelling is not a change.
	same, err := w.patch(ch.ID, PhoneChannelPatch{Number: ptr("+90 (532) 111-68-69")})
	if err != nil || !same.Enabled || !*same.NumberVerified {
		t.Fatalf("same number: %+v, %v; want unchanged", same, err)
	}

	changed, err := w.patch(ch.ID, PhoneChannelPatch{Number: ptr(phoneB)})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Enabled || *changed.NumberVerified || changed.DisabledReason != domain.DeliverableNumberUnverified ||
		changed.NumberMasked != "+44•••••0199" {
		t.Fatalf("after a number change = %+v; want disabled, unverified, number_unverified, the new mask", changed)
	}
	var enc string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT number_enc FROM notification_channels WHERE id = $1`, ch.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if pt, err := w.c.DecryptBytes(enc, secret.CanonicalAAD("channel_number", ch.ID)); err != nil || string(pt) != phoneB {
		t.Fatalf("number_enc opens to %q, %v", pt, err)
	}

	// A code in flight for the old number must not verify the new one. (A verified channel is sent
	// no code, FR-041a review P2-3, so the code is sent to the now unverified number.)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin); err != nil {
		t.Fatal(err)
	}
	// A number change and an enable in one write: the change wins, the enable is refused.
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Number: ptr(phoneA), Enabled: ptr(true)}); !errors.Is(err, domain.ErrPhoneNumberUnverified) {
		t.Fatalf("number change + enable: err = %v, want ErrPhoneNumberUnverified", err)
	}
	if got := w.get(t, ch.ID); got.NumberMasked != "+44•••••0199" {
		t.Fatal("the refused write changed the number")
	}
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Number: ptr(phoneA)}); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_verifications
		  WHERE subject_kind = 'channel' AND subject_id = $1 AND expires_at > now()`, ch.ID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active codes after the number change = %d, %v; want none", active, err)
	}
}

func TestPhoneChannelRecipientEligibility(t *testing.T) {
	w := newPhoneChannelWorld(t)
	viewer := w.member(t, "viewer@example.com", domain.RoleViewer)
	orgEditor := phoneUser(t, w.st, w.ctx, "org-editor@example.com")
	if _, err := w.st.CreateMembership(w.ctx, domain.Membership{UserID: orgEditor, OrgID: w.org, Role: domain.RoleEditor}); err != nil {
		t.Fatal(err)
	}
	globalAdmin, err := w.st.CreateLocalUser(w.ctx, "ga@example.com", "GA", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	outsider := phoneUser(t, w.st, w.ctx, "outsider@example.com")
	var machine string
	if err := w.st.pool.QueryRow(w.ctx, `INSERT INTO users (oidc_sub, email, kind) VALUES ('m1', 'm@example.com', 'machine')
		RETURNING id::text`).Scan(&machine); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.CreateProjectMembership(w.ctx, w.project, machine, domain.RoleEditor); err != nil {
		t.Fatal(err)
	}

	for _, uid := range []string{viewer, orgEditor, globalAdmin.ID, w.admin} {
		w.userChannel(t, domain.ChannelSMS, uid)
	}
	for name, uid := range map[string]string{"outsider": outsider, "machine member": machine, "nobody": "",
		"not a uuid": "alice", "unknown": "00000000-0000-0000-0000-000000000001"} {
		_, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSMS,
			Name: name, Recipient: domain.PhoneRecipientUser, RecipientUserID: uid, Enabled: true}, "", w.actor)
		if !errors.Is(err, domain.ErrPhoneRecipientNotEligible) {
			t.Fatalf("%s: err = %v, want ErrPhoneRecipientNotEligible", name, err)
		}
	}

	ch := w.userChannel(t, domain.ChannelSMS, viewer)
	if _, err := w.patch(ch.ID, PhoneChannelPatch{RecipientUserID: ptr(outsider)}); !errors.Is(err, domain.ErrPhoneRecipientNotEligible) {
		t.Fatalf("retarget to an outsider: %v", err)
	}
	if _, err := w.patch(ch.ID, PhoneChannelPatch{RecipientUserID: ptr("")}); !errors.Is(err, domain.ErrPhoneRecipientNotEligible) {
		t.Fatalf("retarget to nobody: %v", err)
	}
	moved, err := w.patch(ch.ID, PhoneChannelPatch{RecipientUserID: ptr(orgEditor)})
	if err != nil || moved.RecipientUserID != orgEditor {
		t.Fatalf("retarget to an org member: %+v, %v", moved, err)
	}

	// The check runs at every update: a recipient who lost access blocks an edit — but never a pause.
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM memberships WHERE user_id = $1`, orgEditor); err != nil {
		t.Fatal(err)
	}
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Name: ptr("renamed")}); !errors.Is(err, domain.ErrPhoneRecipientNotEligible) {
		t.Fatalf("edit with an ineligible recipient: %v", err)
	}
	paused, err := w.patch(ch.ID, PhoneChannelPatch{Enabled: ptr(false)})
	if err != nil || paused.Enabled {
		t.Fatalf("pause with an ineligible recipient: %+v, %v", paused, err)
	}
}

func TestPhoneChannelRecipientModeIsFixed(t *testing.T) {
	w := newPhoneChannelWorld(t)
	user := w.userChannel(t, domain.ChannelSMS, w.admin)
	number := w.numberChannel(t, domain.ChannelSMS, phoneA)
	if _, err := w.patch(user.ID, PhoneChannelPatch{Number: ptr(phoneB)}); !errors.Is(err, domain.ErrPhoneRecipientFixed) {
		t.Fatalf("number on a user channel: %v", err)
	}
	if _, err := w.patch(number.ID, PhoneChannelPatch{RecipientUserID: ptr(w.admin)}); !errors.Is(err, domain.ErrPhoneRecipientFixed) {
		t.Fatalf("recipient on a number channel: %v", err)
	}
	if _, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSMS,
		Name: "both", Recipient: domain.PhoneRecipientUser, RecipientUserID: w.admin}, phoneA, w.actor); !errors.Is(err, domain.ErrPhoneRecipientFixed) {
		t.Fatalf("a user channel with a number: %v", err)
	}
	// The generic writers refuse the phone types: their columns need the phone writers.
	if _, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSMS,
		Name: "raw", Recipient: domain.PhoneRecipientNumber}); err == nil {
		t.Fatal("CreateNotificationChannel accepted an sms channel")
	}
	if _, err := w.st.UpdateNotificationChannel(w.ctx, user); err == nil {
		t.Fatal("UpdateNotificationChannel accepted an sms channel")
	}
}

func TestPhoneChannelGates(t *testing.T) {
	w := newPhoneChannelWorld(t)
	create := func(typ domain.ChannelType) error {
		_, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: typ,
			Name: "x", Recipient: domain.PhoneRecipientUser, RecipientUserID: w.admin, Enabled: true}, "", w.actor)
		return err
	}
	voiceOff := w.userChannel(t, domain.ChannelVoice, w.admin)
	if _, err := w.patch(voiceOff.ID, PhoneChannelPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}

	w.telephony(t, false, domain.TelephonyProviderTwilio)
	if err := create(domain.ChannelSMS); !errors.Is(err, domain.ErrPhoneTelephonyDisabled) {
		t.Fatalf("create with telephony off: %v", err)
	}
	if _, err := w.patch(voiceOff.ID, PhoneChannelPatch{Enabled: ptr(true)}); !errors.Is(err, domain.ErrPhoneTelephonyDisabled) {
		t.Fatalf("enable with telephony off: %v", err)
	}
	if _, err := w.patch(voiceOff.ID, PhoneChannelPatch{Name: ptr("renamed while off")}); err != nil {
		t.Fatalf("a rename needs no telephony: %v", err)
	}

	w.telephony(t, true, domain.TelephonyProviderGenericHTTP)
	if err := create(domain.ChannelVoice); !errors.Is(err, domain.ErrPhoneVoiceUnsupported) {
		t.Fatalf("voice under generic_http: %v", err)
	}
	if err := create(domain.ChannelSMS); err != nil {
		t.Fatalf("sms under generic_http: %v", err)
	}
	if _, err := w.patch(voiceOff.ID, PhoneChannelPatch{Enabled: ptr(true)}); !errors.Is(err, domain.ErrPhoneVoiceUnsupported) {
		t.Fatalf("enable voice under generic_http: %v", err)
	}
	if n := len(w.audits(t, "notification_channel.create")); n != 2 {
		t.Fatalf("create audit rows = %d, want only the two creates that happened", n)
	}

	w.st.WithCipher(nil)
	if err := create(domain.ChannelSMS); !errors.Is(err, domain.ErrPhoneKeyMissing) {
		t.Fatalf("create without the at-rest key: %v", err)
	}
}

func TestPhoneChannelDeliverableReasons(t *testing.T) {
	w := newPhoneChannelWorld(t)
	alice := w.member(t, "alice@example.com", domain.RoleEditor)
	w.verifiedPhone(t, alice, phoneA)
	bob := w.member(t, "bob@example.com", domain.RoleEditor) // no phone

	route := w.userChannel(t, domain.ChannelSMS, alice)
	voice := w.userChannel(t, domain.ChannelVoice, alice)
	noNumber := w.userChannel(t, domain.ChannelSMS, bob)
	paused := w.userChannel(t, domain.ChannelSMS, alice)
	if _, err := w.patch(paused.ID, PhoneChannelPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	unverified := w.numberChannel(t, domain.ChannelSMS, phoneB)
	carol := w.member(t, "carol@example.com", domain.RoleViewer)
	w.verifiedPhone(t, carol, "+12025550147")
	gone := w.userChannel(t, domain.ChannelSMS, carol)
	dave := w.member(t, "dave@example.com", domain.RoleViewer)
	w.verifiedPhone(t, dave, "+12025550148")
	ineligible := w.userChannel(t, domain.ChannelSMS, dave)
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM memberships WHERE user_id = $1`, dave); err != nil {
		t.Fatal(err)
	}
	if err := w.st.DeleteUser(w.ctx, carol); err != nil {
		t.Fatal(err)
	}
	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSlack,
		Name: "slack", Config: map[string]string{"url": "https://hooks.example/x"}})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{route.ID: "", voice.ID: "", noNumber.ID: domain.DeliverableNoVerifiedNumber,
		paused.ID: domain.DeliverableDisabled, unverified.ID: domain.DeliverableNumberUnverified,
		gone.ID: domain.DeliverableRecipientDeleted, ineligible.ID: domain.DeliverableRecipientNotEligible}
	check := func(label string, want map[string]string) {
		t.Helper()
		list, err := w.st.ListChannelsByProject(w.ctx, w.project)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range list {
			if ch.ID == slack.ID {
				if ch.Deliverable != nil || ch.Recipient != "" || ch.NumberVerified != nil {
					t.Fatalf("%s: a slack channel carries phone fields: %+v", label, ch)
				}
				continue
			}
			wantDeliverable(t, label+" "+ch.Name, ch, want[ch.ID])
		}
		// The predicate form of the same fragment agrees with the DTO on every row, and is TRUE for
		// a disabled Slack channel (the four other types are judged as before).
		rows, err := w.st.pool.Query(w.ctx, `SELECT c.id::text, `+phoneRouteSQL("c")+` FROM notification_channels c`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var isRoute bool
			if err := rows.Scan(&id, &isRoute); err != nil {
				t.Fatal(err)
			}
			if id != slack.ID && isRoute != (want[id] == "") {
				t.Fatalf("%s: phoneRouteSQL(%s) = %v, want reason %q", label, id, isRoute, want[id])
			}
			if id == slack.ID && !isRoute {
				t.Fatalf("%s: phoneRouteSQL is FALSE for a Slack channel", label)
			}
		}
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE notification_channels SET enabled = false WHERE id = $1`, slack.ID); err != nil {
		t.Fatal(err)
	}
	check("twilio", want)

	w.telephony(t, true, domain.TelephonyProviderGenericHTTP)
	want[voice.ID] = domain.DeliverableVoiceUnsupportedByProvider
	check("generic_http", want)

	w.telephony(t, false, domain.TelephonyProviderTwilio)
	for _, id := range []string{route.ID, voice.ID, noNumber.ID, ineligible.ID} {
		want[id] = domain.DeliverableTelephonyDisabled
	}
	check("telephony off", want)
}

func TestDeleteUserDisablesTheirPhoneChannels(t *testing.T) {
	w := newPhoneChannelWorld(t)
	alice := w.member(t, "alice@example.com", domain.RoleEditor)
	w.verifiedPhone(t, alice, phoneA)
	mine := w.userChannel(t, domain.ChannelSMS, alice)
	other := w.userChannel(t, domain.ChannelVoice, w.admin)

	if err := w.st.DeleteUser(w.ctx, alice); err != nil {
		t.Fatal(err)
	}
	got := w.get(t, mine.ID)
	if got.Enabled || got.DisabledReason != domain.DeliverableRecipientDeleted || got.RecipientUserID != "" {
		t.Fatalf("channel of a deleted user = %+v; want disabled, recipient_deleted, no recipient", got)
	}
	wantDeliverable(t, "deleted recipient", got, domain.DeliverableRecipientDeleted)
	if o := w.get(t, other.ID); !o.Enabled || o.DisabledReason != "" {
		t.Fatalf("another user's channel changed: %+v", o)
	}
	if err := w.st.DeleteUser(w.ctx, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
	// An update must give the channel a recipient again; a pause and a delete need none.
	if _, err := w.patch(mine.ID, PhoneChannelPatch{Name: ptr("x")}); !errors.Is(err, domain.ErrPhoneRecipientNotEligible) {
		t.Fatalf("edit without a recipient: %v", err)
	}
	retargeted, err := w.patch(mine.ID, PhoneChannelPatch{RecipientUserID: ptr(w.admin)})
	if err != nil || retargeted.DisabledReason != "" || retargeted.Enabled {
		t.Fatalf("retarget a recipient_deleted channel: %+v, %v; want reason cleared, still disabled", retargeted, err)
	}
}

func TestPhoneChannelAuditRowsAndNoneForSlack(t *testing.T) {
	w := newPhoneChannelWorld(t)
	ch := w.numberChannel(t, domain.ChannelVoice, phoneA)
	if _, err := w.patch(ch.ID, PhoneChannelPatch{Name: ptr("renamed")}); err != nil {
		t.Fatal(err)
	}
	if err := w.st.DeletePhoneChannel(w.ctx, ch.ID, w.actor, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.GetNotificationChannel(w.ctx, ch.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted channel still readable: %v", err)
	}
	if err := w.st.DeletePhoneChannel(w.ctx, ch.ID, w.actor, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	target := "channel " + ch.ID + " · voice"
	for _, action := range []string{"notification_channel.create", "notification_channel.update", "notification_channel.delete"} {
		got := w.audits(t, action)
		if len(got) != 1 || got[0] != target {
			t.Fatalf("%s = %v, want [%s]", action, got, target)
		}
	}
	var actor, org string
	var viaToken bool
	if err := w.st.pool.QueryRow(w.ctx, `SELECT actor_user_id::text, org_id::text, via_token FROM audit_logs
		  WHERE action = 'notification_channel.delete'`).Scan(&actor, &org, &viaToken); err != nil {
		t.Fatal(err)
	}
	if actor != w.admin || org != w.org || viaToken {
		t.Fatalf("audit row: actor %s org %s via_token %v", actor, org, viaToken)
	}

	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSlack,
		Name: "slack", Config: map[string]string{"url": "https://hooks.example/x"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	slack.Name = "renamed"
	if _, err := w.st.UpdateNotificationChannel(w.ctx, slack); err != nil {
		t.Fatal(err)
	}
	if err := w.st.DeleteNotificationChannel(w.ctx, slack.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM audit_logs WHERE target LIKE '%' || $1 || '%'`, slack.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a slack channel's writes left %d audit rows, %v", n, err)
	}
}

// The store runs the caller's D28 decision against the row it LOCKED, and a refusal writes nothing.
func TestPhoneChannelAuthorizeSeesTheLockedRow(t *testing.T) {
	w := newPhoneChannelWorld(t)
	ch := w.userChannel(t, domain.ChannelSMS, w.admin)
	refused := errors.New("refused")
	var seen domain.NotificationChannel
	_, err := w.st.UpdatePhoneChannel(w.ctx, ch.ID, PhoneChannelPatch{Name: ptr("x")}, w.actor,
		func(stored domain.NotificationChannel) error { seen = stored; return refused })
	if !errors.Is(err, refused) || seen.RecipientUserID != w.admin || seen.Name != ch.Name {
		t.Fatalf("authorize: err %v, saw %+v", err, seen)
	}
	if err := w.st.DeletePhoneChannel(w.ctx, ch.ID, w.actor, func(domain.NotificationChannel) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("delete authorize: %v", err)
	}
	if got := w.get(t, ch.ID); got.Name != ch.Name {
		t.Fatal("a refused update wrote")
	}
	if n := len(w.audits(t, "notification_channel.update")) + len(w.audits(t, "notification_channel.delete")); n != 0 {
		t.Fatalf("refused writes left %d audit rows", n)
	}
}

func TestPhoneChannelVerificationCodes(t *testing.T) {
	w := newPhoneChannelWorld(t)
	scriptPhoneCodes(t, "111111", "222222")
	ch := w.numberChannel(t, domain.ChannelSMS, phoneA)
	user := w.userChannel(t, domain.ChannelSMS, w.admin)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, user.ID, w.admin); !errors.Is(err, domain.ErrPhoneChannelNotNumber) {
		t.Fatalf("send to a user channel: %v", err)
	}
	if _, err := w.st.VerifyChannelNumber(w.ctx, user.ID, "111111", w.actor); !errors.Is(err, domain.ErrPhoneChannelNotNumber) {
		t.Fatalf("verify a user channel: %v", err)
	}
	if _, err := w.st.VerifyChannelNumber(w.ctx, ch.ID, "111111", w.actor); !errors.Is(err, domain.ErrPhoneCodeExpired) {
		t.Fatalf("verify before any send: %v", err)
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, "00000000-0000-0000-0000-000000000001", w.admin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("send to no channel: %v", err)
	}
	w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin) //nolint:errcheck // 111111
	w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin) //nolint:errcheck // 222222 supersedes it
	// A superseded code is judged against the newer one: incorrect, and the attempt is counted.
	if _, err := w.st.VerifyChannelNumber(w.ctx, ch.ID, "111111", w.actor); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("superseded code: %v, want ErrPhoneCodeIncorrect", err)
	}
	var attempts int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT attempts FROM phone_verifications WHERE subject_id = $1
		 ORDER BY created_at DESC LIMIT 1`, ch.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts on the newer code = %d, %v", attempts, err)
	}
	// R6: three codes per requester (w.admin) per hour; the third send above was the third.
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin); err != nil {
		t.Fatalf("third send: %v", err)
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("fourth send in the hour: %v", err)
	}
	if n := len(w.audits(t, "notification_channel.verify")); n != 0 {
		t.Fatalf("a failed verification left %d audit rows", n)
	}
}

func TestListPhoneRecipients(t *testing.T) {
	w := newPhoneChannelWorld(t)
	alice := w.member(t, "alice@example.com", domain.RoleViewer)
	w.verifiedPhone(t, alice, phoneA)
	bob := w.member(t, "bob@example.com", domain.RoleEditor)
	mustSetPhone(t, w.st, w.ctx, bob, phoneB) // unverified
	phoneUser(t, w.st, w.ctx, "outsider@example.com")
	var machine string
	if err := w.st.pool.QueryRow(w.ctx, `INSERT INTO users (oidc_sub, email, kind) VALUES ('m1', 'm@example.com', 'machine')
		RETURNING id::text`).Scan(&machine); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.CreateProjectMembership(w.ctx, w.project, machine, domain.RoleEditor); err != nil {
		t.Fatal(err)
	}
	ga, err := w.st.CreateLocalUser(w.ctx, "ga@example.com", "GA", "x", true)
	if err != nil {
		t.Fatal(err)
	}

	got, err := w.st.ListPhoneRecipients(w.ctx, w.project)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.PhoneRecipient{}
	for _, r := range got {
		byID[r.UserID] = r
	}
	if len(got) != 4 || len(byID) != 4 {
		t.Fatalf("recipients = %+v; want alice, bob, the org admin and the global admin", got)
	}
	if r := byID[alice]; !r.PhoneVerified || r.Email != "alice@example.com" || r.DisplayName != "Person alice@example.com" {
		t.Fatalf("alice = %+v", r)
	}
	if byID[bob].PhoneVerified || byID[w.admin].PhoneVerified {
		t.Fatal("an unverified or absent phone reads as verified")
	}
	if _, ok := byID[ga.ID]; !ok {
		t.Fatal("a global admin is an eligible recipient")
	}
}

func TestReencryptCoversChannelNumbers(t *testing.T) {
	w := newPhoneChannelWorld(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	cB, _ := secret.New(keyB)
	w.st.WithCipher(cA)
	scriptPhoneCodes(t, "482193")
	ch := w.numberChannel(t, domain.ChannelSMS, phoneA)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin); err != nil {
		t.Fatal(err)
	}

	rotated, _ := secret.New(keyB, keyA)
	w.st.WithCipher(rotated)
	if _, err := w.st.ReencryptSecrets(w.ctx); err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	// The code in flight still verifies, its row now carrying the recomputed hash.
	if got, err := w.st.VerifyChannelNumber(w.ctx, ch.ID, "482193", w.actor); err != nil || !*got.NumberVerified {
		t.Fatalf("code across the rotation: %+v, %v", got, err)
	}
	w.st.WithCipher(cB)
	var enc, hash string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT number_enc, number_hash FROM notification_channels WHERE id = $1`, ch.ID).
		Scan(&enc, &hash); err != nil {
		t.Fatal(err)
	}
	if pt, err := cB.DecryptBytes(enc, secret.CanonicalAAD("channel_number", ch.ID)); err != nil || string(pt) != phoneA {
		t.Fatalf("under key B alone: %q, %v", pt, err)
	}
	if want, _ := cB.KeyedHash("phone-number", []byte(phoneA)); hash != want {
		t.Fatal("number_hash not recomputed under the new primary")
	}
	var stale int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_verifications WHERE subject_id = $1 AND number_hash <> $2`,
		ch.ID, hash).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("verification rows still under the old key: %d, %v", stale, err)
	}
	if _, err := cA.DecryptBytes(enc, secret.CanonicalAAD("channel_number", ch.ID)); err == nil {
		t.Fatal("the old key still opens the rewritten number")
	}
}

// The channel half of the lock order (Task 7 review, Minor 1; see TestUserPhoneSetAndVerifyDoNotDeadlock):
// a number change and a verification of one channel, concurrently, never deadlock.
func TestPhoneChannelWritesDoNotDeadlock(t *testing.T) {
	w := newPhoneChannelWorld(t)
	prev := newPhoneCode
	t.Cleanup(func() { newPhoneCode = prev })
	newPhoneCode = func() (string, error) { return "123456", nil }
	ch := w.numberChannel(t, domain.ChannelSMS, phoneA)
	numbers := []string{phoneA, "+12025550147"}
	for i := 0; i < 60; i++ {
		if _, err := w.st.pool.Exec(w.ctx, `UPDATE phone_verifications SET created_at = created_at - interval '2 days'`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin); err != nil {
			t.Fatalf("round %d: send: %v", i, err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := w.st.VerifyChannelNumber(w.ctx, ch.ID, "123456", w.actor)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := w.patch(ch.ID, PhoneChannelPatch{Number: ptr(numbers[(i+1)%2])})
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if isDeadlock(err) {
				t.Fatalf("round %d: deadlock: %v", i, err)
			}
			if err != nil && !isPhoneCodeVerdict(err) {
				t.Fatalf("round %d: %v", i, err)
			}
		}
	}
}

// Task 8 review, Minor 1: DeleteUser and a create, retarget or edit of a `user` channel naming the
// same person serialize on the user row. Before, a create's INSERT held only the foreign key's share
// lock: the deletion's UPDATE did not see the uncommitted channel, and ON DELETE SET NULL then left it
// enabled with no recipient and no reason — or, the other way round, the create met a raw 23503.
// Afterwards no channel is left recipient-less and still on (or without its reason), every refusal
// is recipient_not_eligible, and nothing deadlocks.
func TestDeleteUserSerializesWithPhoneChannelWrites(t *testing.T) {
	w := newPhoneChannelWorld(t)
	for i := 0; i < 40; i++ {
		u := w.member(t, fmt.Sprintf("racer%d@example.com", i), domain.RoleEditor)
		retarget := w.userChannel(t, domain.ChannelSMS, w.admin)
		edit := w.userChannel(t, domain.ChannelVoice, u)

		type outcome struct {
			label string
			err   error
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make(chan outcome, 4)
		run := func(label string, f func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- outcome{label, f()}
			}()
		}
		run("create", func() error {
			_, err := w.st.CreatePhoneChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSMS,
				Name: "new", Recipient: domain.PhoneRecipientUser, RecipientUserID: u, Enabled: true}, "", w.actor)
			return err
		})
		run("retarget", func() error {
			_, err := w.patch(retarget.ID, PhoneChannelPatch{RecipientUserID: ptr(u)})
			return err
		})
		run("edit", func() error {
			_, err := w.patch(edit.ID, PhoneChannelPatch{Name: ptr("renamed")})
			return err
		})
		run("delete user", func() error { return w.st.DeleteUser(w.ctx, u) })
		close(start)
		wg.Wait()
		close(results)
		for r := range results {
			var pgErr *pgconn.PgError
			switch {
			case r.err == nil:
			case isDeadlock(r.err):
				t.Fatalf("round %d: %s deadlocked: %v", i, r.label, r.err)
			case errors.As(r.err, &pgErr):
				t.Fatalf("round %d: %s answered a raw database error %s: %v", i, r.label, pgErr.Code, r.err)
			case r.label != "delete user" && errors.Is(r.err, domain.ErrPhoneRecipientNotEligible):
			default:
				t.Fatalf("round %d: %s: %v", i, r.label, r.err)
			}
		}
		var orphans int
		if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM notification_channels
			 WHERE recipient = 'user' AND recipient_user_id IS NULL AND (enabled OR disabled_reason IS NULL)`).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if orphans != 0 {
			t.Fatalf("round %d: %d channel(s) lost their recipient and stayed on, or lost it without a reason", i, orphans)
		}
	}
}

// Task 8 review, Minor 2: the generic pause and delete refuse sms/voice as the generic create and
// update do — a phone channel is enabled only through its gates and deleted only with its codes and
// its audit row. The four other types are written exactly as before.
func TestGenericChannelWritersRefusePhoneChannels(t *testing.T) {
	w := newPhoneChannelWorld(t)
	alice := w.member(t, "alice@example.com", domain.RoleEditor)
	user := w.userChannel(t, domain.ChannelVoice, alice)
	number := w.numberChannel(t, domain.ChannelSMS, phoneA)

	if err := w.st.SetNotificationChannelEnabled(w.ctx, number.ID, true); !errors.Is(err, errPhoneChannelWriter) {
		t.Fatalf("generic enable of an unverified number channel: %v, want errPhoneChannelWriter", err)
	}
	if got := w.get(t, number.ID); got.Enabled {
		t.Fatal("the generic writer enabled an unverified number channel")
	}
	if err := w.st.SetNotificationChannelEnabled(w.ctx, user.ID, false); !errors.Is(err, errPhoneChannelWriter) {
		t.Fatalf("generic pause of a phone channel: %v, want errPhoneChannelWriter", err)
	}
	if err := w.st.DeleteNotificationChannel(w.ctx, user.ID); !errors.Is(err, errPhoneChannelWriter) {
		t.Fatalf("generic delete of a phone channel: %v, want errPhoneChannelWriter", err)
	}
	if got := w.get(t, user.ID); !got.Enabled {
		t.Fatal("a refused generic write changed the phone channel")
	}

	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project, Type: domain.ChannelSlack,
		Name: "slack", Config: map[string]string{"url": "https://hooks.example/x"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.SetNotificationChannelEnabled(w.ctx, slack.ID, false); err != nil {
		t.Fatalf("pause slack: %v", err)
	}
	if got := w.get(t, slack.ID); got.Enabled {
		t.Fatal("slack not paused")
	}
	if err := w.st.DeleteNotificationChannel(w.ctx, slack.ID); err != nil {
		t.Fatalf("delete slack: %v", err)
	}
	const unknown = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	if err := w.st.DeleteNotificationChannel(w.ctx, slack.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
	if err := w.st.SetNotificationChannelEnabled(w.ctx, unknown, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause unknown: %v, want ErrNotFound", err)
	}
}

// waitForPhoneLockWait polls until a backend of this database other than the excluded ones is waiting on
// a lock that matches cond (an SQL condition over pg_locks aliased l), or fails after ten seconds.
func waitForPhoneLockWait(t *testing.T, w phoneChannelWorld, label, cond string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_locks l
			  JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE NOT l.granted AND a.datname = current_database() AND l.pid <> pg_backend_pid() AND `+cond,
			args...).Scan(&n); err != nil {
			t.Fatalf("%s: read pg_locks: %v", label, err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: nobody started waiting", label)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Task 9 review, Important 1: the three-party interleaving, ordered with explicit transactions rather
// than timing. W1 retargets channel C to U and holds C and KEY SHARE on U; W2, an edit of C, queues
// on C's row lock behind W1; DeleteUser(U) misses C in its first pass and waits for U behind W1. W1
// commits: W2 now holds C, DeleteUser holds U. Before the fix DeleteUser's second pass waited for C
// while W2's eligibility check waited for U — a deadlock (40P01, a 500). Now DeleteUser cannot get
// C without waiting, so it lets go of U and starts again, W2 finishes, and the deletion disables C.
func TestDeleteUserDoesNotDeadlockWithAQueuedEditOfARetargetedChannel(t *testing.T) {
	w := newPhoneChannelWorld(t)
	u := w.member(t, "target@example.com", domain.RoleEditor)
	c := w.userChannel(t, domain.ChannelSMS, w.admin)

	// W1: the retarget's locks and write, as UpdatePhoneChannel takes them (row, then KEY SHARE on
	// the user), without its advisory subject lock so W2 queues on the row itself.
	w1, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w1.Rollback(w.ctx) //nolint:errcheck // committed below
	if _, err := w1.Exec(w.ctx, `SELECT 1 FROM notification_channels WHERE id = $1 FOR NO KEY UPDATE`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := recipientEligibleTx(w.ctx, w1, w.project, u); err != nil {
		t.Fatal(err)
	}
	if _, err := w1.Exec(w.ctx, `UPDATE notification_channels SET recipient_user_id = $2 WHERE id = $1`, c.ID, u); err != nil {
		t.Fatal(err)
	}

	// W2: an edit of C, through the real lock helper (subject, then row): it queues on C's row.
	w2, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Rollback(w.ctx) //nolint:errcheck // committed below
	var w2pid int
	if err := w2.QueryRow(w.ctx, `SELECT pg_backend_pid()`).Scan(&w2pid); err != nil {
		t.Fatal(err)
	}
	w2Locked := make(chan error, 1)
	go func() {
		_, _, err := w.st.lockPhoneChannelTx(w.ctx, w2, c.ID)
		w2Locked <- err
	}()
	waitForPhoneLockWait(t, w, "W2 queued on C", `l.pid = $1`, w2pid)

	// DeleteUser(U): its first pass does not see C; it waits for U behind W1.
	deleted := make(chan error, 1)
	go func() { deleted <- w.st.DeleteUser(w.ctx, u) }()
	waitForPhoneLockWait(t, w, "DeleteUser waiting for U", `l.pid <> $1`, w2pid)

	if err := w1.Commit(w.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-w2Locked; err != nil {
		t.Fatalf("W2 lock: %v", err)
	}
	// W2 holds C. DeleteUser holds U and next needs C: wait until it is waiting on W2.
	var w2xid string
	if err := w2.QueryRow(w.ctx, `SELECT backend_xid::text FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&w2xid); err != nil {
		t.Fatal(err)
	}
	waitForPhoneLockWait(t, w, "DeleteUser waiting for C", `l.locktype = 'transactionid' AND l.transactionid::text = $1`, w2xid)

	// W2's eligibility check takes KEY SHARE on U, then the edit commits.
	if err := recipientEligibleTx(w.ctx, w2, w.project, u); err != nil {
		t.Fatalf("W2 eligibility: %v", err)
	}
	if _, err := w2.Exec(w.ctx, `UPDATE notification_channels SET name = 'renamed' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("W2 edit: %v", err)
	}
	if err := w2.Commit(w.ctx); err != nil {
		t.Fatalf("W2 commit: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	got := w.get(t, c.ID)
	if got.Name != "renamed" || got.Enabled || got.RecipientUserID != "" || got.DisabledReason != domain.DeliverableRecipientDeleted {
		t.Fatalf("channel after the deletion = %+v; want the edit kept, disabled, recipient_deleted", got)
	}
}
