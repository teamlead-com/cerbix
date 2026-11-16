package outbox

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/prober"
)

// channelStore is notify's store with a fixed channel set.
type channelStore struct{ channels []domain.NotificationChannel }

func (c channelStore) ListEnabledChannelsForMonitor(context.Context, string) ([]domain.NotificationChannel, error) {
	return c.channels, nil
}
func (c channelStore) ListEnabledChannelsByProject(context.Context, string) ([]domain.NotificationChannel, error) {
	return c.channels, nil
}
func (c channelStore) EnabledChannelsByIDs(context.Context, []string) ([]domain.NotificationChannel, error) {
	return c.channels, nil
}

// TestRedirectedAlertIsNotMarkedDelivered (sec-hardening-3 T-2, S17): a channel URL that answers 302
// to an endpoint answering 200 fails the event with the status in last_error. Before, the 302 became a
// bodiless GET that answered 200, and the outbox marked the alert delivered although nobody got it.
func TestRedirectedAlertIsNotMarkedDelivered(t *testing.T) {
	landing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer landing.Close()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", landing.URL+"/landing")
		w.WriteHeader(http.StatusFound)
	}))
	defer moved.Close()

	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusDown}},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
		},
	}
	channels := channelStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": moved.URL + "/hook"}},
	}}
	nd := notify.New(channels, prober.NewGuard(true, false).HTTPClient(0))
	New(fs, nil, nd, &fakeMetrics{}, slog.New(slog.NewTextHandler(io.Discard, nil))).drain(context.Background())

	if len(fs.delivered) != 0 {
		t.Fatalf("the redirected alert was marked delivered: %v", fs.delivered)
	}
	if len(fs.failed) != 1 || !strings.Contains(fs.failReasons[0], "non-2xx status 302") {
		t.Fatalf("failed=%v reasons=%q, want e1 failed with non-2xx status 302", fs.failed, fs.failReasons)
	}
}
