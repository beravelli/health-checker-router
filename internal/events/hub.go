// Package events is a small in-process pub/sub used to push live health
// updates to connected dashboards over SSE.
//
// This is a live dashboard, so clients subscribe once rather than polling.
// Publishing never blocks: a subscriber that cannot keep up drops events
// rather than stalling the prober.
package events

import (
	"encoding/json"
	"sync"
	"time"
)

// Type names the kind of event carried.
type Type string

const (
	TypeHealth   Type = "health"   // region/service health recomputed
	TypeWeights  Type = "weights"  // LB weights changed
	TypeFailOpen Type = "failopen" // fail-open window opened or closed
	TypeIncident Type = "incident" // new incident recorded
	TypeRollout  Type = "rollout"  // rollout state changed
)

// Event is one published message.
type Event struct {
	Type Type      `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

// Encode renders the event as a JSON payload for an SSE data: line.
func (e Event) Encode() ([]byte, error) { return json.Marshal(e) }

// Hub fans events out to subscribers.
type Hub struct {
	mu     sync.RWMutex
	nextID int
	subs   map[int]chan Event
	// buffer is the per-subscriber queue depth before events are dropped.
	buffer int
}

// NewHub returns a Hub with a per-subscriber buffer of 16 events.
func NewHub() *Hub {
	return &Hub{subs: map[int]chan Event{}, buffer: 16}
}

// Subscribe registers a listener. The returned cancel function must be called
// to release the subscription.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	id := h.nextID
	h.nextID++
	ch := make(chan Event, h.buffer)
	h.subs[id] = ch

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

// Publish delivers an event to every subscriber, skipping any whose buffer is
// full. Dropping is deliberate: a slow dashboard must not apply backpressure to
// the probe loop.
func (h *Hub) Publish(t Type, data any) {
	ev := Event{Type: t, At: time.Now().UTC(), Data: data}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribers reports the current subscriber count, for metrics.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
