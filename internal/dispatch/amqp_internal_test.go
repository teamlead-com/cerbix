package dispatch

import "testing"

// TestBrokerStateHook covers the cerbix_broker_up wiring at the unit level (the live-broker
// round-trip is opt-in via CERBIX_TEST_RABBITMQ_URL). WithBrokerState reports up=true
// immediately (the dispatcher only exists after a successful dial), then supervise() calls
// setBrokerState(false) on loss and redial() calls setBrokerState(true) on recovery — here we
// drive those transitions directly and assert the callback sequence, plus nil-safety.
func TestBrokerStateHook(t *testing.T) {
	var got []bool
	d := &AMQP{}
	d.WithBrokerState(func(up bool) { got = append(got, up) }) // immediate up
	d.setBrokerState(false)                                    // broker_lost
	d.setBrokerState(true)                                     // broker_reconnected

	want := []bool{true, false, true}
	if len(got) != len(want) {
		t.Fatalf("callback sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("callback[%d] = %v, want %v (full %v)", i, got[i], want[i], got)
		}
	}

	// A dispatcher without a wired hook must not panic on a state change.
	(&AMQP{}).setBrokerState(false)
}
