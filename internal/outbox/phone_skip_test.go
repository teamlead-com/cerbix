package outbox

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a D16: a monitor whose linked channels include a phone channel. Before the text paths skipped
// sms and voice, the phone channel failed the whole event ("unknown channel type") and the outbox
// re-delivered the Slack alert on every retry until the event dead-lettered. Now the text channel is
// delivered once, the phone channels are DeliverPhone's (learnt from the text half's own read), and
// the event is marked delivered.
func TestPhoneChannelDoesNotFailTheTextDelivery(t *testing.T) {
	var posts atomic.Int32
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer slack.Close()

	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusDown}},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
		},
	}
	channels := channelStore{channels: []domain.NotificationChannel{
		{ID: "sms1", Type: domain.ChannelSMS, Enabled: true, Recipient: domain.PhoneRecipientNumber},
		{ID: "nc1", Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": slack.URL}},
		{ID: "voice1", Type: domain.ChannelVoice, Enabled: true, Recipient: domain.PhoneRecipientUser},
	}}
	ps := newMemPhoneStore()
	ps.targets["sms1"] = domain.PhoneTarget{ChannelID: "sms1", Kind: domain.ChannelSMS,
		Recipient: domain.PhoneRecipientNumber, Number: "+905551236869"}
	ps.targets["voice1"] = domain.PhoneTarget{ChannelID: "voice1", Kind: domain.ChannelVoice,
		Recipient: domain.PhoneRecipientUser, UserID: "u1", Number: "+447700900199"}
	provider := telephony.NewFakeProvider()
	tel := &rigTelephony{provider: provider}
	tel.ts = domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	New(fs, nil, notify.New(channels, nil).WithPhone(ps, tel, nil, logger), &fakeMetrics{}, logger).
		drain(context.Background())

	if len(fs.failed) != 0 {
		t.Fatalf("the event failed: %q", fs.failReasons)
	}
	if len(fs.delivered) != 1 || posts.Load() != 1 {
		t.Fatalf("delivered=%v slack posts=%d; want the event delivered and one Slack post", fs.delivered, posts.Load())
	}
	if n := len(provider.Calls()); n != 2 {
		t.Fatalf("provider calls %d; want the sms and the voice channel phoned once each", n)
	}
}
