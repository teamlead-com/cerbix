package dispatch

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// syncBuffer guards the log buffer against concurrent supervisor writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAMQPSupervisorRecovers severs the live connection and asserts the
// supervisor redials, consumers resubscribe, and publishing works again —
// with exactly one broker_lost / broker_reconnected pair in the log.
// Opt-in via CERBIX_TEST_RABBITMQ_URL (a real broker is required).
func TestAMQPSupervisorRecovers(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP supervisor test")
	}
	var logBuf syncBuffer
	d, err := NewAMQP(url, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

	results := d.Results()
	mustRoundTrip := func(id string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if err := d.PublishResult(ctx, domain.Heartbeat{MonitorID: id, Up: true}); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("publish %s: %v", id, err)
			}
			time.Sleep(300 * time.Millisecond)
		}
		for {
			select {
			case hb := <-results:
				if hb.MonitorID == id {
					return
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("no result %s within 15s", id)
			}
		}
	}
	mustRoundTrip("before-outage")

	// Sever the connection out from under the dispatcher — from the client's
	// perspective this is indistinguishable from the broker dropping it.
	conn, _ := d.current()
	_ = conn.Close()

	// The supervisor must redial and consumers must resubscribe.
	mustRoundTrip("after-recovery")

	logs := logBuf.String()
	if strings.Count(logs, "broker_lost") != 1 {
		t.Fatalf("want exactly one broker_lost, logs:\n%s", logs)
	}
	if strings.Count(logs, "broker_reconnected") != 1 {
		t.Fatalf("want exactly one broker_reconnected, logs:\n%s", logs)
	}
}

// TestAMQPConsumerSurvivesChannelDeath kills the results queue out from under a
// live connection (a channel-level event, not connection loss) and asserts the
// consumer resubscribes and delivery resumes — the residual silent-death gap.
// Opt-in via CERBIX_TEST_RABBITMQ_URL.
func TestAMQPConsumerSurvivesChannelDeath(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP channel-death test")
	}
	var logBuf syncBuffer
	d, err := NewAMQP(url, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	results := d.Results()

	roundTrip := func(id string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			if err := d.PublishResult(ctx, domain.Heartbeat{MonitorID: id, Up: true}); err != nil && time.Now().After(deadline) {
				t.Fatalf("publish %s: %v", id, err)
			}
			select {
			case hb := <-results:
				if hb.MonitorID == id {
					return
				}
			case <-time.After(1 * time.Second):
			}
			if time.Now().After(deadline) {
				t.Fatalf("no result %s within 20s", id)
			}
		}
	}
	roundTrip("before-channel-death")

	// Delete the results queue on a SEPARATE channel: the consumer's delivery
	// channel closes (basic.cancel) but the TCP connection stays up, so the
	// connection supervisor never fires. Recovery must come from the
	// channelRetryBackoff leg re-declaring and resubscribing.
	conn, _ := d.current()
	admin, err := conn.Channel()
	if err != nil {
		t.Fatalf("admin channel: %v", err)
	}
	if _, err := admin.QueueDelete(resultsQueue, false, false, false); err != nil {
		t.Fatalf("delete queue: %v", err)
	}
	_ = admin.Close()

	// Within a few backoff cycles the consumer re-declares and delivery resumes,
	// and no connection-level broker_lost was logged.
	roundTrip("after-channel-death")
	if strings.Contains(logBuf.String(), "broker_lost") {
		t.Fatalf("channel death must not report broker_lost:\n%s", logBuf.String())
	}
}
