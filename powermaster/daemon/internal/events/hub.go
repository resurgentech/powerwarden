// Package events is powermaster's in-process event hub: every host and
// command transition is published here with a monotonically increasing seq and
// kept in a bounded ring so that a reconnecting subscriber can resume.
package events

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// Event types.
const (
	TypeHost    = "host"
	TypeCommand = "command"
	TypeResync  = "resync"
)

// DefaultSubscriberBuffer is the per-subscriber channel depth. A subscriber
// that falls this far behind is dropped and must resume with Last-Event-ID.
const DefaultSubscriberBuffer = 256

// Event is one published transition. Data is the JSON object sent as the SSE
// `data:` line, frozen at publish time; it always carries "seq" and "type"
// (type equals the SSE `event:` name).
type Event struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Hub fans events out to subscribers and keeps the last ringSize events.
type Hub struct {
	mu   sync.Mutex
	seq  uint64
	ring []Event // circular, len == cap once full
	size int
	next int // write position once full
	subs map[*Subscription]struct{}
}

// NewHub returns a hub that keeps ringSize events for resume.
func NewHub(ringSize int) *Hub {
	if ringSize < 1 {
		ringSize = 1
	}
	return &Hub{size: ringSize, ring: make([]Event, 0, ringSize), subs: map[*Subscription]struct{}{}}
}

// Publish marshals data, assigns the next seq, stores it in the ring and
// delivers it to every subscriber without blocking.
func (h *Hub) Publish(typ string, data any) Event {
	fields := map[string]json.RawMessage{}
	raw, err := json.Marshal(data)
	if err == nil {
		if uerr := json.Unmarshal(raw, &fields); uerr != nil {
			// Not an object: carry it under "data".
			fields = map[string]json.RawMessage{"data": raw}
		}
	} else {
		msg, _ := json.Marshal(err.Error())
		fields["marshal_error"] = msg
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	fields["seq"], _ = json.Marshal(h.seq)
	fields["type"], _ = json.Marshal(typ)
	out, _ := json.Marshal(fields)
	ev := Event{Seq: h.seq, Type: typ, Data: out}
	if len(h.ring) < h.size {
		h.ring = append(h.ring, ev)
	} else {
		h.ring[h.next] = ev
		h.next = (h.next + 1) % h.size
	}
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			s.lagged.Store(true)
			h.dropLocked(s)
		}
	}
	return ev
}

// ResyncEvent builds the event sent instead of a backlog when a client's
// Last-Event-ID is no longer in the ring. It is not stored or broadcast.
func (h *Hub) ResyncEvent() Event {
	seq := h.Seq()
	data, _ := json.Marshal(map[string]any{"seq": seq, "type": TypeResync})
	return Event{Seq: seq, Type: TypeResync, Data: data}
}

// Seq returns the last assigned seq.
func (h *Hub) Seq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Subscribers returns the number of live subscribers.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Subscription is one subscriber. Deliver Backlog first (or a resync event if
// Resync is set), then read C until it closes.
type Subscription struct {
	Backlog []Event
	Resync  bool
	C       <-chan Event

	hub    *Hub
	ch     chan Event
	once   sync.Once
	lagged atomic.Bool
}

// Subscribe registers a subscriber. With hasLast, events after lastSeq that
// are still in the ring are returned as Backlog; if some are gone (or lastSeq
// is from a different process), Resync is set and the client must re-read
// GET /v1/hosts.
func (h *Hub) Subscribe(lastSeq uint64, hasLast bool) *Subscription {
	return h.subscribe(lastSeq, hasLast, DefaultSubscriberBuffer)
}

func (h *Hub) subscribe(lastSeq uint64, hasLast bool, buf int) *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan Event, buf)
	s := &Subscription{hub: h, ch: ch, C: ch}
	if hasLast && lastSeq != h.seq {
		ordered := h.orderedLocked()
		switch {
		case lastSeq > h.seq:
			s.Resync = true
		case len(ordered) == 0 || lastSeq+1 < ordered[0].Seq:
			s.Resync = true
		default:
			for _, ev := range ordered {
				if ev.Seq > lastSeq {
					s.Backlog = append(s.Backlog, ev)
				}
			}
		}
	}
	h.subs[s] = struct{}{}
	return s
}

func (h *Hub) orderedLocked() []Event {
	if len(h.ring) < h.size {
		return append([]Event(nil), h.ring...)
	}
	out := make([]Event, 0, h.size)
	out = append(out, h.ring[h.next:]...)
	return append(out, h.ring[:h.next]...)
}

func (h *Hub) dropLocked(s *Subscription) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	s.once.Do(func() { close(s.ch) })
}

// Close unregisters the subscriber and closes C.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.dropLocked(s)
}

// Lagged reports whether the hub dropped this subscriber for falling behind.
func (s *Subscription) Lagged() bool { return s.lagged.Load() }
