package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	api "github.com/resurgentech/powerwarden/powermaster/api"
)

// API is what the TUI needs from powermaster.
type API interface {
	ListHosts(ctx context.Context) ([]api.Host, error)
	Stats(ctx context.Context) (*api.Stats, error)
	Power(ctx context.Context, host string, action api.PowerAction, force bool) (api.Command, int, error)
	// Events streams events until ctx ends or the stream breaks. lastID < 0
	// means no Last-Event-ID.
	Events(ctx context.Context, lastID int64, fn func(Event)) error
}

// Event is one SSE message.
type Event struct {
	ID   int64
	Type string
	Data []byte
}

// HTTPClient implements API with the generated powermasterapi client.
type HTTPClient struct {
	c      *api.ClientWithResponses
	base   string
	token  string
	stream *http.Client
}

// NewHTTPClient builds a client for baseURL with a bearer token.
func NewHTTPClient(baseURL, token string) (*HTTPClient, error) {
	auth := func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	c, err := api.NewClientWithResponses(baseURL, api.WithRequestEditorFn(auth),
		api.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	if err != nil {
		return nil, err
	}
	return &HTTPClient{c: c, base: baseURL, token: token, stream: &http.Client{}}, nil
}

func apiErr(status int, body []byte) error {
	var e api.Error
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("%d %s: %s", status, e.Error, e.Message)
	}
	return fmt.Errorf("HTTP %d", status)
}

func (h *HTTPClient) ListHosts(ctx context.Context) ([]api.Host, error) {
	r, err := h.c.ListHostsWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, apiErr(r.StatusCode(), r.Body)
	}
	return *r.JSON200, nil
}

func (h *HTTPClient) Stats(ctx context.Context) (*api.Stats, error) {
	r, err := h.c.GetStatsWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, apiErr(r.StatusCode(), r.Body)
	}
	return r.JSON200, nil
}

func (h *HTTPClient) Power(ctx context.Context, host string, action api.PowerAction, force bool) (api.Command, int, error) {
	params := &api.RequestPowerParams{}
	if force {
		params.Force = &force
	}
	reason := "tui"
	r, err := h.c.RequestPowerWithResponse(ctx, host, params, api.PowerRequest{Action: action, Reason: &reason})
	if err != nil {
		return api.Command{}, 0, err
	}
	switch {
	case r.JSON200 != nil:
		return *r.JSON200, 200, nil
	case r.JSON202 != nil:
		return *r.JSON202, 202, nil
	}
	return api.Command{}, r.StatusCode(), apiErr(r.StatusCode(), r.Body)
}

func (h *HTTPClient) Events(ctx context.Context, lastID int64, fn func(Event)) error {
	params := &api.StreamEventsParams{}
	if lastID >= 0 {
		params.LastEventID = &lastID
	}
	req, err := api.NewStreamEventsRequest(h.base, params)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return apiErr(resp.StatusCode, b)
	}
	return ParseSSE(resp.Body, fn)
}

// ParseSSE reads an SSE stream and calls fn for each event.
func ParseSSE(r io.Reader, fn func(Event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var cur Event
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Type != "" || len(data) > 0 {
				cur.Data = []byte(strings.Join(data, "\n"))
				fn(cur)
			}
			cur, data = Event{}, nil
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat
		case strings.HasPrefix(line, "id:"):
			cur.ID, _ = strconv.ParseInt(strings.TrimSpace(line[3:]), 10, 64)
		case strings.HasPrefix(line, "event:"):
			cur.Type = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
