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
	case ChannelWebhook, ChannelSlack, ChannelTelegram, ChannelEmail, ChannelSMS, ChannelVoice:
		return true
	default:
		return false
	}
}

// NotificationChannel is a per-project delivery target that monitors can be
// linked to. Config holds the type-specific settings (a URL, or bot_token +
// chat_id for Telegram).
//
// The fields after CreatedAt belong to the phone types (sms, voice; FR-041a §4.3) and are typed
// columns, not config keys. Every one is omitempty, so the JSON of the four other types is byte for
// byte what it was. A phone channel's number is never a field here: the store keeps its ciphertext,
// and a reader sees the mask (D35).
type NotificationChannel struct {
	ID        string            `json:"id"`
	ProjectID string            `json:"project_id"`
	Type      ChannelType       `json:"type"`
	Name      string            `json:"name"`
	Config    map[string]string `json:"config"`
	Enabled   bool              `json:"enabled"`
	CreatedAt time.Time         `json:"created_at"`

	// Recipient is the mode, PhoneRecipientUser or PhoneRecipientNumber, fixed at creation.
	Recipient string `json:"recipient,omitempty"`
	// RecipientUserID is the person a `user` channel phones; empty once that user is deleted.
	RecipientUserID string `json:"recipient_user_id,omitempty"`
	// NumberMasked and NumberVerified describe a `number` channel's own number.
	NumberMasked   string `json:"number_masked,omitempty"`
	NumberVerified *bool  `json:"number_verified,omitempty"`
	// DisabledReason says why the server disabled the channel: DeliverableRecipientDeleted or
	// DeliverableNumberUnverified.
	DisabledReason string `json:"disabled_reason,omitempty"`
	// Deliverable is whether the channel is a route now (D31 rule 1, R11). Server only.
	Deliverable *ChannelDeliverable `json:"deliverable,omitempty"`
	// RecipientDisplayName is a `user` channel's recipient by display name, never an email: null
	// once the user is deleted, absent for a `number` channel and the four other types (§4.3).
	RecipientDisplayName RecipientName `json:"recipient_display_name,omitzero"`
	// OnlyRouteFor and OnlyRouteForTotal are the paging services for which this phone channel is
	// the only route (§4.3), at most 50 of them, and how many there are. Set by the API on the
	// channels it answers with; nil — absent — on the four other types.
	OnlyRouteFor      *[]ServiceWithoutRoute `json:"only_route_for,omitempty"`
	OnlyRouteForTotal *int                   `json:"only_route_for_total,omitempty"`
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

// MergeChannelConfig applies a client's config patch to the config a channel
// already has, and returns the result; stored is not mutated.
//
// The rule exists because Redacted() means a secret never reaches the client: an
// edit form has nothing to round-trip. So a SECRET key the client left blank —
// absent, or present and empty — keeps the value already stored, and only a
// non-empty one replaces it. Every other key is exactly what the client sent, so an
// optional field can still be cleared by sending it empty. A key the patch does not
// mention keeps its stored value; the type-specific required set is enforced by
// Validate on the merged result, so no edit can leave a channel undeliverable.
func MergeChannelConfig(stored, patch map[string]string) map[string]string {
	out := make(map[string]string, len(stored)+len(patch))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range patch {
		if SecretChannelConfigKeys[k] && strings.TrimSpace(v) == "" {
			continue
		}
		out[k] = v
	}
	return out
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
	if !IsPhoneChannelType(c.Type) && (c.Recipient != "" || c.RecipientUserID != "" || c.NumberMasked != "" ||
		c.NumberVerified != nil || c.DisabledReason != "") {
		return fmt.Errorf("notification channel: recipient and number belong to sms and voice channels only")
	}
	switch c.Type {
	case ChannelSMS, ChannelVoice:
		// What a phone channel holds is typed fields, never config keys (§4.3).
		if len(c.Config) > 0 {
			return fmt.Errorf("notification channel: %s carries no config", c.Type)
		}
		if c.Recipient != PhoneRecipientUser && c.Recipient != PhoneRecipientNumber {
			return fmt.Errorf("notification channel: %s requires recipient user or number", c.Type)
		}
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
		// The address rule of every mail cerbix sends (sec-hardening-3 S18), on the values exactly as
		// the channel sender sends them: trimmed. The error names the field and position, never the value.
		if !ValidMailAddress(strings.TrimSpace(c.Config["from"])) {
			return fmt.Errorf("notification channel: email config.from must be a single bare address (e.g. alerts@example.com)")
		}
		position := 0
		for _, element := range strings.Split(c.Config["to"], ",") {
			element = strings.TrimSpace(element)
			if element == "" {
				continue
			}
			position++
			if !ValidMailAddress(element) {
				return fmt.Errorf("notification channel: email config.to address %d must be a single bare address (e.g. ops@example.com)", position)
			}
		}
		// A `to` of nothing but commas and spaces names no recipient: refused like an empty one, rather
		// than accepted to fail every send with "no recipients" (code review of sec-hardening-3, P3-1).
		if position == 0 {
			return fmt.Errorf("notification channel: email requires config.to, config.smtp_host and config.from")
		}
	}
	return nil
}
