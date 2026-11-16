package domain

import "testing"

// TestAddressesAreSingleAndUnnamedAtWrite (sec-hardening-3 T-3, S18): every write surface that stores
// a mail address refuses what the message builder would refuse, and accepts what it sends.
func TestAddressesAreSingleAndUnnamedAtWrite(t *testing.T) {
	refused := []string{"Ops <ops@example.com>", "a@b,c@d", "a b@c", "üser@example.com", "ops@example.com\x7f"}
	accepted := []string{"a@b.example", "a@[192.0.2.1]"}
	surfaces := map[string]func(addr string) error{
		"subscriber": func(a string) error { return Subscriber{StatusPageID: "p", Email: a}.Validate() },
		"channel from": func(a string) error {
			return NotificationChannel{ProjectID: "p", Name: "n", Type: ChannelEmail,
				Config: map[string]string{"smtp_host": "mail", "from": a, "to": "ops@example.com"}}.Validate()
		},
		"channel to element": func(a string) error {
			return NotificationChannel{ProjectID: "p", Name: "n", Type: ChannelEmail,
				Config: map[string]string{"smtp_host": "mail", "from": "status@example.com", "to": "ops@example.com, " + a}}.Validate()
		},
		"mail settings from": func(a string) error {
			return MailSettings{Enabled: true, SMTPHost: "smtp", SMTPPort: 587, From: a}.Validate()
		},
	}
	for name, validate := range surfaces {
		for _, a := range refused {
			if name == "channel to element" && a == "a@b,c@d" {
				continue // a comma separates `to` elements: there it is two good addresses
			}
			if validate(a) == nil {
				t.Errorf("%s accepted %q", name, a)
			}
		}
		for _, a := range accepted {
			if err := validate(a); err != nil {
				t.Errorf("%s refused %q: %v", name, a, err)
			}
		}
	}
	// A channel `from` with surrounding spaces is judged trimmed, and is sent trimmed.
	if err := (NotificationChannel{ProjectID: "p", Name: "n", Type: ChannelEmail,
		Config: map[string]string{"smtp_host": "mail", "from": "  ops@example.com  ", "to": "a@example.com"}}).Validate(); err != nil {
		t.Errorf("a padded channel from was refused: %v", err)
	}
}

// TestAnEmailChannelNeedsARecipient (code review of sec-hardening-3, P3-1): a `to` of nothing but
// commas and spaces names no recipient, so it is refused at write with the message an empty `to` gets.
// Before, it was accepted, and every send failed with "no recipients".
func TestAnEmailChannelNeedsARecipient(t *testing.T) {
	empty := NotificationChannel{ProjectID: "p", Name: "n", Type: ChannelEmail,
		Config: map[string]string{"smtp_host": "mail", "from": "status@example.com", "to": ""}}.Validate()
	if empty == nil {
		t.Fatal("an empty to was accepted")
	}
	for _, to := range []string{",", " , , "} {
		err := NotificationChannel{ProjectID: "p", Name: "n", Type: ChannelEmail,
			Config: map[string]string{"smtp_host": "mail", "from": "status@example.com", "to": to}}.Validate()
		if err == nil || err.Error() != empty.Error() {
			t.Errorf("to %q: error %v, want %q", to, err, empty)
		}
	}
}
