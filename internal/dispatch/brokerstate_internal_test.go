package dispatch

// Review of iter-0212, P2-2: the broker's state lives in the dispatcher, so the readiness hook is
// told the CURRENT state when it is wired, not an unconditional "up". cli wires
// WithBrokerState(registry.SetBrokerUp) after NewAMQP has already started the supervisor; a loss
// in that window used to be overwritten by the wiring's "up", and /readyz said ready with the
// broker gone (sec-hardening-2 S12).

import (
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

func TestALossBeforeTheHookIsWiredReachesReadiness(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "worker")
	reg.SetReady(true, "")
	d := &AMQP{}
	d.setBrokerState(false) // supervise(): broker_lost, before cli wires the hook
	d.WithBrokerState(reg.SetBrokerUp)
	if reg.Ready() || reg.LastError() != metrics.NotReadyBroker {
		t.Errorf("a loss before WithBrokerState: Ready() = %v, LastError() = %q — want not ready, %q", reg.Ready(), reg.LastError(), metrics.NotReadyBroker)
	}
	d.setBrokerState(true) // redial(): broker_reconnected
	if !reg.Ready() {
		t.Errorf("after reconnect: not ready (%q)", reg.LastError())
	}
}

// Wiring the hook while the supervisor reports changes: the last value the hook saw is the
// dispatcher's state.
func TestTheHookEndsOnTheDispatchersState(t *testing.T) {
	for range 200 {
		var mu sync.Mutex
		var last *bool
		d := &AMQP{}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			d.setBrokerState(true)
			d.setBrokerState(false)
		}()
		go func() {
			defer wg.Done()
			d.WithBrokerState(func(up bool) { mu.Lock(); last = &up; mu.Unlock() })
		}()
		wg.Wait()
		mu.Lock()
		got := last
		mu.Unlock()
		if got == nil {
			t.Fatal("the hook was never called")
		}
		if *got {
			t.Fatalf("the hook ended on up=%v; the dispatcher's state is down", *got)
		}
	}
}
