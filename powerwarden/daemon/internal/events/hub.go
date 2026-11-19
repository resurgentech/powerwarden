// Package events is powerwarden's server-sent event hub: a ring of recent
// events with monotonically increasing seq, Last-Event-ID resume, a resync
// event when the requested id fell out of the ring, and comment heartbeats.
// /v1/events (bearer) and /ui/events (basic auth) serve the same hub.
package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	TypeDemand   = "demand"
	TypeWorker   = "worker"
	TypeClaim    = "claim"
	TypeHost     = "host"
	TypeDecision = "decision"
	TypeResync   = "resync"
)

// Event is one encoded event.
type Event struct {
	Seq  int64
	Type string
	Data []byte
}

// Hub fans events out to subscribers.
type Hub struct {
	mu        sync.Mutex
	seq       int64
	ring      []Event
	size      int
	subs      map[*sub]struct{}
	heartbeat time.Duration
}

type sub struct {
	ch     chan Event
	closed bool
}

// New returns a hub keeping ringSize events.
func New(ringSize int, heartbeat time.Duration) *Hub {
	return &Hub{size: ringSize, subs: map[*sub]struct{}{}, heartbeat: heartbeat}
}

// Publish assigns the next seq, builds the payload with it, and broadcasts.
// build must return a value whose JSON carries "seq" and "type".
func (h *Hub) Publish(typ string, build func(seq int64) any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	data, err := json.Marshal(build(h.seq))
	if err != nil {
		panic(fmt.Sprintf("events: marshal %s: %v", typ, err))
	}
	ev := Event{Seq: h.seq, Type: typ, Data: data}
	h.ring = append(h.ring, ev)
	if len(h.ring) > h.size {
		h.ring = h.ring[len(h.ring)-h.size:]
	}
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default: // slow subscriber: drop it; it reconnects with Last-Event-ID
			h.dropLocked(s)
		}
	}
}

// Seq returns the last assigned seq.
func (h *Hub) Seq() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Subscribers returns the number of connected subscribers.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// subscribe returns the backlog after lastID (or a resync event) and a live channel.
func (h *Hub) subscribe(lastID int64, hasLast bool) ([]Event, *sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var backlog []Event
	if hasLast && lastID < h.seq {
		oldest := h.seq + 1
		if len(h.ring) > 0 {
			oldest = h.ring[0].Seq
		}
		if lastID+1 < oldest {
			data, _ := json.Marshal(map[string]any{"seq": h.seq, "type": TypeResync})
			backlog = []Event{{Seq: h.seq, Type: TypeResync, Data: data}}
		} else {
			for _, ev := range h.ring {
				if ev.Seq > lastID {
					backlog = append(backlog, ev)
				}
			}
		}
	}
	s := &sub{ch: make(chan Event, 256)}
	h.subs[s] = struct{}{}
	return backlog, s
}

func (h *Hub) unsubscribe(s *sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(s)
}

func (h *Hub) dropLocked(s *sub) {
	if !s.closed {
		s.closed = true
		delete(h.subs, s)
		close(s.ch)
	}
}

// ServeHTTP streams events. Authentication is the caller's job.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var lastID int64
	hdr := r.Header.Get("Last-Event-ID")
	hasLast := hdr != ""
	if hasLast {
		v, err := strconv.ParseInt(hdr, 10, 64)
		if err != nil || v < 0 {
			http.Error(w, `{"error":"bad_request","message":"Last-Event-ID must be an integer"}`, http.StatusBadRequest)
			return
		}
		lastID = v
	}
	backlog, s := h.subscribe(lastID, hasLast)
	defer h.unsubscribe(s)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(ev Event) error {
		_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, ev.Data)
		return err
	}
	for _, ev := range backlog {
		if write(ev) != nil {
			return
		}
	}
	fl.Flush()
	tick := time.NewTicker(h.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-s.ch:
			if !ok {
				return
			}
			if write(ev) != nil {
				return
			}
			fl.Flush()
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
