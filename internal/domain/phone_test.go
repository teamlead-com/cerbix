package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeE164(t *testing.T) {
	ok := map[string]string{
		"+905321116869":     "+905321116869",
		"+90 532 111 68 69": "+905321116869",
		"+1 (202) 555-0147": "+12025550147",
		"+44.7700.900.199":  "+447700900199",
		"+380441234567":     "+380441234567",
		"+4930123302":       "+4930123302",
		"+123456789012345":  "+123456789012345",
		"+12345678":         "+12345678",
	}
	for in, want := range ok {
		got, err := NormalizeE164(in)
		if err != nil || got != want {
			t.Errorf("NormalizeE164(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "905321116869", "+0905321116869", "+1234567", "+1234567890123456", "+90abc1116869", "+90 532 x", "++905321116869", "+90-532+111"} {
		if got, err := NormalizeE164(in); err == nil {
			t.Errorf("NormalizeE164(%q) = %q; want error", in, got)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+905321116869":  "+90•••••6869",
		"+12025550147":   "+1•••••0147",
		"+447700900199":  "+44•••••0199",
		"+4930123302":    "+49•••••3302",
		"+380441234567":  "+380•••••4567",
		"+79161234567":   "+7•••••4567",
		"+358401234567":  "+358•••••4567",
		"+9715012345678": "+971•••••5678",
	}
	for in, want := range cases {
		if got := MaskPhone(in); got != want {
			t.Errorf("MaskPhone(%q) = %q; want %q", in, got, want)
		}
	}
	if got := MaskPhone("garbage"); got != "" {
		t.Errorf("MaskPhone(garbage) = %q; want empty", got)
	}
}

func TestPhoneChannelTypes(t *testing.T) {
	if ChannelSMS != "sms" || ChannelVoice != "voice" {
		t.Fatal("wrong constants")
	}
	if !IsPhoneChannelType(ChannelSMS) || !IsPhoneChannelType(ChannelVoice) {
		t.Fatal("sms/voice must be phone types")
	}
	for _, c := range []ChannelType{ChannelWebhook, ChannelSlack, ChannelTelegram, ChannelEmail, "x"} {
		if IsPhoneChannelType(c) {
			t.Errorf("%q must not be a phone type", c)
		}
	}
	if !ChannelSMS.Valid() || !ChannelVoice.Valid() {
		t.Error("sms/voice are channel types the API accepts (Task 8)")
	}
	if PhoneRecipientUser != "user" || PhoneRecipientNumber != "number" {
		t.Fatal("recipient modes")
	}
}

func TestValidPhoneReason(t *testing.T) {
	for _, r := range []string{"sending", "sent", "provider_unreachable", "provider_rejected", "outcome_unknown", "not_deliverable", "voice_unsupported_by_provider", "telephony_disabled", "storm_limited"} {
		if !ValidPhoneReason(r) {
			t.Errorf("%q must be valid", r)
		}
	}
	for _, r := range []string{"", "no_incident_ref", "link_omitted", "SENT", "queued"} {
		if ValidPhoneReason(r) {
			t.Errorf("%q must be invalid", r)
		}
	}
	notes := []string{PhoneNoteNoIncidentRef, PhoneNoteNoPublicURL, PhoneNoteLinkOmitted, PhoneNoteNoVerifiedNumber, PhoneNoteRecipientNotEligible}
	want := []string{"no_incident_ref", "no_public_url", "link_omitted", "no_verified_number", "recipient_not_eligible"}
	for i := range notes {
		if notes[i] != want[i] {
			t.Errorf("note %d = %q; want %q", i, notes[i], want[i])
		}
	}
	d := []string{DeliverableDisabled, DeliverableTelephonyDisabled, DeliverableVoiceUnsupportedByProvider, DeliverableNumberUnverified, DeliverableNoVerifiedNumber, DeliverableRecipientNotEligible, DeliverableRecipientDeleted}
	dw := []string{"disabled", "telephony_disabled", "voice_unsupported_by_provider", "number_unverified", "no_verified_number", "recipient_not_eligible", "recipient_deleted"}
	for i := range d {
		if d[i] != dw[i] {
			t.Errorf("deliverable %d = %q; want %q", i, d[i], dw[i])
		}
	}
}

func TestNormalizeIncidentRef(t *testing.T) {
	ok := map[string]string{
		"7K3M9QXZ": "7K3M9QXZ",
		"7k3m9qxz": "7K3M9QXZ",
		"IL0O1234": "11001234",
		"abcdefgh": "ABCDEFGH",
	}
	for in, want := range ok {
		got, valid := NormalizeIncidentRef(in)
		if !valid || got != want {
			t.Errorf("NormalizeIncidentRef(%q) = %q, %v; want %q", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "7K3M9QX", "7K3M9QXZ1", "7K3M9QXU", "7K3M9Q-Z", "7K3M 9QX", "7K3M9QX€", "ABCDEFı", "ABCDEFſ"} {
		if got, valid := NormalizeIncidentRef(in); valid {
			t.Errorf("NormalizeIncidentRef(%q) = %q; want invalid", in, got)
		}
	}
}

func TestValidPhoneLanguage(t *testing.T) {
	for _, s := range []string{"en", "tr-TR", "en-US", "zh-Hant-TW", "fil"} {
		if !ValidPhoneLanguage(s) {
			t.Errorf("ValidPhoneLanguage(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "e", "english", "en_US", "en-", "-US", "en US", "tr-TR\n",
		"en-" + strings.Repeat("abcdefgh-", 5) + "x1"} {
		if ValidPhoneLanguage(s) {
			t.Errorf("ValidPhoneLanguage(%q) = true, want false", s)
		}
	}
}

func TestValidPhoneCode(t *testing.T) {
	for _, s := range []string{"000000", "482193", "999999"} {
		if !ValidPhoneCode(s) {
			t.Errorf("ValidPhoneCode(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "48219", "4821930", "48219a", " 482193", "４８２１９３"} {
		if ValidPhoneCode(s) {
			t.Errorf("ValidPhoneCode(%q) = true, want false", s)
		}
	}
}

func TestPhoneChannelValidate(t *testing.T) {
	ok := []NotificationChannel{
		{ProjectID: "p", Name: "sms alice", Type: ChannelSMS, Recipient: PhoneRecipientUser, RecipientUserID: "u"},
		{ProjectID: "p", Name: "duty phone", Type: ChannelVoice, Recipient: PhoneRecipientNumber},
		{ProjectID: "p", Name: "empty config", Type: ChannelSMS, Recipient: PhoneRecipientNumber, Config: map[string]string{}},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%q: %v", c.Name, err)
		}
	}
	bad := map[string]NotificationChannel{
		"phone with config": {ProjectID: "p", Name: "x", Type: ChannelSMS, Recipient: PhoneRecipientNumber,
			Config: map[string]string{"url": "https://x"}},
		"no recipient mode": {ProjectID: "p", Name: "x", Type: ChannelVoice},
		"unknown mode":      {ProjectID: "p", Name: "x", Type: ChannelSMS, Recipient: "team"},
		"slack with a recipient": {ProjectID: "p", Name: "x", Type: ChannelSlack, Recipient: PhoneRecipientUser,
			Config: map[string]string{"url": "https://hooks.example/x"}},
		"webhook with a mask": {ProjectID: "p", Name: "x", Type: ChannelWebhook, NumberMasked: "+1•••••0147",
			Config: map[string]string{"url": "https://hooks.example/x"}},
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The four existing types serialize exactly as before FR-041a: none of the phone fields appears.
func TestNonPhoneChannelJSONUnchanged(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ch := NotificationChannel{ID: "c1", ProjectID: "p1", Type: ChannelSlack, Name: "ops",
		Config: map[string]string{"url": ""}, Enabled: true, CreatedAt: at}
	got, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"c1","project_id":"p1","type":"slack","name":"ops","config":{"url":""},"enabled":true,"created_at":"2026-11-17T10:00:00Z"}`
	if string(got) != want {
		t.Fatalf("slack JSON changed:\n got %s\nwant %s", got, want)
	}
}

func TestPhoneChannelJSON(t *testing.T) {
	verified := false
	ch := NotificationChannel{ID: "c1", ProjectID: "p1", Type: ChannelSMS, Name: "duty", Config: map[string]string{},
		Recipient: PhoneRecipientNumber, NumberMasked: "+1•••••0147", NumberVerified: &verified,
		DisabledReason: DeliverableNumberUnverified,
		Deliverable:    &ChannelDeliverable{Route: false, Reason: DeliverableNumberUnverified}}
	got, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"recipient":"number"`, `"number_masked":"+1•••••0147"`, `"number_verified":false`,
		`"disabled_reason":"number_unverified"`, `"deliverable":{"route":false,"reason":"number_unverified"}`} {
		if !strings.Contains(string(got), frag) {
			t.Errorf("phone channel JSON lacks %s: %s", frag, got)
		}
	}
	route, _ := json.Marshal(ChannelDeliverable{Route: true})
	if string(route) != `{"route":true}` {
		t.Fatalf("a route carries no reason: %s", route)
	}
}

// FR-041a review P2-7: the marker of an error returned before the phone routes were counted keeps the
// error's text — the event's failure reason — and its chain.
func TestPhoneRoutesUncountedKeepsTheError(t *testing.T) {
	inner := errors.New("notify: phone channels: db down")
	err := PhoneRoutesUncounted(inner)
	if err.Error() != inner.Error() || !errors.Is(err, inner) || !errors.Is(err, ErrPhoneRoutesUncounted) {
		t.Fatalf("marked %q: is inner %v, is uncounted %v", err, errors.Is(err, inner), errors.Is(err, ErrPhoneRoutesUncounted))
	}
	if errors.Is(inner, ErrPhoneRoutesUncounted) || PhoneRoutesUncounted(nil) != nil {
		t.Fatal("an unmarked error matches, or nil was marked")
	}
}
