package domain

import (
	"context"
	"sync"
)

// PhoneChannelsSeen collects the sms and voice channels a text delivery read and left out (FR-041a
// D16, P14). The outbox worker puts one in the context of an event's text half; the dispatcher's
// channel reads record into it every phone channel they drop. The phone half then knows, from the
// read the text half made anyway, whether the event has a phone recipient at all — so an event
// without one makes no further call and gains no new way to fail.
//
// It also records whether such a read COMPLETED. A read that failed — one channel whose config cannot
// be opened fails the whole list — has told the phone half nothing, and the worker then asks for the
// phone channels alone (ReadCompleted).
//
// The completion is also a signal (ReadDone): the phone half starts as soon as the text half's read
// has answered, and runs beside the text half's sends rather than after them (FR-041a review P2-1).
// The one read serves both halves. A reader therefore records every phone channel it left out BEFORE
// it records the read completed.
type PhoneChannelsSeen struct {
	mu   sync.Mutex
	ids  []string
	read bool
	done chan struct{} // closed when the read completed
}

type phoneChannelsSeenKey struct{}

// WithPhoneChannelsSeen returns ctx carrying a new, empty collector, and the collector.
func WithPhoneChannelsSeen(ctx context.Context) (context.Context, *PhoneChannelsSeen) {
	s := &PhoneChannelsSeen{done: make(chan struct{})}
	return context.WithValue(ctx, phoneChannelsSeenKey{}, s), s
}

// RecordPhoneChannelSeen records a phone channel a read left out, once, in the collector ctx carries;
// a context without one records nothing.
func RecordPhoneChannelSeen(ctx context.Context, channelID string) {
	s, _ := ctx.Value(phoneChannelsSeenKey{}).(*PhoneChannelsSeen)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.ids {
		if id == channelID {
			return
		}
	}
	s.ids = append(s.ids, channelID)
}

// RecordChannelReadCompleted records, in the collector ctx carries, that a channel read answered —
// after every phone channel it left out has been recorded — and signals ReadDone; a context without
// one records nothing.
func RecordChannelReadCompleted(ctx context.Context) {
	s, _ := ctx.Value(phoneChannelsSeenKey{}).(*PhoneChannelsSeen)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.read {
		s.read = true
		close(s.done)
	}
}

// ReadDone is closed once a channel read has completed (RecordChannelReadCompleted): IDs then name
// every phone channel the event has.
func (s *PhoneChannelsSeen) ReadDone() <-chan struct{} { return s.done }

// ReadCompleted reports whether a channel read answered: only then do IDs say which phone channels
// the event has.
func (s *PhoneChannelsSeen) ReadCompleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read
}

// IDs returns the phone channels recorded, in the order the reads returned them.
func (s *PhoneChannelsSeen) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}
