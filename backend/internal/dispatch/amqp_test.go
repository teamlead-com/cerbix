package dispatch_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/dispatch"
	"git.example.com/monitoring/cerbix/internal/domain"
)

// TestAMQPRoundTrip requires a real RabbitMQ. It is opt-in via
// CERBIX_TEST_RABBITMQ_URL and skipped otherwise, so default `go test ./...` and
// CI stay hermetic (the in-process dispatcher covers the seam there).
func TestAMQPRoundTrip(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP dispatcher test")
	}
	d, err := dispatch.NewAMQP(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

	jobs := d.Jobs()
	want := "mon-roundtrip"
	if err := d.PublishJob(ctx, dispatch.CheckJob{Monitor: domain.Monitor{ID: want, Name: "x"}}); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	if got := awaitJob(t, jobs, want); got != want {
		t.Fatalf("job round-trip = %q, want %q", got, want)
	}

	results := d.Results()
	if err := d.PublishResult(ctx, domain.Heartbeat{MonitorID: want, Up: true, Code: 200}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if got := awaitResult(t, results, want); !got {
		t.Fatal("result round-trip: heartbeat not up")
	}
}

func awaitJob(t *testing.T, jobs <-chan dispatch.CheckJob, want string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case j := <-jobs:
			if j.Monitor.ID == want {
				return j.Monitor.ID
			}
		case <-deadline:
			t.Fatal("timed out waiting for job")
			return ""
		}
	}
}

func awaitResult(t *testing.T, results <-chan domain.Heartbeat, want string) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case hb := <-results:
			if hb.MonitorID == want {
				return hb.Up
			}
		case <-deadline:
			t.Fatal("timed out waiting for result")
			return false
		}
	}
}
