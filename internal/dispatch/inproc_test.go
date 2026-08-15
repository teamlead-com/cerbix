package dispatch

import (
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestInProcJobsAndResults(t *testing.T) {
	d := NewInProc(4)
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close dispatcher: %v", err)
		}
	})
	ctx := context.Background()

	if err := d.PublishJob(ctx, CheckJob{Monitor: domain.Monitor{ID: "m1"}}); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	delivered := <-d.Jobs()
	if delivered.Job.Monitor.ID != "m1" {
		t.Fatalf("job monitor = %q", delivered.Job.Monitor.ID)
	}

	if err := d.PublishResult(ctx, domain.Heartbeat{MonitorID: "m1", Up: true}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	hb := <-d.Results()
	if hb.MonitorID != "m1" || !hb.Up {
		t.Fatalf("result = %+v", hb)
	}
}

func TestInProcRespectsContextCancellation(t *testing.T) {
	d := NewInProc(1)
	// Fill the single-slot buffer.
	if err := d.PublishJob(context.Background(), CheckJob{Monitor: domain.Monitor{ID: "a"}}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.PublishJob(ctx, CheckJob{Monitor: domain.Monitor{ID: "b"}}); err == nil {
		t.Fatal("publish on full buffer with cancelled ctx should error")
	}
}
