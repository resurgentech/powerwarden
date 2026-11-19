package events

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func pub(h *Hub, typ string) {
	h.Publish(typ, func(seq int64) any { return map[string]any{"seq": seq, "type": typ} })
}

// readEvents connects with lastID ("" for none) and returns the first n
// "event:" names plus whether a heartbeat was seen.
func readEvents(t *testing.T, srv *httptest.Server, lastID string, n int) ([]string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var names []string
	heartbeat := false
	for len(names) < n && sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
		if line == ": heartbeat" {
			heartbeat = true
		}
	}
	return names, heartbeat
}

func TestResumeFromRing(t *testing.T) {
	h := New(3, time.Hour)
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, typ := range []string{TypeDemand, TypeWorker, TypeHost, TypeDecision} {
		pub(h, typ)
	}
	// Ring holds seq 2..4; resuming after 2 replays 3 and 4.
	if got, _ := readEvents(t, srv, "2", 2); strings.Join(got, ",") != "host,decision" {
		t.Fatalf("got %v", got)
	}
	// Seq 1 needed but gone: resync.
	if got, _ := readEvents(t, srv, "0", 1); got[0] != TypeResync {
		t.Fatalf("got %v", got)
	}
}

func TestLiveAndHeartbeat(t *testing.T) {
	h := New(10, 20*time.Millisecond)
	srv := httptest.NewServer(h)
	defer srv.Close()
	go func() {
		for h.Subscribers() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		pub(h, TypeDemand)
	}()
	got, hb := readEvents(t, srv, "", 1)
	if len(got) != 1 || got[0] != TypeDemand || !hb {
		t.Fatalf("got %v heartbeat=%v", got, hb)
	}
}

func TestBadLastEventID(t *testing.T) {
	h := New(10, time.Hour)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Last-Event-ID", "abc")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d", rec.Code)
	}
}
