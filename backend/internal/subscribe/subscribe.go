// Package subscribe emails confirmed status-page subscribers on incident
// lifecycle events. It plugs into the transactional outbox so delivery is
// retried like webhooks.
package subscribe

import (
	"context"
	"fmt"
	"strings"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// Store lists the confirmed subscriber emails affected by an incident's project.
type Store interface {
	ConfirmedSubscriberEmailsForProject(ctx context.Context, projectID string) ([]string, error)
}

// Sender sends a plain-text email.
type Sender interface {
	Send(to, subject, body string) error
	BaseURL() string
}

// Notifier fans an incident event out to a project's confirmed subscribers.
type Notifier struct {
	store  Store
	mailer Sender
}

// New builds a Notifier.
func New(store Store, mailer Sender) *Notifier {
	return &Notifier{store: store, mailer: mailer}
}

// Deliver emails the incident event to every confirmed subscriber of a status
// page that surfaces the incident's project. Best-effort per recipient; returns
// the first send error (so the outbox retries).
func (n *Notifier) Deliver(ctx context.Context, ev domain.IncidentEvent) error {
	emails, err := n.store.ConfirmedSubscriberEmailsForProject(ctx, ev.Incident.ProjectID)
	if err != nil {
		return err
	}
	if len(emails) == 0 {
		return nil
	}
	subject, body := render(ev)
	var firstErr error
	for _, to := range emails {
		if err := n.mailer.Send(to, subject, body); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func render(ev domain.IncidentEvent) (string, string) {
	inc := ev.Incident
	verb := map[string]string{
		domain.EventIncidentOpened:   "opened",
		domain.EventIncidentUpdated:  "updated",
		domain.EventIncidentResolved: "resolved",
	}[ev.Type]
	if verb == "" {
		verb = "updated"
	}
	subject := fmt.Sprintf("[%s] %s", inc.Status, inc.Title)
	var b strings.Builder
	fmt.Fprintf(&b, "Incident %s: %s\n\nStatus: %s\nImpact: %s\n", verb, inc.Title, inc.Status, inc.Impact)
	if ev.Update != nil && ev.Update.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", ev.Update.Body)
	}
	return subject, b.String()
}
