// Package webhook delivers signed outbound HTTP notifications to subscribers on
// incident lifecycle events. Deliver reports an error if any webhook fails so the
// transactional outbox can retry the event.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// Store is the persistence surface the dispatcher needs.
type Store interface {
	ListEnabledWebhooksForProject(ctx context.Context, projectID string) ([]domain.Webhook, error)
}

// SignatureHeader carries the HMAC of the body; EventHeader names the event.
const (
	SignatureHeader = "X-Cerbix-Signature"
	EventHeader     = "X-Cerbix-Event"
)

// Sign returns the HMAC-SHA256 of body under secret, as "sha256=<hex>".
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Dispatcher signs and POSTs an incident event to a project's webhooks.
type Dispatcher struct {
	store   Store
	client  *http.Client
	timeout time.Duration
}

// New builds a dispatcher. A nil client gets a sane default with a short timeout.
func New(store Store, client *http.Client) *Dispatcher {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Dispatcher{store: store, client: client, timeout: 10 * time.Second}
}

// Deliver POSTs the signed event to every enabled webhook of the event's project.
// It returns a joined error if any delivery fails so the outbox retries the whole
// event; nil means every webhook (possibly none) accepted it.
func (d *Dispatcher) Deliver(ctx context.Context, ev domain.IncidentEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: marshal event: %w", err)
	}
	hooks, err := d.store.ListEnabledWebhooksForProject(ctx, ev.Incident.ProjectID)
	if err != nil {
		return fmt.Errorf("webhook: list for %s: %w", ev.Incident.ProjectID, err)
	}
	var errs []error
	for _, h := range hooks {
		if err := d.post(ctx, h, ev.Type, body); err != nil {
			errs = append(errs, fmt.Errorf("webhook %s: %w", h.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (d *Dispatcher) post(ctx context.Context, h domain.Webhook, eventType string, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventHeader, eventType)
	req.Header.Set(SignatureHeader, Sign(h.Secret, body))
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
