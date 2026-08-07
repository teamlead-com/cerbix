package domain

import (
	"fmt"
	"strings"
	"time"
)

// ChannelType is a notification delivery mechanism.
type ChannelType string

const (
	// ChannelWebhook POSTs a JSON event to a URL.
	ChannelWebhook ChannelType = "webhook"
	// ChannelSlack POSTs a Slack incoming-webhook message.
	ChannelSlack ChannelType = "slack"
	// ChannelTelegram sends a message via the Telegram Bot API.
	ChannelTelegram ChannelType = "telegram"
	// ChannelEmail sends an email via SMTP.
	ChannelEmail ChannelType = "email"
)

// Valid reports whether t is a known channel type.
func (t ChannelType) Valid() bool {
	switch t {
	case ChannelWebhook, ChannelSlack, ChannelTelegram, ChannelEmail:
		return true
	default:
		return false
	}
}

// NotificationChannel is a per-project delivery target that monitors can be
// linked to. Config holds the type-specific settings (a URL, or bot_token +
// chat_id for Telegram).
type NotificationChannel struct {
	ID        string            `json:"id"`
	ProjectID string            `json:"project_id"`
	Type      ChannelType       `json:"type"`
	Name      string            `json:"name"`
	Config    map[string]string `json:"config"`
	Enabled   bool              `json:"enabled"`
	CreatedAt time.Time         `json:"created_at"`
}

// SecretChannelConfigKeys are the channel config values that carry a credential
// (or a URL that embeds one, like a Slack/webhook token). They are blanked by
// Redacted() before a channel is returned to a client — list responses are
// visible to viewers, who must not read another team's bot token or SMTP password.
var SecretChannelConfigKeys = map[string]bool{
	"url":           true, // Slack/webhook URLs embed the auth token in the path
	"bot_token":     true, // Telegram
	"smtp_password": true, // email
}

// Redacted returns a copy of the channel with secret config values blanked, safe
// to serialize into an API response. The original is not mutated. A key that was
// set is replaced with the empty string (its presence is still observable via the
// key, but the value is gone); the corresponding create/edit UI re-collects the
// secret rather than round-tripping it.
func (c NotificationChannel) Redacted() NotificationChannel {
	if c.Config == nil {
		return c
	}
	cfg := make(map[string]string, len(c.Config))
	for k, v := range c.Config {
		if SecretChannelConfigKeys[k] {
			cfg[k] = ""
			continue
		}
		cfg[k] = v
	}
	c.Config = cfg
	return c
}

// Validate enforces channel invariants, including type-specific required config.
func (c NotificationChannel) Validate() error {
	if c.ProjectID == "" {
		return fmt.Errorf("notification channel: project_id is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("notification channel: name is required")
	}
	if !c.Type.Valid() {
		return fmt.Errorf("notification channel: unknown type %q", c.Type)
	}
	switch c.Type {
	case ChannelWebhook, ChannelSlack:
		u := strings.TrimSpace(c.Config["url"])
		if u == "" {
			return fmt.Errorf("notification channel: %s requires config.url", c.Type)
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("notification channel: url must be http(s)")
		}
	case ChannelTelegram:
		if strings.TrimSpace(c.Config["bot_token"]) == "" || strings.TrimSpace(c.Config["chat_id"]) == "" {
			return fmt.Errorf("notification channel: telegram requires config.bot_token and config.chat_id")
		}
	case ChannelEmail:
		if strings.TrimSpace(c.Config["to"]) == "" || strings.TrimSpace(c.Config["smtp_host"]) == "" || strings.TrimSpace(c.Config["from"]) == "" {
			return fmt.Errorf("notification channel: email requires config.to, config.smtp_host and config.from")
		}
	}
	return nil
}
