package keyreq

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// RetryDelay is how long the retry waits after a 401 on the first call with a
// just-issued key. Tests shrink it.
var RetryDelay = 5 * time.Second

// WrapRetry arms the retry-once rule (SR-9): when armed, the first request
// through the returned client that is answered 401 is sent once more after
// RetryDelay, and that answer is returned. After the first request (any
// outcome) the rule is spent. While the first request is in flight, other
// requests wait for it. Unarmed, hc is returned as it is.
func WrapRetry(hc *http.Client, armed bool) *http.Client {
	if !armed {
		return hc
	}
	c := *hc
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &retryTransport{base: base}
	return &c
}

type retryTransport struct {
	base http.RoundTripper
	gate sync.Mutex
	done atomic.Bool
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.done.Load() {
		return t.base.RoundTrip(req)
	}
	t.gate.Lock()
	if t.done.Load() {
		t.gate.Unlock()
		return t.base.RoundTrip(req)
	}
	defer t.gate.Unlock()
	defer t.done.Store(true)

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	replay := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return resp, nil // cannot be sent again: the 401 stands
		}
		body, gerr := req.GetBody()
		if gerr != nil {
			return resp, nil
		}
		replay.Body = body
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	timer := time.NewTimer(RetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return t.base.RoundTrip(replay)
}
