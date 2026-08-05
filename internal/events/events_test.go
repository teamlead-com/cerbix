package events

import (
	"testing"
)

func TestBrokerPublishSubscribe(t *testing.T) {
	b := NewBroker()
	ch1, unsub1 := b.Subscribe()
	ch2, unsub2 := b.Subscribe()
	defer unsub1()

	b.Publish(Event{Type: "status", MonitorID: "m1", ProjectID: "p1", Status: "down"})
	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.MonitorID != "m1" {
				t.Fatalf("got %+v", ev)
			}
		default:
			t.Fatal("subscriber did not receive event")
		}
	}

	// After unsubscribe, the channel is closed and receives no more events.
	unsub2()
	b.Publish(Event{MonitorID: "m2"})
	if _, ok := <-ch2; ok {
		t.Fatal("unsubscribed channel should be closed")
	}
	// unsub is idempotent.
	unsub2()
}

func TestBrokerDropsOnFullBuffer(t *testing.T) {
	b := NewBroker()
	_, unsub := b.Subscribe() // never drained
	defer unsub()
	// Publishing far more than the buffer must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(Event{MonitorID: "m"})
		}
		close(done)
	}()
	<-done
}
