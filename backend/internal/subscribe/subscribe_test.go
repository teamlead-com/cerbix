package subscribe

import (
	"context"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
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
