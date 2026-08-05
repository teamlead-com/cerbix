package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, m domain.Monitor) domain.Heartbeat {
	return domain.Heartbeat{MonitorID: m.ID, Up: true, Code: 200}
}

func TestPoolProcessesJobs(t *testing.T) {
	disp := dispatch.NewInProc(8)
	p := New(disp, fakeRunner{}, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if err := disp.PublishJob(ctx, dispatch.CheckJob{Monitor: domain.Monitor{ID: "m1", Type: domain.MonitorHTTP}}); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	select {
	case hb := <-disp.Results():
		if hb.MonitorID != "m1" || !hb.Up {
			t.Fatalf("result = %+v", hb)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not produce a result")
	}
}

func TestNewClampsSize(t *testing.T) {
	p := New(dispatch.NewInProc(1), fakeRunner{}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if p.size != 1 {
		t.Fatalf("size = %d, want clamped to 1", p.size)
	}
}
