package store

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-041a D31 rule 1 at send, P1, P4: PhoneTargets returns the handed phone channels with the verdict
// of the ONE rule-1 fragment (the DTO's `deliverable` reads the same text) and the verified number
// opened for this send. DB-gated.

func TestPhoneTargets(t *testing.T) {
	w := newPhoneChannelWorld(t)
	const (
		shared   = "+447700900123"
		alice    = "+905551110001"
		bobFirst = "+905551110002"
		bobNew   = "+905551110003"
	)
	duty := w.verifyNumberChannel(t, w.numberChannel(t, domain.ChannelSMS, shared).ID)
	if _, err := w.patch(duty.ID, PhoneChannelPatch{Enabled: ptr(true)}); err != nil { // created disabled (§4.2)
		t.Fatal(err)
	}
	unverified := w.numberChannel(t, domain.ChannelSMS, "+447700900124") // created disabled (§4.2)
	a := w.member(t, "alice@example.com", domain.RoleViewer)
	w.verifiedPhone(t, a, alice)
	lang := "tr-TR"
	if _, err := w.st.SetUserPhoneLanguage(w.ctx, a, &lang); err != nil {
		t.Fatal(err)
	}
	aliceVoice := w.userChannel(t, domain.ChannelVoice, a)
	b := w.member(t, "bob@example.com", domain.RoleViewer)
	w.verifiedPhone(t, b, bobFirst)
	bobSMS := w.userChannel(t, domain.ChannelSMS, b)
	c := w.member(t, "carol@example.com", domain.RoleViewer)
	mustSetPhone(t, w.st, w.ctx, c, "+905551110004") // never verified
	carolSMS := w.userChannel(t, domain.ChannelSMS, c)
	slack, err := w.st.CreateNotificationChannel(w.ctx, domain.NotificationChannel{ProjectID: w.project,
		Type: domain.ChannelSlack, Name: "slack", Config: map[string]string{"url": "https://hooks.example/x"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	off := w.userChannel(t, domain.ChannelSMS, a)
	if _, err := w.patch(off.ID, PhoneChannelPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}

	ids := []string{carolSMS.ID, slack.ID, duty.ID, unverified.ID, aliceVoice.ID, bobSMS.ID, off.ID, w.uuidFor(t)}
	targets := func() map[string]domain.PhoneTarget {
		t.Helper()
		got, err := w.st.PhoneTargets(w.ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]domain.PhoneTarget{}
		for _, tg := range got {
			out[tg.ChannelID] = tg
			// The verdict is the DTO's: one SQL text for both (D31 rule 1, Task 8/9).
			if dto := w.get(t, tg.ChannelID).Deliverable; dto == nil || dto.Reason != tg.NotRouteReason {
				t.Fatalf("channel %s: target verdict %q, DTO %+v", tg.ChannelID, tg.NotRouteReason, dto)
			}
		}
		return out
	}

	got := targets()
	// Only the enabled phone channels: not the Slack one, not a disabled phone, not an unknown id.
	if len(got) != 4 {
		t.Fatalf("targets = %v; want duty, alice, bob, carol", got)
	}
	want := map[string]domain.PhoneTarget{
		duty.ID: {ChannelID: duty.ID, ProjectID: w.project, Kind: domain.ChannelSMS,
			Recipient: domain.PhoneRecipientNumber, Number: shared},
		aliceVoice.ID: {ChannelID: aliceVoice.ID, ProjectID: w.project, Kind: domain.ChannelVoice,
			Recipient: domain.PhoneRecipientUser, UserID: a, Language: lang, Number: alice},
		bobSMS.ID: {ChannelID: bobSMS.ID, ProjectID: w.project, Kind: domain.ChannelSMS,
			Recipient: domain.PhoneRecipientUser, UserID: b, Number: bobFirst},
		// P1: an unverified number is never opened.
		carolSMS.ID: {ChannelID: carolSMS.ID, ProjectID: w.project, Kind: domain.ChannelSMS,
			Recipient: domain.PhoneRecipientUser, UserID: c, NotRouteReason: domain.DeliverableNoVerifiedNumber},
	}
	for id, tg := range want {
		if got[id] != tg {
			t.Fatalf("target %s = %+v (number %q); want %+v (number %q)", id, got[id], got[id].Number, tg, tg.Number)
		}
	}
	if !got[duty.ID].Route() || got[carolSMS.ID].Route() {
		t.Fatal("Route() disagrees with the verdict")
	}

	// P4: bob replaces his number. Unverified, the channel is no route and nothing is opened; once
	// verified, the next send goes to the new number — no channel edit in between.
	mustSetPhone(t, w.st, w.ctx, b, bobNew)
	if tg := targets()[bobSMS.ID]; tg.NotRouteReason != domain.DeliverableNoVerifiedNumber || tg.Number != "" {
		t.Fatalf("bob's unverified replacement: %+v number %q", tg, tg.Number)
	}
	w.verifiedPhone(t, b, bobNew)
	if tg := targets()[bobSMS.ID]; !tg.Route() || tg.Number != bobNew {
		t.Fatalf("bob's verified replacement: %+v number %q", tg, tg.Number)
	}

	// Alice leaves the project: not eligible. Her verified number still identifies the row (R9), but
	// the channel is no route.
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM memberships WHERE user_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if tg := targets()[aliceVoice.ID]; tg.NotRouteReason != domain.DeliverableRecipientNotEligible || tg.Route() {
		t.Fatalf("alice outside the project: %+v", tg)
	}

	// Telephony in effect: a voice channel under generic_http, then telephony off for everyone.
	w.telephony(t, true, domain.TelephonyProviderGenericHTTP)
	if _, err := w.st.CreateProjectMembership(w.ctx, w.project, a, domain.RoleViewer); err != nil {
		t.Fatal(err)
	}
	got = targets()
	if got[aliceVoice.ID].NotRouteReason != domain.DeliverableVoiceUnsupportedByProvider || !got[duty.ID].Route() {
		t.Fatalf("under generic_http: voice %+v, sms %+v", got[aliceVoice.ID], got[duty.ID])
	}
	w.telephony(t, false, domain.TelephonyProviderTwilio)
	for id, tg := range targets() {
		if tg.NotRouteReason != domain.DeliverableTelephonyDisabled {
			t.Fatalf("channel %s with telephony off: %+v", id, tg)
		}
	}
}

// uuidFor is a uuid no row holds.
func (w phoneChannelWorld) uuidFor(t *testing.T) string {
	t.Helper()
	var id string
	if err := w.st.pool.QueryRow(w.ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Task 11 review, finding 1: a number that exists but cannot be opened (a damaged ciphertext, a key
// dropped before `cerbix reencrypt`) fails THAT target only. The read succeeds; the other targets keep
// their numbers, so the event's other channels are phoned once (D19) while the broken one is retried.
func TestPhoneTargetsIsolatesAnUnreadableNumber(t *testing.T) {
	w := newPhoneChannelWorld(t)
	duty := w.verifyNumberChannel(t, w.numberChannel(t, domain.ChannelSMS, "+447700900123").ID)
	if _, err := w.patch(duty.ID, PhoneChannelPatch{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	a := w.member(t, "alice@example.com", domain.RoleViewer)
	w.verifiedPhone(t, a, "+905551110001")
	aliceVoice := w.userChannel(t, domain.ChannelVoice, a)
	b := w.member(t, "bob@example.com", domain.RoleViewer)
	w.verifiedPhone(t, b, "+905551110002")
	bobSMS := w.userChannel(t, domain.ChannelSMS, b)

	// Alice's number sealed for somebody else: it no longer opens under her AAD.
	sealed, err := w.c.EncryptBytes([]byte("+905551110001"), userPhoneAAD(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.pool.Exec(w.ctx, `UPDATE user_phones SET number_enc = $1 WHERE user_id = $2`, sealed, a); err != nil {
		t.Fatal(err)
	}

	got, err := w.st.PhoneTargets(w.ctx, []string{duty.ID, aliceVoice.ID, bobSMS.ID})
	if err != nil {
		t.Fatalf("PhoneTargets: %v; want the read to succeed and only alice's target to carry the failure", err)
	}
	if len(got) != 3 {
		t.Fatalf("targets = %v; want three", got)
	}
	byID := map[string]domain.PhoneTarget{}
	for _, tg := range got {
		byID[tg.ChannelID] = tg
	}
	if tg := byID[duty.ID]; !tg.Route() || tg.NumberErr != nil {
		t.Fatalf("duty: %+v err %v", tg, tg.NumberErr)
	}
	if tg := byID[bobSMS.ID]; !tg.Route() || tg.Number != "+905551110002" || tg.NumberErr != nil {
		t.Fatalf("bob: %+v err %v", tg, tg.NumberErr)
	}
	alice := byID[aliceVoice.ID]
	if alice.Route() || !alice.Unreadable() || alice.Number != "" {
		t.Fatalf("alice: %+v route %v unreadable %v; want no number and the failure", alice, alice.Route(), alice.Unreadable())
	}
	if msg := alice.NumberErr.Error(); !strings.Contains(msg, aliceVoice.ID) || strings.Contains(msg, "905551110001") {
		t.Fatalf("the failure %q must name the channel and never the number", msg)
	}

	// A target that is no route anyway carries the failure too, but is not Unreadable: its row is the
	// unsent one, without a mask.
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM memberships WHERE user_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	got, err = w.st.PhoneTargets(w.ctx, []string{aliceVoice.ID})
	if err != nil || len(got) != 1 {
		t.Fatalf("PhoneTargets: %v, %v", got, err)
	}
	if tg := got[0]; tg.NotRouteReason != domain.DeliverableRecipientNotEligible || tg.Unreadable() || tg.Number != "" {
		t.Fatalf("alice outside the project: %+v", tg)
	}
}
