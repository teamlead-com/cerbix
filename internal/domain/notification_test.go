package domain

import "testing"

func TestNotificationChannelValidate(t *testing.T) {
	ok := []NotificationChannel{
		{ProjectID: "p1", Type: ChannelWebhook, Name: "w", Config: map[string]string{"url": "https://x"}},
		{ProjectID: "p1", Type: ChannelSlack, Name: "s", Config: map[string]string{"url": "http://x"}},
		{ProjectID: "p1", Type: ChannelTelegram, Name: "t", Config: map[string]string{"bot_token": "b", "chat_id": "c"}},
		{ProjectID: "p1", Type: ChannelEmail, Name: "e", Config: map[string]string{"to": "a@x", "smtp_host": "mail", "from": "c@x"}},
	}
	for i, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("valid channel %d rejected: %v", i, err)
		}
	}
	bad := map[string]NotificationChannel{
		"no project":       {Type: ChannelWebhook, Name: "w", Config: map[string]string{"url": "https://x"}},
		"no name":          {ProjectID: "p1", Type: ChannelWebhook, Config: map[string]string{"url": "https://x"}},
		"bad type":         {ProjectID: "p1", Type: "sms", Name: "x"},
		"webhook no url":   {ProjectID: "p1", Type: ChannelWebhook, Name: "w", Config: map[string]string{}},
		"webhook bad url":  {ProjectID: "p1", Type: ChannelWebhook, Name: "w", Config: map[string]string{"url": "ftp://x"}},
		"telegram no chat": {ProjectID: "p1", Type: ChannelTelegram, Name: "t", Config: map[string]string{"bot_token": "b"}},
		"email no to":      {ProjectID: "p1", Type: ChannelEmail, Name: "e", Config: map[string]string{"smtp_host": "mail", "from": "c@x"}},
		"email no host":    {ProjectID: "p1", Type: ChannelEmail, Name: "e", Config: map[string]string{"to": "a@x", "from": "c@x"}},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if err := c.Validate(); err == nil {
				t.Errorf("expected %s to fail", name)
			}
		})
	}
}

// TestMergeChannelConfig pins the edit contract. A secret value never reaches a
// client (Redacted blanks it), so an edit form has nothing to send back: a blank
// secret must keep what is stored, while a non-secret field is whatever the client
// sent — including empty, which is how an optional field is cleared.
func TestMergeChannelConfig(t *testing.T) {
	stored := map[string]string{"bot_token": "STORED-TOKEN", "chat_id": "42"}

	got := MergeChannelConfig(stored, map[string]string{"bot_token": "", "chat_id": "77"})
	if got["bot_token"] != "STORED-TOKEN" {
		t.Errorf("blank secret overwrote the stored one: %q", got["bot_token"])
	}
	if got["chat_id"] != "77" {
		t.Errorf("non-secret not replaced: %q", got["chat_id"])
	}

	// A field the user never focused arrives as whitespace in some browsers; it is
	// still "left blank".
	if got := MergeChannelConfig(stored, map[string]string{"bot_token": "   "}); got["bot_token"] != "STORED-TOKEN" {
		t.Errorf("whitespace secret overwrote the stored one: %q", got["bot_token"])
	}

	// A real new secret replaces, and an absent key keeps its stored value.
	got = MergeChannelConfig(stored, map[string]string{"bot_token": "NEW-TOKEN"})
	if got["bot_token"] != "NEW-TOKEN" {
		t.Errorf("new secret not applied: %q", got["bot_token"])
	}
	if got["chat_id"] != "42" {
		t.Errorf("absent key lost its stored value: %q", got["chat_id"])
	}

	// A non-secret CAN be cleared — that is how an optional field is removed.
	if got := MergeChannelConfig(map[string]string{"url": "https://x", "note": "n"},
		map[string]string{"note": ""}); got["note"] != "" {
		t.Errorf("non-secret not cleared: %q", got["note"])
	}

	// The stored map is never mutated: the caller still holds the channel it read.
	if stored["bot_token"] != "STORED-TOKEN" || stored["chat_id"] != "42" {
		t.Fatalf("stored config mutated: %v", stored)
	}

	// The merged result is what Validate judges, so a cleared required field is a
	// refusal rather than a broken channel.
	ch := NotificationChannel{ProjectID: "p1", Type: ChannelTelegram, Name: "tg",
		Config: MergeChannelConfig(stored, map[string]string{"chat_id": ""})}
	if err := ch.Validate(); err == nil {
		t.Fatal("clearing chat_id must not validate")
	}
}
