package cli

// Review of iter-0212, P3-3: pingDatabase reports database reachability into the registry and
// nothing else — a successful ping must not erase another not-ready (a lost broker) nor undo a
// shutdown. It takes a narrow pinger, so the loop is driven here without a database.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

type fakePinger struct{ fail atomic.Bool }

func (p *fakePinger) Ping(context.Context) error {
	if p.fail.Load() {
		return errors.New("dial tcp 10.0.0.9:5432: connection refused")
	}
	return nil
}

func TestASuccessfulPingErasesNoOtherNotReady(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "api")
	reg.SetReady(true, "")
	p := &fakePinger{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pingDatabase(ctx, p, 5*time.Millisecond, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// eventually waits until the registry reports want, or fails.
	eventually := func(want, what string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if reg.LastError() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s: LastError() = %q, want %q", what, reg.LastError(), want)
	}
	// holds checks that the registry keeps reporting want across many successful pings.
	holds := func(want, what string) {
		t.Helper()
		for range 20 {
			time.Sleep(5 * time.Millisecond)
			if got := reg.LastError(); got != want {
				t.Fatalf("%s: a successful ping changed LastError() to %q, want %q", what, got, want)
			}
		}
	}

	eventually("", "pings succeed")
	p.fail.Store(true)
	eventually(metrics.NotReadyDatabase, "pings fail")
	p.fail.Store(false)
	eventually("", "pings recover")

	reg.SetBrokerUp(false)
	holds(metrics.NotReadyBroker, "broker lost")
	reg.SetBrokerUp(true)
	eventually("", "broker back")

	reg.SetReady(false, "shutting down")
	holds(metrics.NotReadyShuttingDown, "shutting down")
}
