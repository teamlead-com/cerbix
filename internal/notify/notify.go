// Package notify delivers monitor up/down transition notifications to a
// monitor's linked channels (webhook, Slack, Telegram, email). Deliver reports an
// error if any channel fails so the transactional outbox can retry the event.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// telegramAPI is the Telegram Bot API base; a var so tests can point it at a stub.
var telegramAPI = "https://api.telegram.org"

// sendMailFunc is the SMTP sender (a var so tests can capture messages without a
// real mail server).
var sendMailFunc = smtp.SendMail

// Store is the persistence surface the dispatcher needs.
type Store interface {
	ListEnabledChannelsForMonitor(ctx context.Context, monitorID string) ([]domain.NotificationChannel, error)
	ListEnabledChannelsByProject(ctx context.Context, projectID string) ([]domain.NotificationChannel, error)
	EnabledChannelsByIDs(ctx context.Context, ids []string) ([]domain.NotificationChannel, error)
}

// Dispatcher delivers a monitor transition to the monitor's channels.
type Dispatcher struct {
	store   Store
	client  *http.Client
	timeout time.Duration
}

// New builds a dispatcher. A nil client gets a default with a short timeout.
func New(store Store, client *http.Client) *Dispatcher {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Dispatcher{store: store, client: client, timeout: 10 * time.Second}
}

// Deliver sends the transition to every enabled channel linked to the monitor.
// It returns a joined error if any channel fails (or channels can't be listed) so
// the outbox retries the whole event; nil means every channel was delivered. A
// monitor with no channels is a successful no-op.
func (d *Dispatcher) Deliver(ctx context.Context, monitor domain.Monitor, up bool) error {
	channels, err := d.store.ListEnabledChannelsForMonitor(ctx, monitor.ID)
	if err != nil {
		return fmt.Errorf("notify: list channels for %s: %w", monitor.ID, err)
	}
	var errs []error
	for _, ch := range channels {
		if ch.Type == domain.ChannelEmail {
			if err := d.sendEmail(ch, monitor, up); err != nil {
				errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
			}
			continue
		}
		target, body, contentType, err := render(ch, monitor, up)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s render: %w", ch.ID, err))
			continue
		}
		if err := d.post(ctx, ch, target, contentType, body); err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
		}
	}
	return errors.Join(errs...)
}

// DeliverText sends an arbitrary text message (e.g. an SLO burn-rate alert) to
// every enabled channel linked to the monitor, with the same per-channel error
// aggregation as Deliver so the outbox can retry the whole event.
func (d *Dispatcher) DeliverText(ctx context.Context, monitor domain.Monitor, text string) error {
	channels, err := d.store.ListEnabledChannelsForMonitor(ctx, monitor.ID)
	if err != nil {
		return fmt.Errorf("notify: list channels for %s: %w", monitor.ID, err)
	}
	var errs []error
	for _, ch := range channels {
		if ch.Type == domain.ChannelEmail {
			if err := d.sendMail(ch, text); err != nil {
				errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
			}
			continue
		}
		target, body, contentType, err := renderText(ch, monitor, text)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s render: %w", ch.ID, err))
			continue
		}
		if err := d.post(ctx, ch, target, contentType, body); err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
		}
	}
	return errors.Join(errs...)
}

// DeliverProjectText sends a free-text message (e.g. a weekly SLA report) to every
// enabled channel in the project, aggregating per-channel errors for outbox retry.
func (d *Dispatcher) DeliverProjectText(ctx context.Context, projectID, text string) error {
	channels, err := d.store.ListEnabledChannelsByProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("notify: list channels for project %s: %w", projectID, err)
	}
	var errs []error
	for _, ch := range channels {
		if ch.Type == domain.ChannelEmail {
			if err := d.sendMail(ch, text); err != nil {
				errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
			}
			continue
		}
		target, body, contentType, err := renderProjectText(ch, projectID, text)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s render: %w", ch.ID, err))
			continue
		}
		if err := d.post(ctx, ch, target, contentType, body); err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
		}
	}
	return errors.Join(errs...)
}

// DeliverChannels sends a free-text message (an on-call escalation step) to a specific
// set of enabled channels (resolved by the scheduler at fire time), with the same
// per-channel error aggregation as the other Deliver methods for outbox retry.
func (d *Dispatcher) DeliverChannels(ctx context.Context, channelIDs []string, text string) error {
	channels, err := d.store.EnabledChannelsByIDs(ctx, channelIDs)
	if err != nil {
		return fmt.Errorf("notify: channels by ids: %w", err)
	}
	var errs []error
	for _, ch := range channels {
		if ch.Type == domain.ChannelEmail {
			if err := d.sendMail(ch, text); err != nil {
				errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
			}
			continue
		}
		target, body, contentType, err := renderAlertText(ch, text)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s render: %w", ch.ID, err))
			continue
		}
		if err := d.post(ctx, ch, target, contentType, body); err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", ch.ID, err))
		}
	}
	return errors.Join(errs...)
}

// renderAlertText produces the (url, body, content-type) for a context-free alert
// text (e.g. an escalation step) delivered to a specific channel.
func renderAlertText(ch domain.NotificationChannel, text string) (string, []byte, string, error) {
	switch ch.Type {
	case domain.ChannelWebhook:
		body, err := json.Marshal(map[string]any{"event": "escalation", "text": text})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelSlack:
		body, err := json.Marshal(map[string]string{"text": text})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelTelegram:
		target := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPI, ch.Config["bot_token"])
		body, err := json.Marshal(map[string]string{"chat_id": ch.Config["chat_id"], "text": text})
		return target, body, "application/json", err
	default:
		return "", nil, "", fmt.Errorf("notify: unknown channel type %q", ch.Type)
	}
}

// sendEmail delivers a monitor transition email via SMTP.
func (d *Dispatcher) sendEmail(ch domain.NotificationChannel, monitor domain.Monitor, up bool) error {
	return d.sendMail(ch, Message(monitor, up))
}

// sendMail delivers a plain-text email (subject == body == text) via SMTP.
func (d *Dispatcher) sendMail(ch domain.NotificationChannel, text string) error {
	host := ch.Config["smtp_host"]
	port := ch.Config["smtp_port"]
	if port == "" {
		port = "587"
	}
	from := ch.Config["from"]
	var to []string
	for _, addr := range strings.Split(ch.Config["to"], ",") {
		if a := strings.TrimSpace(addr); a != "" {
			to = append(to, a)
		}
	}
	if len(to) == 0 {
		return fmt.Errorf("no recipients")
	}
	var auth smtp.Auth
	if user := ch.Config["smtp_username"]; user != "" {
		auth = smtp.PlainAuth("", user, ch.Config["smtp_password"], host)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n",
		from, strings.Join(to, ", "), text, text)
	if err := sendMailFunc(host+":"+port, auth, from, to, []byte(msg)); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return nil
}

func (d *Dispatcher) post(ctx context.Context, _ domain.NotificationChannel, target, contentType string, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("non-2xx status %d", resp.StatusCode)
	}
	return nil
}

// Message returns the human-readable transition text.
func Message(monitor domain.Monitor, up bool) string {
	if up {
		return fmt.Sprintf("🟢 %s recovered — it is back UP.", monitor.Name)
	}
	return fmt.Sprintf("🔴 %s is DOWN.", monitor.Name)
}

// render produces the (url, body, content-type) for a monitor transition.
func render(ch domain.NotificationChannel, monitor domain.Monitor, up bool) (string, []byte, string, error) {
	text := Message(monitor, up)
	switch ch.Type {
	case domain.ChannelWebhook:
		body, err := json.Marshal(map[string]any{
			"event":      "monitor.transition",
			"monitor_id": monitor.ID,
			"monitor":    monitor.Name,
			"project_id": monitor.ProjectID,
			"up":         up,
			"text":       text,
		})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelSlack:
		body, err := json.Marshal(map[string]string{"text": text})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelTelegram:
		target := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPI, ch.Config["bot_token"])
		body, err := json.Marshal(map[string]string{"chat_id": ch.Config["chat_id"], "text": text})
		return target, body, "application/json", err
	default:
		return "", nil, "", fmt.Errorf("notify: unknown channel type %q", ch.Type)
	}
}

// renderText produces the (url, body, content-type) for a free-text alert
// (e.g. an SLO burn-rate alert) that carries no up/down transition.
func renderText(ch domain.NotificationChannel, monitor domain.Monitor, text string) (string, []byte, string, error) {
	switch ch.Type {
	case domain.ChannelWebhook:
		body, err := json.Marshal(map[string]any{
			"event":      "monitor.alert",
			"monitor_id": monitor.ID,
			"monitor":    monitor.Name,
			"project_id": monitor.ProjectID,
			"text":       text,
		})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelSlack:
		body, err := json.Marshal(map[string]string{"text": text})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelTelegram:
		target := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPI, ch.Config["bot_token"])
		body, err := json.Marshal(map[string]string{"chat_id": ch.Config["chat_id"], "text": text})
		return target, body, "application/json", err
	default:
		return "", nil, "", fmt.Errorf("notify: unknown channel type %q", ch.Type)
	}
}

// renderProjectText produces the (url, body, content-type) for a project-level
// free-text message (e.g. an SLA report) that has no monitor context.
func renderProjectText(ch domain.NotificationChannel, projectID, text string) (string, []byte, string, error) {
	switch ch.Type {
	case domain.ChannelWebhook:
		body, err := json.Marshal(map[string]any{
			"event":      "sla.report",
			"project_id": projectID,
			"text":       text,
		})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelSlack:
		body, err := json.Marshal(map[string]string{"text": text})
		return ch.Config["url"], body, "application/json", err
	case domain.ChannelTelegram:
		target := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPI, ch.Config["bot_token"])
		body, err := json.Marshal(map[string]string{"chat_id": ch.Config["chat_id"], "text": text})
		return target, body, "application/json", err
	default:
		return "", nil, "", fmt.Errorf("notify: unknown channel type %q", ch.Type)
	}
}
