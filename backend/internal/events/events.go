// Package events is an in-process pub/sub hub for realtime monitor status
// changes streamed to the browser over SSE. Single-process only: for multi-replica
// realtime, front this with Redis pub/sub between api replicas.
package events

import (
	"sync"
	"time"
)

// Event is a live monitor status change.
type Event struct {
	Type      string    `json:"type"` // "status"
	MonitorID string    `json:"monitor_id"`
	ProjectID string    `json:"project_id"`
	Status    string    `json:"status"` // up | down | pending
	LatencyMS int64     `json:"latency_ms"`
	TS        time.Time `json:"ts"`
}

// Broker fans published events out to all current subscribers.
type Broker struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// NewBroker builds an empty broker.
func NewBroker() *Broker { return &Broker{subs: make(map[chan Event]struct{})} }

// Subscribe returns a buffered event channel and an unsubscribe func. The channel
// is closed by unsubscribe; always call it (e.g. defer) to avoid leaks.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			close(ch)
			b.mu.Unlock()
		})
	}
}

// Publish delivers to every subscriber, dropping the event for any whose buffer is
// full rather than blocking the caller (ingest must never stall on a slow client).
func (b *Broker) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
