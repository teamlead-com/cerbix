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

	amqp "github.com/rabbitmq/amqp091-go"

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

// TestAMQPPublisherReopensChannel proves the publisher self-heals from a
// channel-level death while the connection stays up (the supervisor only watches
// the connection). Opt-in via CERBIX_TEST_RABBITMQ_URL.
func TestAMQPPublisherReopensChannel(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the publisher channel-reopen test")
	}
	var logBuf syncBuffer
	d, err := NewAMQP(url, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close dispatcher: %v", err)
		}
	})

	if err := d.PublishResult(context.Background(), domain.Heartbeat{MonitorID: "m1", Up: true}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Kill the publish channel out from under the publisher (connection stays up, so
	// the connection supervisor never fires).
	d.pubMu.Lock()
	ch := d.pubCh
	d.pubMu.Unlock()
	_ = ch.Close()

	// The next publish must transparently reopen the channel and succeed.
	if err := d.PublishResult(context.Background(), domain.Heartbeat{MonitorID: "m2", Up: true}); err != nil {
		t.Fatalf("publish after channel death should self-heal, got: %v", err)
	}
	if !strings.Contains(logBuf.String(), "publish_channel_reopened") {
		t.Fatalf("expected publish_channel_reopened log, got: %s", logBuf.String())
	}
}

// TestAMQPDeadLettersPoison proves an unparseable result body is forwarded to the
// durable dead-letter queue rather than silently dropped on Nack. Opt-in via
// CERBIX_TEST_RABBITMQ_URL.
func TestAMQPDeadLettersPoison(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the dead-letter test")
	}
	var logBuf syncBuffer
	d, err := NewAMQP(url, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close dispatcher: %v", err)
		}
	})

	// Drain the dead queue first so we assert on THIS run's poison message.
	conn, _ := d.current()
	pch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	t.Cleanup(func() {
		if err := pch.Close(); err != nil {
			t.Errorf("close poison channel: %v", err)
		}
	})
	if _, err := pch.QueuePurge(deadQueue, false); err != nil {
		t.Fatalf("purge dead: %v", err)
	}

	_ = d.Results() // start the results consumer (it dead-letters poison)

	poison := []byte("this-is-not-json")
	if err := pch.PublishWithContext(context.Background(), "", resultsQueue, false, false,
		amqp.Publishing{ContentType: "application/json", Body: poison}); err != nil {
		t.Fatalf("publish poison: %v", err)
	}

	for i := 0; i < 60; i++ {
		msg, ok, err := pch.Get(deadQueue, true)
		if err != nil {
			t.Fatalf("get dead: %v", err)
		}
		if ok {
			if string(msg.Body) != string(poison) {
				t.Fatalf("dead-letter body = %q, want %q", msg.Body, poison)
			}
			if src, _ := msg.Headers["x-cerbix-source"].(string); src != "results" {
				t.Fatalf("dead-letter source = %q, want results", src)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("poison message was not dead-lettered; logs: %s", logBuf.String())
}
