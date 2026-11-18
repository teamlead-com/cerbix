package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a §4.2, §4.3, §9 — the sms and voice channels through the API: D28's matrix on create,
// update and delete, the gates (D27, D3, D29), a `number` channel's verification, the recipients
// endpoint, the audit rows, and that no response carries a number (P3). The fake store models the
// store's contract; the store's own tests prove it against PostgreSQL.

const (
	projectChannelsPath = "/api/v1/projects/p1/notification-channels"
	recipientsPath      = "/api/v1/projects/p1/phone-recipients"
)

var (
	pcEditor     = authz.Principal{UserID: "u-ed", AuditLabel: "ed@x", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}
	pcBob        = authz.Principal{UserID: "u-bob", AuditLabel: "bob@x", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}
	pcProjAdmin  = authz.Principal{UserID: "u-pa", AuditLabel: "pa@x", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
	pcViewer     = authz.Principal{UserID: "u-v", AuditLabel: "v@x", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleViewer}}}
	pcTokenAdmin = authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "t-adm", ViaToken: true, AuditLabel: "token:ops",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
)

// fakePhoneChannels is the phone-channel half of the fake store: the plaintext numbers (kept so a
// test can prove no response carries one), the active codes, the eligible recipients per project.
type fakePhoneChannels struct {
	numbers    map[string]string
	codes      map[string]string
	recipients map[string][]domain.PhoneRecipient
	provider   string
	seq        int
	// requesters are the requesters SendChannelVerificationCode was handed, in order (P2-3).
	requesters []string
}

func (f *fakeStore) phoneChans() *fakePhoneChannels {
	if f.pc == nil {
		f.pc = &fakePhoneChannels{numbers: map[string]string{}, codes: map[string]string{},
			recipients: map[string][]domain.PhoneRecipient{}, provider: domain.TelephonyProviderTwilio}
	}
	return f.pc
}

func (f *fakeStore) phoneEligible(projectID, userID string) bool {
	for _, r := range f.phoneChans().recipients[projectID] {
		if r.UserID == userID {
			return true
		}
	}
	return false
}

func (f *fakeStore) phoneAudit(projectID string, actor store.AuditActor, action, id string, typ domain.ChannelType) {
	f.audit = append([]domain.AuditEntry{{OrgID: f.projects[projectID].OrgID, ActorUserID: actor.ActorUserID,
		ViaToken: actor.ViaToken, Action: action, Target: "channel " + id + " · " + string(typ)}}, f.audit...)
}

// fakeDeliverable is the fake's D31 rule 1, enough for the DTO's shape.
func (f *fakeStore) fakeDeliverable(ch *domain.NotificationChannel) {
	reason := ""
	switch {
	case ch.Recipient == domain.PhoneRecipientUser && ch.RecipientUserID == "":
		reason = domain.DeliverableRecipientDeleted
	case ch.Recipient == domain.PhoneRecipientNumber && !*ch.NumberVerified:
		reason = domain.DeliverableNumberUnverified
	case !ch.Enabled:
		reason = domain.DeliverableDisabled
	case !f.telephonyOn:
		reason = domain.DeliverableTelephonyDisabled
	}
	ch.Deliverable = &domain.ChannelDeliverable{Route: reason == "", Reason: reason}
	// The recipient's display name, as the store's read resolves it: null once the user is gone.
	if ch.Recipient == domain.PhoneRecipientUser {
		ch.RecipientDisplayName = domain.RecipientName{Present: true}
		for _, r := range f.phoneChans().recipients[ch.ProjectID] {
			if r.UserID == ch.RecipientUserID && ch.RecipientUserID != "" {
				name := r.DisplayName
				ch.RecipientDisplayName.Name = &name
			}
		}
	}
}

func (f *fakeStore) phoneGate(typ domain.ChannelType) error {
	if !f.telephonyOn {
		return domain.ErrPhoneTelephonyDisabled
	}
	if typ == domain.ChannelVoice && f.phoneChans().provider == domain.TelephonyProviderGenericHTTP {
		return domain.ErrPhoneVoiceUnsupported
	}
	return nil
}

func (f *fakeStore) CreatePhoneChannel(_ context.Context, ch domain.NotificationChannel, number string, actor store.AuditActor) (domain.NotificationChannel, error) {
	pc := f.phoneChans()
	if err := ch.Validate(); err != nil {
		return domain.NotificationChannel{}, err
	}
	if f.telephonyNoKey {
		return domain.NotificationChannel{}, domain.ErrPhoneKeyMissing
	}
	if err := f.phoneGate(ch.Type); err != nil {
		return domain.NotificationChannel{}, err
	}
	pc.seq++
	ch.ID = fmt.Sprintf("pc-%d", pc.seq)
	ch.Config = map[string]string{}
	switch ch.Recipient {
	case domain.PhoneRecipientUser:
		if number != "" {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		if !f.phoneEligible(ch.ProjectID, ch.RecipientUserID) {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientNotEligible
		}
	case domain.PhoneRecipientNumber:
		if ch.RecipientUserID != "" {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		n, err := domain.NormalizeE164(number)
		if err != nil {
			return domain.NotificationChannel{}, err
		}
		pc.numbers[ch.ID] = n
		verified := false
		ch.NumberVerified, ch.NumberMasked = &verified, domain.MaskPhone(n)
		ch.Enabled, ch.DisabledReason = false, domain.DeliverableNumberUnverified
	}
	f.fakeDeliverable(&ch)
	f.channels[ch.ID] = ch
	f.phoneAudit(ch.ProjectID, actor, "notification_channel.create", ch.ID, ch.Type)
	return ch, nil
}

func (f *fakeStore) UpdatePhoneChannel(_ context.Context, id string, p store.PhoneChannelPatch, actor store.AuditActor,
	authorize func(domain.NotificationChannel) error) (domain.NotificationChannel, error) {
	pc := f.phoneChans()
	stored, ok := f.channels[id]
	if !ok {
		return domain.NotificationChannel{}, store.ErrNotFound
	}
	if authorize != nil {
		if err := authorize(stored); err != nil {
			return domain.NotificationChannel{}, err
		}
	}
	next := stored
	if p.Name != nil {
		next.Name = *p.Name
	}
	changed := false
	if stored.Recipient == domain.PhoneRecipientUser {
		if p.Number != nil {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		if p.RecipientUserID != nil {
			next.RecipientUserID = *p.RecipientUserID
		}
		pureDisable := p.Name == nil && p.RecipientUserID == nil && p.Enabled != nil && !*p.Enabled
		if !pureDisable && !f.phoneEligible(stored.ProjectID, next.RecipientUserID) {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientNotEligible
		}
	} else {
		if p.RecipientUserID != nil {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		if p.Number != nil {
			if f.telephonyNoKey {
				return domain.NotificationChannel{}, domain.ErrPhoneKeyMissing
			}
			n, err := domain.NormalizeE164(*p.Number)
			if err != nil {
				return domain.NotificationChannel{}, err
			}
			if n != pc.numbers[id] {
				changed = true
				pc.numbers[id] = n
				verified := false
				next.NumberVerified, next.NumberMasked = &verified, domain.MaskPhone(n)
				next.Enabled, next.DisabledReason = false, domain.DeliverableNumberUnverified
				delete(pc.codes, id)
			}
		}
	}
	if p.Enabled != nil {
		switch {
		case !*p.Enabled:
			next.Enabled = false
		case changed || next.Recipient == domain.PhoneRecipientNumber && !*next.NumberVerified:
			return domain.NotificationChannel{}, domain.ErrPhoneNumberUnverified
		case !stored.Enabled:
			if err := f.phoneGate(stored.Type); err != nil {
				return domain.NotificationChannel{}, err
			}
			next.Enabled, next.DisabledReason = true, ""
		}
	}
	f.fakeDeliverable(&next)
	f.channels[id] = next
	f.phoneAudit(stored.ProjectID, actor, "notification_channel.update", id, stored.Type)
	return next, nil
}

func (f *fakeStore) DeletePhoneChannel(_ context.Context, id string, actor store.AuditActor, authorize func(domain.NotificationChannel) error) error {
	stored, ok := f.channels[id]
	if !ok {
		return store.ErrNotFound
	}
	if authorize != nil {
		if err := authorize(stored); err != nil {
			return err
		}
	}
	delete(f.channels, id)
	f.phoneAudit(stored.ProjectID, actor, "notification_channel.delete", id, stored.Type)
	return nil
}

func (f *fakeStore) SendChannelVerificationCode(_ context.Context, id, requester string) (string, string, error) {
	pc := f.phoneChans()
	pc.requesters = append(pc.requesters, requester)
	ch, ok := f.channels[id]
	if !ok {
		return "", "", store.ErrNotFound
	}
	if f.telephonyNoKey {
		return "", "", domain.ErrPhoneKeyMissing
	}
	if ch.Recipient != domain.PhoneRecipientNumber {
		return "", "", domain.ErrPhoneChannelNotNumber
	}
	if ch.NumberVerified != nil && *ch.NumberVerified {
		return "", "", domain.ErrPhoneAlreadyVerified
	}
	code := f.phoneCode
	if code == "" {
		code = "482193"
	}
	pc.codes[id] = code
	return pc.numbers[id], code, nil
}

func (f *fakeStore) VerifyChannelNumber(_ context.Context, id, code string, actor store.AuditActor) (domain.NotificationChannel, error) {
	pc := f.phoneChans()
	ch, ok := f.channels[id]
	if !ok {
		return domain.NotificationChannel{}, store.ErrNotFound
	}
	if ch.Recipient != domain.PhoneRecipientNumber {
		return domain.NotificationChannel{}, domain.ErrPhoneChannelNotNumber
	}
	active, ok := pc.codes[id]
	if !ok {
		return domain.NotificationChannel{}, domain.ErrPhoneCodeExpired
	}
	if active != code {
		return domain.NotificationChannel{}, domain.ErrPhoneCodeIncorrect
	}
	delete(pc.codes, id)
	verified := true
	ch.NumberVerified = &verified
	if ch.DisabledReason == domain.DeliverableNumberUnverified {
		ch.DisabledReason = ""
	}
	f.fakeDeliverable(&ch)
	f.channels[id] = ch
	f.phoneAudit(ch.ProjectID, actor, "notification_channel.verify", id, ch.Type)
	return ch, nil
}

func (f *fakeStore) ListPhoneRecipients(_ context.Context, projectID string) ([]domain.PhoneRecipient, error) {
	return append([]domain.PhoneRecipient{}, f.phoneChans().recipients[projectID]...), nil
}

func phoneChannelFixture() (*fakeStore, *fakeProviderSource, http.Handler) {
	fs, src, _ := phoneFixture()
	pc := fs.phoneChans()
	pc.recipients["p1"] = []domain.PhoneRecipient{
		{UserID: "u-ed", DisplayName: "Ed", Email: "ed@x", PhoneVerified: true},
		{UserID: "u-bob", DisplayName: "Bob", Email: "bob@x"},
		{UserID: "u-pa", DisplayName: "Pat", Email: "pa@x", PhoneVerified: true},
		{UserID: "oa", DisplayName: "Org Admin", Email: "oa@x"},
		{UserID: "ga", DisplayName: "Global Admin", Email: "ga@x"},
	}
	h := api.New(fs, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), 8).WithTelephony(src).Router()
	return fs, src, h
}

func userChannelBody(typ, recipient string) string {
	return fmt.Sprintf(`{"type":%q,"name":"pager","recipient":"user","recipient_user_id":%q}`, typ, recipient)
}

const numberChannelBody = `{"type":"sms","name":"duty phone","recipient":"number","number":"+90 532 111 68 69","enabled":true}`

func decodeChannel(t *testing.T, body string) domain.NotificationChannel {
	t.Helper()
	var ch domain.NotificationChannel
	if err := json.Unmarshal([]byte(body), &ch); err != nil {
		t.Fatalf("decode channel %s: %v", body, err)
	}
	return ch
}

// seedPhoneChannel stores a phone channel directly, as the store would have left it.
func seedPhoneChannel(fs *fakeStore, id string, typ domain.ChannelType, recipient, userID, number string, verified bool) {
	ch := domain.NotificationChannel{ID: id, ProjectID: "p1", Type: typ, Name: id, Config: map[string]string{},
		Enabled: true, Recipient: recipient, RecipientUserID: userID}
	if recipient == domain.PhoneRecipientNumber {
		fs.phoneChans().numbers[id] = number
		ch.NumberMasked, ch.NumberVerified = domain.MaskPhone(number), &verified
		ch.Enabled = verified
	}
	fs.fakeDeliverable(&ch)
	fs.channels[id] = ch
}

func TestPhoneChannelCreateMatrix(t *testing.T) {
	cases := []struct {
		name string
		p    authz.Principal
		body string
		want int
	}{
		{"editor, user mode, themselves", pcEditor, userChannelBody("sms", "u-ed"), http.StatusCreated},
		{"editor, user mode, someone else", pcEditor, userChannelBody("sms", "u-bob"), http.StatusForbidden},
		{"editor, number mode", pcEditor, numberChannelBody, http.StatusForbidden},
		{"project admin, user mode, someone else", pcProjAdmin, userChannelBody("voice", "u-bob"), http.StatusCreated},
		{"project admin, number mode", pcProjAdmin, numberChannelBody, http.StatusCreated},
		{"org admin, user mode, someone else", o1Admin, userChannelBody("sms", "u-ed"), http.StatusCreated},
		{"org admin, number mode", o1Admin, numberChannelBody, http.StatusCreated},
		{"global admin, user mode", globalAdmin, userChannelBody("sms", "u-bob"), http.StatusCreated},
		{"global admin, number mode", globalAdmin, numberChannelBody, http.StatusCreated},
		{"token with project:manage, number mode", pcTokenAdmin, numberChannelBody, http.StatusCreated},
		{"token with project:manage, user mode", pcTokenAdmin, userChannelBody("sms", "u-ed"), http.StatusForbidden},
		{"editor token, number mode", tokenEditor, numberChannelBody, http.StatusForbidden},
		{"editor token, user mode", tokenEditor, userChannelBody("sms", "u-ed"), http.StatusForbidden},
		{"viewer, user mode, themselves", pcViewer, userChannelBody("sms", "u-v"), http.StatusForbidden},
		{"viewer, number mode", pcViewer, numberChannelBody, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, src, h := phoneChannelFixture()
			rec := do(h, tc.p, http.MethodPost, projectChannelsPath, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			assertNoNumber(t, rec.Body.String())
			if n := len(src.p.Calls()); n != 0 {
				t.Fatalf("creating a channel sent %d messages; nothing is sent before verify/send", n)
			}
			created := 0
			for _, ch := range fs.channels {
				if domain.IsPhoneChannelType(ch.Type) {
					created++
				}
			}
			if (tc.want == http.StatusCreated) != (created == 1) {
				t.Fatalf("phone channels stored = %d", created)
			}
		})
	}
}

func TestPhoneChannelUpdateAndDeleteMatrix(t *testing.T) {
	type step struct {
		name   string
		p      authz.Principal
		method string
		id     string
		body   string
		want   int
	}
	cases := []step{
		{"editor renames their own", pcEditor, http.MethodPatch, "ch-ed", `{"name":"mine"}`, http.StatusOK},
		{"editor pauses their own", pcEditor, http.MethodPatch, "ch-ed", `{"enabled":false}`, http.StatusOK},
		{"editor retargets their own to someone else", pcEditor, http.MethodPatch, "ch-ed", `{"recipient_user_id":"u-bob"}`, http.StatusForbidden},
		{"editor retargets someone else's to themselves", pcEditor, http.MethodPatch, "ch-bob", `{"recipient_user_id":"u-ed"}`, http.StatusForbidden},
		{"editor renames someone else's", pcEditor, http.MethodPatch, "ch-bob", `{"name":"x"}`, http.StatusForbidden},
		{"editor edits a number channel", pcEditor, http.MethodPatch, "ch-num", `{"name":"x"}`, http.StatusForbidden},
		{"editor deletes someone else's", pcEditor, http.MethodDelete, "ch-bob", "", http.StatusForbidden},
		{"editor deletes a number channel", pcEditor, http.MethodDelete, "ch-num", "", http.StatusForbidden},
		{"editor deletes their own", pcEditor, http.MethodDelete, "ch-ed", "", http.StatusNoContent},
		{"project admin retargets", pcProjAdmin, http.MethodPatch, "ch-ed", `{"recipient_user_id":"u-bob"}`, http.StatusOK},
		{"org admin edits a number channel", o1Admin, http.MethodPatch, "ch-num", `{"number":"+447700900199"}`, http.StatusOK},
		{"global admin deletes", globalAdmin, http.MethodDelete, "ch-bob", "", http.StatusNoContent},
		{"token edits a number channel", pcTokenAdmin, http.MethodPatch, "ch-num", `{"name":"noc"}`, http.StatusOK},
		{"token edits a user channel", pcTokenAdmin, http.MethodPatch, "ch-ed", `{"name":"x"}`, http.StatusForbidden},
		{"token deletes a user channel", pcTokenAdmin, http.MethodDelete, "ch-ed", "", http.StatusForbidden},
		{"token deletes a number channel", pcTokenAdmin, http.MethodDelete, "ch-num", "", http.StatusNoContent},
		{"editor token edits a number channel", tokenEditor, http.MethodPatch, "ch-num", `{"name":"x"}`, http.StatusForbidden},
		{"viewer edits", pcViewer, http.MethodPatch, "ch-ed", `{"name":"x"}`, http.StatusForbidden},
		{"viewer deletes", pcViewer, http.MethodDelete, "ch-ed", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, h := phoneChannelFixture()
			seedPhoneChannel(fs, "ch-ed", domain.ChannelSMS, domain.PhoneRecipientUser, "u-ed", "", false)
			seedPhoneChannel(fs, "ch-bob", domain.ChannelVoice, domain.PhoneRecipientUser, "u-bob", "", false)
			seedPhoneChannel(fs, "ch-num", domain.ChannelSMS, domain.PhoneRecipientNumber, "", myNumber, true)
			before := fs.channels[tc.id]
			rec := do(h, tc.p, tc.method, "/api/v1/notification-channels/"+tc.id, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			assertNoNumber(t, rec.Body.String())
			if tc.want == http.StatusForbidden {
				if after, ok := fs.channels[tc.id]; !ok || after.Name != before.Name || after.RecipientUserID != before.RecipientUserID {
					t.Fatalf("a refused write changed the channel: %+v", after)
				}
				if len(phoneAudits(fs, "notification_channel.update"))+len(phoneAudits(fs, "notification_channel.delete")) != 0 {
					t.Fatal("a refused write was audited")
				}
			}
		})
	}
}

func TestNumberChannelVerifyFlow(t *testing.T) {
	fs, src, h := phoneChannelFixture()
	rec := do(h, pcProjAdmin, http.MethodPost, projectChannelsPath, numberChannelBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	assertNoNumber(t, rec.Body.String())
	ch := decodeChannel(t, rec.Body.String())
	if ch.Enabled || ch.DisabledReason != "number_unverified" || ch.NumberMasked != "+90•••••6869" ||
		ch.NumberVerified == nil || *ch.NumberVerified || ch.Deliverable == nil || ch.Deliverable.Reason != "number_unverified" {
		t.Fatalf("created = %s; want disabled, number_unverified, masked, not a route", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"number_verified":false`) || strings.Contains(rec.Body.String(), `"number":`) {
		t.Fatalf("created body shape: %s", rec.Body.String())
	}
	chPath := "/api/v1/notification-channels/" + ch.ID

	assertReason(t, "enable before verification", do(h, pcProjAdmin, http.MethodPatch, chPath, `{"enabled":true}`),
		http.StatusConflict, "number_unverified")

	// Only the editor-or-better with project:manage verifies; an editor cannot.
	if rec := do(h, pcEditor, http.MethodPost, chPath+"/verify/send", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("editor verify/send = %d", rec.Code)
	}
	rec = do(h, pcProjAdmin, http.MethodPost, chPath+"/verify/send", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("verify/send = %d %s", rec.Code, rec.Body.String())
	}
	calls := src.p.Calls()
	if len(calls) != 1 || calls[0].Kind != "sms" || calls[0].To != myNumber || calls[0].Body != telephony.VerificationCodeText("482193") {
		t.Fatalf("provider calls = %+v; want exactly the code SMS to the channel's number", calls)
	}
	assertReason(t, "malformed code", do(h, pcProjAdmin, http.MethodPost, chPath+"/verify", `{"code":"12"}`),
		http.StatusBadRequest, "code_incorrect")
	assertReason(t, "wrong code", do(h, pcProjAdmin, http.MethodPost, chPath+"/verify", `{"code":"000000"}`),
		http.StatusBadRequest, "code_incorrect")
	rec = do(h, pcProjAdmin, http.MethodPost, chPath+"/verify", `{"code":"482193"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body.String())
	}
	assertNoNumber(t, rec.Body.String())
	if v := decodeChannel(t, rec.Body.String()); !*v.NumberVerified || v.Enabled || v.DisabledReason != "" {
		t.Fatalf("verified = %s", rec.Body.String())
	}
	rec = do(h, pcProjAdmin, http.MethodPatch, chPath, `{"enabled":true}`)
	if rec.Code != http.StatusOK || !decodeChannel(t, rec.Body.String()).Enabled {
		t.Fatalf("enable after verification = %d %s", rec.Code, rec.Body.String())
	}

	// A number change clears the verification and disables, in the answer as in the store.
	rec = do(h, pcProjAdmin, http.MethodPatch, chPath, `{"number":"+447700900199"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("number change = %d %s", rec.Code, rec.Body.String())
	}
	if c := decodeChannel(t, rec.Body.String()); c.Enabled || *c.NumberVerified || c.DisabledReason != "number_unverified" {
		t.Fatalf("after a number change = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "7700900199") {
		t.Fatalf("the new number leaked: %s", rec.Body.String())
	}

	// Nothing but the code ever reached the provider (P1).
	for _, c := range src.p.Calls() {
		if !strings.HasPrefix(c.Body, "[cerbix] Your verification code is ") {
			t.Fatalf("a non-code text was sent: %+v", c)
		}
	}
	for _, action := range []string{"notification_channel.create", "notification_channel.verify", "notification_channel.update"} {
		for _, e := range phoneAudits(fs, action) {
			if e.Target != "channel "+ch.ID+" · sms" {
				t.Fatalf("%s target = %q", action, e.Target)
			}
		}
	}
}

func TestNumberChannelVerifyRefusals(t *testing.T) {
	fs, src, h := phoneChannelFixture()
	seedPhoneChannel(fs, "ch-num", domain.ChannelSMS, domain.PhoneRecipientNumber, "", myNumber, false)
	seedPhoneChannel(fs, "ch-ed", domain.ChannelSMS, domain.PhoneRecipientUser, "u-ed", "", false)
	send := "/api/v1/notification-channels/ch-num/verify/send"

	if rec := do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/ch-ed/verify/send", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("send to a user channel = %d", rec.Code)
	}
	if rec := do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/nc1/verify/send", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("send to a webhook channel = %d", rec.Code)
	}
	if rec := do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/nope/verify/send", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("send to no channel = %d", rec.Code)
	}
	fs.telephonyOn = false
	assertReason(t, "telephony off", do(h, pcProjAdmin, http.MethodPost, send, ""), http.StatusConflict, "telephony_disabled")
	fs.telephonyOn = true
	fs.telephonyNoKey = true
	assertReason(t, "no key", do(h, pcProjAdmin, http.MethodPost, send, ""), http.StatusConflict, "encryption_key_missing")
	fs.telephonyNoKey = false
	src.p.ScriptSMS(telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"})
	assertReason(t, "provider refusal", do(h, pcProjAdmin, http.MethodPost, send, ""), http.StatusBadGateway, "provider_rejected")
	if n := len(src.p.Calls()); n != 1 {
		t.Fatalf("calls = %d, want only the refused one", n)
	}
}

// FR-041a review P2-3: the channel's code is asked for by the principal of the request — a person by
// their user id, an API token as itself — which is whose hourly limit it spends; a verified channel
// is sent no code: 409 number_already_verified, and the provider is not called.
func TestNumberChannelVerifySendRequesterAndVerifiedRefusal(t *testing.T) {
	fs, src, h := phoneChannelFixture()
	seedPhoneChannel(fs, "ch-num", domain.ChannelSMS, domain.PhoneRecipientNumber, "", myNumber, false)
	seedPhoneChannel(fs, "ch-ok", domain.ChannelSMS, domain.PhoneRecipientNumber, "", myNumber, true)
	send := "/api/v1/notification-channels/ch-num/verify/send"
	for _, p := range []authz.Principal{pcProjAdmin, pcTokenAdmin} {
		if rec := do(h, p, http.MethodPost, send, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("%s verify/send = %d %s", p.UserID, rec.Code, rec.Body.String())
		}
	}
	if got := fs.phoneChans().requesters; len(got) != 2 || got[0] != "u-pa" || got[1] != authz.SyntheticTokenActorPrefix+"t-adm" {
		t.Fatalf("requesters = %v; want the person's user id, then the token as itself", got)
	}
	calls := len(src.p.Calls())
	assertReason(t, "verified channel", do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/ch-ok/verify/send", ""),
		http.StatusConflict, "number_already_verified")
	if n := len(src.p.Calls()); n != calls {
		t.Fatalf("a verified channel's refusal called the provider (%d calls, had %d)", n, calls)
	}
}

func TestPhoneChannelWriteRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(fs *fakeStore)
		method string
		path   string
		body   string
		status int
		reason string
	}{
		{"telephony off", func(fs *fakeStore) { fs.telephonyOn = false }, http.MethodPost, projectChannelsPath,
			userChannelBody("sms", "u-ed"), http.StatusConflict, "telephony_disabled"},
		{"voice under generic_http", func(fs *fakeStore) { fs.phoneChans().provider = domain.TelephonyProviderGenericHTTP },
			http.MethodPost, projectChannelsPath, userChannelBody("voice", "u-ed"), http.StatusBadRequest, "voice_unsupported_by_provider"},
		{"no at-rest key", func(fs *fakeStore) { fs.telephonyNoKey = true }, http.MethodPost, projectChannelsPath,
			numberChannelBody, http.StatusConflict, "encryption_key_missing"},
		{"invalid number", nil, http.MethodPost, projectChannelsPath,
			`{"type":"sms","name":"x","recipient":"number","number":"0532 111"}`, http.StatusBadRequest, "number_invalid"},
		{"no recipient", nil, http.MethodPost, projectChannelsPath,
			`{"type":"sms","name":"x","recipient":"user"}`, http.StatusBadRequest, "recipient_not_eligible"},
		{"ineligible recipient", nil, http.MethodPost, projectChannelsPath,
			userChannelBody("sms", "u-stranger"), http.StatusBadRequest, "recipient_not_eligible"},
		{"a user channel with a number", nil, http.MethodPost, projectChannelsPath,
			`{"type":"sms","name":"x","recipient":"user","recipient_user_id":"u-ed","number":"+905321116869"}`,
			http.StatusBadRequest, "recipient_fixed"},
		{"mode change", nil, http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"recipient":"number"}`,
			http.StatusBadRequest, "recipient_fixed"},
		{"number on a user channel", nil, http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"number":"+905321116869"}`,
			http.StatusBadRequest, "recipient_fixed"},
		{"retarget to nobody", nil, http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"recipient_user_id":""}`,
			http.StatusBadRequest, "recipient_not_eligible"},
		{"enable with telephony off", func(fs *fakeStore) {
			fs.telephonyOn = false
			c := fs.channels["ch-ed"]
			c.Enabled = false
			fs.channels["ch-ed"] = c
		}, http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"enabled":true}`, http.StatusConflict, "telephony_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, h := phoneChannelFixture()
			seedPhoneChannel(fs, "ch-ed", domain.ChannelSMS, domain.PhoneRecipientUser, "u-ed", "", false)
			if tc.setup != nil {
				tc.setup(fs)
			}
			assertReason(t, tc.name, do(h, pcProjAdmin, tc.method, tc.path, tc.body), tc.status, tc.reason)
		})
	}
	plain := []struct{ name, method, path, body string }{
		{"phone config", http.MethodPost, projectChannelsPath, `{"type":"sms","name":"x","recipient":"user","recipient_user_id":"u-ed","config":{"to":"x"}}`},
		{"no mode", http.MethodPost, projectChannelsPath, `{"type":"sms","name":"x"}`},
		{"recipient on slack", http.MethodPost, projectChannelsPath, `{"type":"slack","name":"x","config":{"url":"https://h.example/x"},"recipient":"user"}`},
		{"number on a webhook edit", http.MethodPatch, "/api/v1/notification-channels/nc1", `{"number":"+905321116869"}`},
		{"phone config edit", http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"config":{"url":"https://x"}}`},
		{"empty phone edit", http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{}`},
	}
	for _, tc := range plain {
		fs, _, h := phoneChannelFixture()
		seedPhoneChannel(fs, "ch-ed", domain.ChannelSMS, domain.PhoneRecipientUser, "u-ed", "", false)
		rec := do(h, pcProjAdmin, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
		assertNoNumber(t, rec.Body.String())
	}
}

func TestPhoneRecipientsEndpoint(t *testing.T) {
	_, _, h := phoneChannelFixture()
	ids := func(p authz.Principal) []string {
		t.Helper()
		rec := do(h, p, http.MethodGet, recipientsPath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", p.UserID, rec.Code, rec.Body.String())
		}
		var list []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range list {
			for k := range r {
				switch k {
				case "user_id", "display_name", "email", "phone_verified":
				default:
					t.Fatalf("recipient field %q: %s", k, rec.Body.String())
				}
			}
			out = append(out, r["user_id"].(string))
		}
		sort.Strings(out)
		return out
	}
	if got := ids(pcEditor); len(got) != 1 || got[0] != "u-ed" {
		t.Fatalf("editor sees %v; want only themselves", got)
	}
	if got := ids(pcProjAdmin); len(got) != 5 {
		t.Fatalf("project admin sees %v; want every eligible recipient", got)
	}
	if got := ids(o1Admin); len(got) != 5 {
		t.Fatalf("org admin sees %v", got)
	}
	if got := ids(pcTokenAdmin); len(got) != 0 {
		t.Fatalf("a token sees %v; a token names no recipient (D28)", got)
	}
	if rec := do(h, pcViewer, http.MethodGet, recipientsPath, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer = %d", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, recipientsPath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider = %d", rec.Code)
	}
}

func TestPhoneChannelAuditedAndSlackNot(t *testing.T) {
	fs, _, h := phoneChannelFixture()
	rec := do(h, pcEditor, http.MethodPost, projectChannelsPath, userChannelBody("sms", "u-ed"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	id := decodeChannel(t, rec.Body.String()).ID
	if rec := do(h, pcEditor, http.MethodPatch, "/api/v1/notification-channels/"+id, `{"name":"pager 2"}`); rec.Code != http.StatusOK {
		t.Fatalf("update = %d", rec.Code)
	}
	if rec := do(h, pcEditor, http.MethodDelete, "/api/v1/notification-channels/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	for _, action := range []string{"notification_channel.create", "notification_channel.update", "notification_channel.delete"} {
		got := phoneAudits(fs, action)
		if len(got) != 1 || got[0].Target != "channel "+id+" · sms" || got[0].ActorUserID != "u-ed" || got[0].OrgID != "o1" {
			t.Fatalf("%s = %+v", action, got)
		}
	}

	before := len(fs.audit)
	rec = do(h, pcEditor, http.MethodPost, projectChannelsPath, `{"type":"slack","name":"ops","config":{"url":"https://hooks.example/x"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("slack create = %d %s", rec.Code, rec.Body.String())
	}
	sid := decodeChannel(t, rec.Body.String()).ID
	do(h, pcEditor, http.MethodPatch, "/api/v1/notification-channels/"+sid, `{"name":"ops2","enabled":false}`)
	do(h, pcEditor, http.MethodDelete, "/api/v1/notification-channels/"+sid, "")
	if len(fs.audit) != before {
		t.Fatalf("a slack channel's writes were audited: %+v", fs.audit[:len(fs.audit)-before])
	}
}

// The four existing types answer with exactly the seven fields they always had (P14).
func TestSlackChannelResponseUnchanged(t *testing.T) {
	_, _, h := phoneChannelFixture()
	for _, rec := range []*httptest.ResponseRecorder{
		do(h, pcEditor, http.MethodGet, projectChannelsPath, ""),
		do(h, pcEditor, http.MethodPost, projectChannelsPath, `{"type":"slack","name":"ops","config":{"url":"https://hooks.example/x"}}`),
		do(h, pcEditor, http.MethodPatch, "/api/v1/notification-channels/nc1", `{"name":"ops"}`),
	} {
		if rec.Code >= 300 {
			t.Fatalf("status %d %s", rec.Code, rec.Body.String())
		}
		var raw any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		objs, ok := raw.([]any)
		if !ok {
			objs = []any{raw}
		}
		for _, o := range objs {
			var keys []string
			for k := range o.(map[string]any) {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "config,created_at,enabled,id,name,project_id,type" {
				t.Fatalf("channel keys = %v", keys)
			}
		}
	}
}

// Task 7 review, Minor 3: the test SMS without the at-rest key is the 409 every phone route answers,
// not a 500.
func TestTelephonyTestSMSWithoutKey(t *testing.T) {
	fs, src, h := phoneFixture()
	fs.phones = map[string]*fakePhone{"ga": {number: myNumber, verified: true}}
	fs.telephonyNoKey = true
	assertReason(t, "test-sms without the key", do(h, globalAdmin, http.MethodPost, testSMSPath, ""),
		http.StatusConflict, "encryption_key_missing")
	if n := len(src.p.Calls()); n != 0 {
		t.Fatalf("calls = %d", n)
	}
}

// Task 7 review, Minor 5: a refused code send tells the operator how the provider answered — the
// outcome, the HTTP status and the provider's code (D34-safe) — and never the number.
func TestCodeSendFailureLogsTheProviderAnswer(t *testing.T) {
	fs, src, _ := phoneChannelFixture()
	seedPhoneChannel(fs, "ch-num", domain.ChannelSMS, domain.PhoneRecipientNumber, "", myNumber, false)
	var logs bytes.Buffer
	h := api.New(fs, slog.New(slog.NewTextHandler(&logs, nil)), 8).WithTelephony(src).Router()

	src.p.ScriptSMS(telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"},
		telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503})
	assertReason(t, "own number", do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody), http.StatusBadGateway, "provider_rejected")
	assertReason(t, "channel", do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/ch-num/verify/send", ""),
		http.StatusBadGateway, "provider_unreachable")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var failed []string
	for _, l := range lines {
		if strings.Contains(l, "phone_code_send_failed") {
			failed = append(failed, l)
		}
	}
	if len(failed) != 2 {
		t.Fatalf("send-failure lines = %d:\n%s", len(failed), logs.String())
	}
	for i, frag := range [][]string{
		{"user_id=u-phone", "outcome=provider_rejected", "http_status=400", "provider_code=21211"},
		{"channel_id=ch-num", "outcome=provider_unreachable", "http_status=503", "provider_code="},
	} {
		for _, f := range frag {
			if !strings.Contains(failed[i], f) {
				t.Fatalf("line %q lacks %q", failed[i], f)
			}
		}
	}
	assertNoNumber(t, logs.String())
}

// FR-041a review P1-2 (D28 as written): a token whose permission list is ONLY project:manage creates,
// updates and deletes a `number` channel. For sms and voice the project's visibility is judged first
// (404), then D28; the four text types keep project:write, so the same token is still refused a Slack
// channel.
func TestManageOnlyTokenManagesANumberChannel(t *testing.T) {
	manageOnly := authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "t-manage", ViaToken: true, AuditLabel: "token:noc",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}},
		Actions:     []authz.Action{authz.ActionProjectManage}}
	fs, _, h := phoneChannelFixture()
	rec := do(h, manageOnly, http.MethodPost, projectChannelsPath, numberChannelBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	id := decodeChannel(t, rec.Body.String()).ID
	if rec := do(h, manageOnly, http.MethodPatch, "/api/v1/notification-channels/"+id, `{"name":"noc"}`); rec.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(h, manageOnly, http.MethodDelete, "/api/v1/notification-channels/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := fs.channels[id]; ok {
		t.Fatal("the number channel survived its delete")
	}
	// D28 still holds for the token: never a `user` channel.
	seedPhoneChannel(fs, "ch-ed", domain.ChannelSMS, domain.PhoneRecipientUser, "u-ed", "", false)
	for _, c := range []struct{ name, method, path, body string }{
		{"create a user channel", http.MethodPost, projectChannelsPath, userChannelBody("sms", "u-ed")},
		{"rename a user channel", http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"name":"x"}`},
		{"delete a user channel", http.MethodDelete, "/api/v1/notification-channels/ch-ed", ""},
		// The four text types keep their own check exactly: project:write, which this token lacks.
		{"create a slack channel", http.MethodPost, projectChannelsPath, `{"type":"slack","name":"ops","config":{"url":"https://hooks.example/x"}}`},
		{"rename a webhook channel", http.MethodPatch, "/api/v1/notification-channels/nc1", `{"name":"x"}`},
		{"delete a webhook channel", http.MethodDelete, "/api/v1/notification-channels/nc1", ""},
	} {
		if rec := do(h, manageOnly, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
	// Visibility first: a principal that cannot see the project is answered 404 for a phone channel,
	// whatever its body, as for every other channel.
	stranger := authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "t-other", ViaToken: true,
		Memberships: []domain.Membership{{OrgID: "o2", Role: domain.RoleOrgAdmin}}, Actions: []authz.Action{authz.ActionProjectManage}}
	for _, c := range []struct{ name, method, path, body string }{
		{"create", http.MethodPost, projectChannelsPath, numberChannelBody},
		{"update", http.MethodPatch, "/api/v1/notification-channels/ch-ed", `{"name":"x"}`},
		{"delete", http.MethodDelete, "/api/v1/notification-channels/ch-ed", ""},
		{"create a slack channel", http.MethodPost, projectChannelsPath, `{"type":"slack","name":"ops","config":{"url":"https://hooks.example/x"}}`},
	} {
		if rec := do(h, stranger, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("stranger %s = %d, want 404 (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
}

// FR-041a review P2-9, the reviewer's three edge cases of the channel writes: a field of the other
// recipient mode is refused as recipient_fixed before its value is looked at, even empty; and a PATCH
// that names nothing to change — `recipient` repeating the mode is no change — is refused, with no
// write and no audit row.
func TestPhoneChannelWriteEdgeCases(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		status                   int
		reason                   string
	}{
		{"PATCH recipient=user on a user channel is no change", http.MethodPatch, "/api/v1/notification-channels/ch-ed",
			`{"recipient":"user"}`, http.StatusBadRequest, ""},
		{"PATCH an empty recipient_user_id on a number channel", http.MethodPatch, "/api/v1/notification-channels/ch-num",
			`{"recipient_user_id":""}`, http.StatusBadRequest, "recipient_fixed"},
		{"PATCH a recipient_user_id on a number channel", http.MethodPatch, "/api/v1/notification-channels/ch-num",
			`{"recipient_user_id":"u-ed"}`, http.StatusBadRequest, "recipient_fixed"},
		{"PATCH an empty number on a user channel", http.MethodPatch, "/api/v1/notification-channels/ch-ed",
			`{"number":""}`, http.StatusBadRequest, "recipient_fixed"},
		{"POST a user channel with an empty number", http.MethodPost, projectChannelsPath,
			`{"type":"sms","name":"x","recipient":"user","recipient_user_id":"u-ed","number":""}`, http.StatusBadRequest, "recipient_fixed"},
		{"POST a number channel with a recipient_user_id", http.MethodPost, projectChannelsPath,
			`{"type":"sms","name":"x","recipient":"number","number":"+905321116869","recipient_user_id":""}`, http.StatusBadRequest, "recipient_fixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, h := phoneChannelFixture()
			seedPhoneChannel(fs, "ch-ed", domain.ChannelSMS, domain.PhoneRecipientUser, "u-ed", "", false)
			seedPhoneChannel(fs, "ch-num", domain.ChannelSMS, domain.PhoneRecipientNumber, "", myNumber, true)
			before := len(fs.channels)
			rec := do(h, pcProjAdmin, tc.method, tc.path, tc.body)
			if tc.reason != "" {
				assertReason(t, tc.name, rec, tc.status, tc.reason)
			} else if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if n := len(phoneAudits(fs, "notification_channel.update")) + len(phoneAudits(fs, "notification_channel.create")); n != 0 {
				t.Fatalf("a refused write wrote %d audit rows", n)
			}
			if len(fs.channels) != before {
				t.Fatal("a refused create stored a channel")
			}
		})
	}
}
