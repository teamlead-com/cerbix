// Package subscribe emails confirmed status-page subscribers on incident
// lifecycle events. It plugs into the transactional outbox so delivery is
// retried like webhooks.
package subscribe

import (
	"context"
	"fmt"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
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
//
// An impact rise (FR-042 §4.5, I8) is mailed only when it reaches critical: a rise to minor or major
// still goes to webhooks — `incidentFanout` calls the webhook dispatcher before this — but is not
// worth a subscriber's inbox. Every event that is not a rise is mailed exactly as before.
func (n *Notifier) Deliver(ctx context.Context, ev domain.IncidentEvent) error {
	if ev.ImpactRaised != nil && ev.ImpactRaised.To != domain.ImpactCritical {
		return nil
	}
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
	if r := ev.ImpactRaised; r != nil {
		// The rise template (§4.5): the subject leads with the new impact, not the status — the
		// status did not change, and the rise is the news.
		subject := fmt.Sprintf("[%s] %s", r.To, inc.Title)
		var b strings.Builder
		fmt.Fprintf(&b, "Incident impact raised to %s: %s\n\nStatus: %s\nImpact: %s (was %s)\n",
			r.To, inc.Title, inc.Status, r.To, r.From)
		if ev.Update != nil && ev.Update.Body != "" {
			fmt.Fprintf(&b, "\n%s\n", ev.Update.Body)
		}
		return subject, b.String()
	}
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
