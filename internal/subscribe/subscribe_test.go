package subscribe

import (
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type fakeStore struct{ emails []string }

func (f fakeStore) ConfirmedSubscriberEmailsForProject(_ context.Context, _ string) ([]string, error) {
	return f.emails, nil
}

type fakeSender struct{ sent []string }

func (f *fakeSender) Send(to, _, _ string) error { f.sent = append(f.sent, to); return nil }
func (f *fakeSender) BaseURL() string            { return "https://status.example" }

func TestNotifierFansOut(t *testing.T) {
	sender := &fakeSender{}
	n := New(fakeStore{emails: []string{"a@x", "b@x"}}, sender)
	ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ProjectID: "p1", Title: "down", Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor}}
	if err := n.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(sender.sent) != 2 {
		t.Fatalf("sent to %d, want 2", len(sender.sent))
	}

	// No subscribers → no-op.
	sender2 := &fakeSender{}
	n2 := New(fakeStore{}, sender2)
	if err := n2.Deliver(context.Background(), ev); err != nil || len(sender2.sent) != 0 {
		t.Fatalf("empty deliver = %v / %d sent", err, len(sender2.sent))
	}
}

// mail is one message a capturingSender recorded.
type mail struct{ to, subject, body string }

// capturingSender records every message with its subject and body, so a test can hold the rendered
// mail to a golden value.
type capturingSender struct{ sent []mail }

func (c *capturingSender) Send(to, subject, body string) error {
	c.sent = append(c.sent, mail{to, subject, body})
	return nil
}
func (c *capturingSender) BaseURL() string { return "https://status.example" }

// FR-042 I8 — every incident event that does not report an impact rise is mailed exactly as it was
// before FR-042, byte for byte. Written and run against the code before the rise rule existed.
func TestEveryNonRiseEventIsMailedExactlyAsBefore(t *testing.T) {
	cases := []struct {
		name    string
		ev      domain.IncidentEvent
		subject string
		body    string
	}{
		{
			name: "opened, no update",
			ev: domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{
				ProjectID: "p1", Title: "Checkout down", Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor}},
			subject: "[investigating] Checkout down",
			body:    "Incident opened: Checkout down\n\nStatus: investigating\nImpact: major\n",
		},
		{
			name: "updated, with an update body",
			ev: domain.IncidentEvent{Type: domain.EventIncidentUpdated, Incident: domain.Incident{
				ProjectID: "p1", Title: "Checkout down", Status: domain.IncidentIdentified, Impact: domain.ImpactMinor},
				Update: &domain.IncidentUpdate{Body: "Root cause found."}},
			subject: "[identified] Checkout down",
			body:    "Incident updated: Checkout down\n\nStatus: identified\nImpact: minor\n\nRoot cause found.\n",
		},
		{
			name: "resolved, with an update body",
			ev: domain.IncidentEvent{Type: domain.EventIncidentResolved, Incident: domain.Incident{
				ProjectID: "p1", Title: "Checkout down", Status: domain.IncidentResolved, Impact: domain.ImpactCritical},
				Update: &domain.IncidentUpdate{Body: "Recovered."}},
			subject: "[resolved] Checkout down",
			body:    "Incident resolved: Checkout down\n\nStatus: resolved\nImpact: critical\n\nRecovered.\n",
		},
		{
			name: "impact none keeps the stored word",
			ev: domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{
				ProjectID: "p1", Title: "Checkout unknown", Status: domain.IncidentInvestigating, Impact: domain.ImpactNone}},
			subject: "[investigating] Checkout unknown",
			body:    "Incident opened: Checkout unknown\n\nStatus: investigating\nImpact: none\n",
		},
	}
	for _, c := range cases {
		sender := &capturingSender{}
		n := New(fakeStore{emails: []string{"a@x"}}, sender)
		if err := n.Deliver(context.Background(), c.ev); err != nil {
			t.Fatalf("%s: deliver: %v", c.name, err)
		}
		if len(sender.sent) != 1 {
			t.Fatalf("%s: %d mails, want 1", c.name, len(sender.sent))
		}
		if got := sender.sent[0]; got.subject != c.subject || got.body != c.body {
			t.Errorf("%s:\n subject %q\n    want %q\n body %q\n want %q", c.name, got.subject, c.subject, got.body, c.body)
		}
	}
}

// FR-042 I8, §4.5 — a rise below critical reaches webhooks but not subscribers' inboxes.
func TestARiseBelowCriticalIsNotMailed(t *testing.T) {
	for _, to := range []domain.IncidentImpact{domain.ImpactMinor, domain.ImpactMajor} {
		sender := &capturingSender{}
		n := New(fakeStore{emails: []string{"a@x", "b@x"}}, sender)
		ev := domain.IncidentEvent{Type: domain.EventIncidentUpdated,
			Incident:     domain.Incident{ProjectID: "p1", Title: "Checkout", Status: domain.IncidentInvestigating, Impact: to},
			Update:       &domain.IncidentUpdate{Body: domain.ImpactRaisedNote(domain.ImpactNone, to, domain.ServiceAlertDown)},
			ImpactRaised: &domain.ImpactRaise{From: domain.ImpactNone, To: to}}
		if err := n.Deliver(context.Background(), ev); err != nil {
			t.Fatalf("rise to %s: deliver: %v", to, err)
		}
		if len(sender.sent) != 0 {
			t.Errorf("rise to %s: %d mails sent, want 0", to, len(sender.sent))
		}
	}
}

// FR-042 I8, §4.5 — a rise to critical is mailed with the rise template.
func TestARiseToCriticalIsMailedWithTheRiseTemplate(t *testing.T) {
	sender := &capturingSender{}
	n := New(fakeStore{emails: []string{"a@x"}}, sender)
	ev := domain.IncidentEvent{Type: domain.EventIncidentUpdated,
		Incident: domain.Incident{ProjectID: "p1", Title: "Checkout — service down",
			Status: domain.IncidentIdentified, Impact: domain.ImpactCritical},
		Update:       &domain.IncidentUpdate{Body: "⬆ Impact raised: major → critical — the service is down."},
		ImpactRaised: &domain.ImpactRaise{From: domain.ImpactMajor, To: domain.ImpactCritical}}
	if err := n.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("%d mails, want 1", len(sender.sent))
	}
	const wantSubject = "[critical] Checkout — service down"
	const wantBody = "Incident impact raised to critical: Checkout — service down\n\nStatus: identified\n" +
		"Impact: critical (was major)\n\n⬆ Impact raised: major → critical — the service is down.\n"
	if got := sender.sent[0]; got.subject != wantSubject || got.body != wantBody {
		t.Errorf("subject %q\n    want %q\nbody %q\nwant %q", got.subject, wantSubject, got.body, wantBody)
	}
}
