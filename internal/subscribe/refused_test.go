package subscribe

import (
	"context"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/mailer"
)

// builderSender refuses an address exactly as the real message builder does, and records the rest.
type builderSender struct{ sent []string }

func (b *builderSender) Send(to, subject, body string) error {
	if _, err := mailer.BuildMessage("status@example.com", []string{to}, subject, body); err != nil {
		return err
	}
	b.sent = append(b.sent, to)
	return nil
}
func (b *builderSender) BaseURL() string { return "https://status.example" }

// TestSubscriberMailSkipsARefusedAddress (sec-hardening-3 T-3, §4.2) is a regression guard for the
// behaviour the owner confirmed, which the code already has: a stored subscriber address the builder
// refuses is skipped, every other subscriber receives the mail exactly once, and the error returned
// (which the incident fan-out only logs) names the position, not the address.
func TestSubscriberMailSkipsARefusedAddress(t *testing.T) {
	sender := &builderSender{}
	n := New(fakeStore{emails: []string{"a@b.example", "a b@c", "c@d.example"}}, sender)
	ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ProjectID: "p1", Title: "down", Status: domain.IncidentInvestigating}}
	err := n.Deliver(context.Background(), ev)
	if strings.Join(sender.sent, ",") != "a@b.example,c@d.example" {
		t.Fatalf("sent to %v, want each valid subscriber exactly once", sender.sent)
	}
	if err == nil || !strings.Contains(err.Error(), "to address 1") || strings.Contains(err.Error(), "a b@c") {
		t.Fatalf("error = %v, want the position and not the address", err)
	}
}
