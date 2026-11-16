package outbox

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/prober"
)

// TestFailedDeliveryStoresAndLogsNoURL (sec-hardening-3 T-4, S21): what the outbox stores in
// last_error and writes to its log for a failed delivery holds no part of the channel URL, on a retry
// and on the attempt that dead-letters. The dead-letter API serves last_error as stored, so this
// covers it too. Before, both quoted the full URL.
func TestFailedDeliveryStoresAndLogsNoURL(t *testing.T) {
	const secret = "PATHSECRET9f3"
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()

	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusDown}},
		pending: []domain.OutboxEvent{
			{ID: "retry", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
			{ID: "dead", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: maxAttempts},
		},
	}
	channels := channelStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": "http://" + addr + "/hook/" + secret + "?token=" + secret}},
	}}
	var logs bytes.Buffer
	nd := notify.New(channels, prober.NewGuard(true, false).HTTPClient(0))
	New(fs, nil, nd, &fakeMetrics{}, slog.New(slog.NewTextHandler(&logs, nil))).drain(context.Background())

	if len(fs.failed) != 2 {
		t.Fatalf("failed = %v, want both events failed", fs.failed)
	}
	for i, reason := range fs.failReasons {
		if strings.Contains(reason, secret) {
			t.Errorf("last_error of %s carries the channel URL: %s", fs.failed[i], reason)
		}
		if !strings.Contains(reason, "channel nc1") || !strings.Contains(reason, "post failed: connection refused") {
			t.Errorf("last_error of %s = %q, want the channel id and the reason", fs.failed[i], reason)
		}
	}
	if !strings.Contains(logs.String(), "outbox_delivery_retry") || !strings.Contains(logs.String(), "outbox_dead") {
		t.Fatalf("the log has no retry and dead-letter lines: %s", logs.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the log carries the channel URL: %s", logs.String())
	}
}
