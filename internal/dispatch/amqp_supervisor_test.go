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
