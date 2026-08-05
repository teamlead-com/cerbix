package domain

import (
	"testing"
	"time"
)

func TestBrandingValidate(t *testing.T) {
	if err := (Branding{AccentColor: "#3b5bdb"}).Validate(); err != nil {
		t.Fatalf("valid hex rejected: %v", err)
	}
	if err := (Branding{AccentColor: "blue"}).Validate(); err == nil {
		t.Fatal("non-hex accent should be rejected")
	}
	if err := (Branding{Announcement: Announcement{Enabled: true, Text: ""}}).Validate(); err == nil {
		t.Fatal("enabled announcement without text should be rejected")
	}
	if err := (Branding{Announcement: Announcement{Level: "boom"}}).Validate(); err == nil {
		t.Fatal("bad announcement level should be rejected")
	}
}

func TestAuthPolicyValidateAndHelpers(t *testing.T) {
	if err := (AuthPolicy{MinPasswordLen: 3, SessionTTLSeconds: 3600}).Validate(); err == nil {
		t.Fatal("too-short min password should be rejected")
	}
	if err := (AuthPolicy{MinPasswordLen: 8, SessionTTLSeconds: 60}).Validate(); err == nil {
		t.Fatal("too-short session ttl should be rejected")
	}
	if err := (AuthPolicy{MinPasswordLen: 8, SessionTTLSeconds: 3600, RequireTOTP: "sometimes"}).Validate(); err == nil {
		t.Fatal("bad require_totp should be rejected")
	}

	p := AuthPolicy{AllowedEmailDomains: []string{" @Example.com ", "example.com", ""}}
	p.Normalize()
	if len(p.AllowedEmailDomains) != 1 || p.AllowedEmailDomains[0] != "example.com" {
		t.Fatalf("normalize = %#v", p.AllowedEmailDomains)
	}
	if p.RequireTOTP != TOTPNone {
		t.Fatalf("require_totp default = %q, want none", p.RequireTOTP)
	}
	if !p.EmailAllowed("a@example.com") || p.EmailAllowed("a@evil.com") {
		t.Fatal("EmailAllowed domain gate wrong")
	}
	if !(AuthPolicy{}).EmailAllowed("anyone@anywhere") {
		t.Fatal("empty allow list should permit any domain")
	}
	if !(AuthPolicy{RequireTOTP: TOTPAll}).TOTPRequiredFor(false) {
		t.Fatal("require all should apply to non-admins")
	}
	if (AuthPolicy{RequireTOTP: TOTPAdmins}).TOTPRequiredFor(false) {
		t.Fatal("require admins should not apply to non-admins")
	}
	if !(AuthPolicy{RequireTOTP: TOTPAdmins}).TOTPRequiredFor(true) {
		t.Fatal("require admins should apply to admins")
	}
}

func TestAlertingSilenced(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	if (Alerting{}).Silenced(now) {
		t.Fatal("default should not be silenced")
	}
	if !(Alerting{GlobalSilence: GlobalSilence{Enabled: true}}).Silenced(now) {
		t.Fatal("enabled silence with no expiry should be active")
	}
	past := now.Add(-time.Hour)
	if (Alerting{GlobalSilence: GlobalSilence{Enabled: true, Until: &past}}).Silenced(now) {
		t.Fatal("expired silence should be inactive")
	}
	future := now.Add(time.Hour)
	if !(Alerting{GlobalSilence: GlobalSilence{Enabled: true, Until: &future}}).Silenced(now) {
		t.Fatal("silence with future expiry should be active")
	}
}

func TestMonitorDefaultsValidate(t *testing.T) {
	if err := (MonitorDefaults{IntervalSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 1}).Validate(); err != nil {
		t.Fatalf("valid defaults rejected: %v", err)
	}
	if err := (MonitorDefaults{IntervalSeconds: 1, TimeoutSeconds: 10, FailureThreshold: 1}).Validate(); err == nil {
		t.Fatal("interval below 5 should be rejected")
	}
	if err := (MonitorDefaults{IntervalSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 0}).Validate(); err == nil {
		t.Fatal("failure_threshold below 1 should be rejected")
	}
}

func TestMailSettingsValidateAndHelpers(t *testing.T) {
	if err := (MailSettings{Enabled: false}).Validate(); err != nil {
		t.Fatalf("disabled mail should validate: %v", err)
	}
	if err := (MailSettings{Enabled: true, SMTPHost: "", From: "a@x"}).Validate(); err == nil {
		t.Fatal("enabled mail without host should be rejected")
	}
	if err := (MailSettings{Enabled: true, SMTPHost: "smtp", From: "a@x", SMTPPort: 99999}).Validate(); err == nil {
		t.Fatal("out-of-range port should be rejected")
	}
	if (MailSettings{Enabled: true, SMTPHost: "smtp", From: ""}).Deliverable() {
		t.Fatal("no from → not deliverable")
	}
	if !(MailSettings{Enabled: true, SMTPHost: "smtp", From: "a@x"}).Deliverable() {
		t.Fatal("host+from+enabled → deliverable")
	}
	if (MailSettings{SMTPPort: 0}).Port() != 587 || (MailSettings{SMTPPort: 25}).Port() != 25 {
		t.Fatal("Port default wrong")
	}
}
