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
