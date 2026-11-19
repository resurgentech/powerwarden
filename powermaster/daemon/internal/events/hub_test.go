package events

import (
	"encoding/json"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("no event within 1s")
	}
	return Event{}
}

func TestPublishAssignsIncreasingSeq(t *testing.T) {
	h := NewHub(10)
	a := h.Publish(TypeHost, map[string]string{"hostname": "a"})
	b := h.Publish(TypeCommand, map[string]int{"id": 1})
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("seq = %d, %d; want 1, 2", a.Seq, b.Seq)
	}
	var got map[string]any
	if err := json.Unmarshal(a.Data, &got); err != nil || got["hostname"] != "a" || got["seq"] != float64(1) || got["type"] != TypeHost {
		t.Fatalf("data = %s (%v)", a.Data, err)
	}
	var cmd map[string]any
	_ = json.Unmarshal(b.Data, &cmd)
	if cmd["type"] != TypeCommand || cmd["seq"] != float64(2) {
		t.Fatalf("command data = %s", b.Data)
	}
	r := h.ResyncEvent()
	if r.Type != TypeResync || string(r.Data) != `{"seq":2,"type":"resync"}` {
		t.Fatalf("resync = %+v %s", r, r.Data)
	}
}

func TestSubscribeLiveDelivery(t *testing.T) {
	h := NewHub(10)
	sub := h.Subscribe(0, false)
	defer sub.Close()
	if sub.Resync || len(sub.Backlog) != 0 {
		t.Fatalf("fresh subscribe: resync=%v backlog=%d", sub.Resync, len(sub.Backlog))
	}
	if h.Subscribers() != 1 {
		t.Fatalf("subscribers = %d", h.Subscribers())
	}
	h.Publish(TypeHost, "x")
	if ev := recv(t, sub.C); ev.Seq != 1 || ev.Type != TypeHost {
		t.Fatalf("event = %+v", ev)
	}
	sub.Close()
	if h.Subscribers() != 0 {
		t.Fatalf("subscribers after close = %d", h.Subscribers())
	}
}

func TestResumeFromRing(t *testing.T) {
	h := NewHub(5)
	for i := 0; i < 4; i++ {
		h.Publish(TypeHost, i)
	}
	sub := h.Subscribe(2, true)
	defer sub.Close()
	if sub.Resync {
		t.Fatal("unexpected resync")
	}
	if len(sub.Backlog) != 2 || sub.Backlog[0].Seq != 3 || sub.Backlog[1].Seq != 4 {
		t.Fatalf("backlog = %+v", sub.Backlog)
	}
	// Up to date: no backlog, no resync.
	sub2 := h.Subscribe(4, true)
	defer sub2.Close()
	if sub2.Resync || len(sub2.Backlog) != 0 {
		t.Fatalf("up to date: resync=%v backlog=%d", sub2.Resync, len(sub2.Backlog))
	}
}

func TestResumeTooOldOrFutureResyncs(t *testing.T) {
	h := NewHub(3)
	for i := 0; i < 10; i++ {
		h.Publish(TypeHost, i)
	}
	// Ring holds 8, 9, 10. Last seen 5 means 6 and 7 are lost.
	sub := h.Subscribe(5, true)
	defer sub.Close()
	if !sub.Resync {
		t.Fatal("want resync for evicted events")
	}
	// Last seen 7: 8.. are all in the ring.
	sub2 := h.Subscribe(7, true)
	defer sub2.Close()
	if sub2.Resync || len(sub2.Backlog) != 3 {
		t.Fatalf("resync=%v backlog=%d", sub2.Resync, len(sub2.Backlog))
	}
	// An id from a previous process (ahead of us) resyncs too.
	sub3 := h.Subscribe(99, true)
	defer sub3.Close()
	if !sub3.Resync {
		t.Fatal("want resync for future id")
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	h := NewHub(1000)
	sub := h.subscribe(0, false, 2)
	for i := 0; i < 5; i++ {
		h.Publish(TypeHost, i)
	}
	n := 0
	for range sub.C {
		n++
	}
	if n != 2 {
		t.Fatalf("received %d before close, want 2", n)
	}
	if !sub.Lagged() {
		t.Fatal("want lagged")
	}
	if h.Subscribers() != 0 {
		t.Fatalf("subscribers = %d", h.Subscribers())
	}
}
