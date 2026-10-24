package httpapi

import (
	"sync"
	"sync/atomic"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// Hub fans events out to SSE subscribers. Every subscriber has a bounded
// buffer; a subscriber whose buffer is full is dropped (its channel is closed)
// so a slow or stuck client can never block Publish or grow memory.
type Hub struct {
	mu        sync.Mutex
	subs      map[*Subscription]struct{}
	buf       int
	maxSubs   int
	observers []func(domain.Event)
	dropped   atomic.Uint64
}

// Subscription is one SSE client. Read events from C; C is closed when the
// subscription is dropped (slow client) or closed.
type Subscription struct {
	C   <-chan domain.Event
	ch  chan domain.Event
	hub *Hub
}

// NewHub returns a hub with 32-event buffers and at most 64 subscribers.
func NewHub() *Hub { return NewHubSize(32, 64) }

// NewHubSize returns a hub with the given per-client buffer and subscriber cap.
func NewHubSize(buffer, maxSubscribers int) *Hub {
	if buffer < 1 {
		buffer = 1
	}
	if maxSubscribers < 1 {
		maxSubscribers = 1
	}
	return &Hub{subs: map[*Subscription]struct{}{}, buf: buffer, maxSubs: maxSubscribers}
}

// Subscribe registers a client. It returns nil if the subscriber cap is reached.
func (h *Hub) Subscribe() *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.maxSubs {
		return nil
	}
	ch := make(chan domain.Event, h.buf)
	s := &Subscription{C: ch, ch: ch, hub: h}
	h.subs[s] = struct{}{}
	return s
}

// Close unsubscribes; safe to call more than once and after being dropped.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
}

// Publish delivers ev to every subscriber without ever blocking. Subscribers
// with a full buffer are dropped. Observers run synchronously after delivery.
func (h *Hub) Publish(ev domain.Event) {
	h.mu.Lock()
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			delete(h.subs, s)
			close(s.ch)
			h.dropped.Add(1)
		}
	}
	obs := h.observers
	h.mu.Unlock()
	for _, o := range obs {
		o(ev)
	}
}

// Observe registers a callback invoked for every published event (used for
// metrics). Callbacks must be fast and non-blocking.
func (h *Hub) Observe(fn func(domain.Event)) {
	h.mu.Lock()
	h.observers = append(h.observers[:len(h.observers):len(h.observers)], fn)
	h.mu.Unlock()
}

// Len returns the number of live subscribers.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Dropped returns how many slow subscribers have been dropped so far.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }

// PrintEvent is the payload of a "print" event.
type PrintEvent struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
