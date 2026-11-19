package pm

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

	powermasterapi "github.com/resurgentech/powerwarden/powermaster/api"
)

// HTTP is the powermaster client over the generated powermasterapi module.
type HTTP struct {
	api     *powermasterapi.Client // request/response calls, with request_timeout
	stream  *powermasterapi.Client // /v1/events, no overall timeout
	timeout time.Duration
}

// NewHTTP builds a client for baseURL authenticating with token. base may be
// nil (http.DefaultTransport); tests pass an httptest transport.
func NewHTTP(baseURL, token string, timeout time.Duration, base http.RoundTripper) (*HTTP, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	auth := powermasterapi.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
	api, err := powermasterapi.NewClient(baseURL, auth,
		powermasterapi.WithHTTPClient(&http.Client{Transport: base, Timeout: timeout}))
	if err != nil {
		return nil, err
	}
	stream, err := powermasterapi.NewClient(baseURL, auth, powermasterapi.WithHTTPClient(&http.Client{Transport: base}))
	if err != nil {
		return nil, err
	}
	return &HTTP{api: api, stream: stream, timeout: timeout}, nil
}

func (c *HTTP) ListHosts(ctx context.Context) ([]Host, error) {
	resp, err := c.api.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var hosts []powermasterapi.Host
	if err := json.NewDecoder(resp.Body).Decode(&hosts); err != nil {
		return nil, fmt.Errorf("powermaster: decode hosts: %w", err)
	}
	out := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, Host{
			Hostname: h.Hostname, Locked: h.Locked, State: string(h.State), StateSince: h.StateSince.UTC(),
			PowerFailedAt: utc(h.PowerFailedAt), ActiveCommand: activeCommand(h.ActiveCommand),
		})
	}
	return out, nil
}

func (c *HTTP) Power(ctx context.Context, hostname, action, reason string) (Command, error) {
	body := powermasterapi.PowerRequest{Action: powermasterapi.PowerAction(action)}
	if reason != "" {
		if len(reason) > 256 {
			reason = reason[:256]
		}
		body.Reason = &reason
	}
	resp, err := c.api.RequestPower(ctx, hostname, nil, body)
	if err != nil {
		return Command{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return Command{}, apiError(resp)
	}
	var cmd powermasterapi.Command
	if err := json.NewDecoder(resp.Body).Decode(&cmd); err != nil {
		return Command{}, fmt.Errorf("powermaster: decode command: %w", err)
	}
	return command(cmd), nil
}

// Events opens GET /v1/events. The returned channel closes when the stream
// ends or ctx is cancelled.
func (c *HTTP) Events(ctx context.Context, lastEventID int64) (<-chan Event, error) {
	var params *powermasterapi.StreamEventsParams
	if lastEventID > 0 {
		params = &powermasterapi.StreamEventsParams{LastEventID: &lastEventID}
	}
	resp, err := c.stream.StreamEvents(ctx, params)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		readSSE(ctx, resp.Body, func(data []byte) bool {
			ev, err := decodeEvent(data)
			if err != nil {
				return true // skip what we cannot parse; a resync or refresh recovers
			}
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return ch, nil
}

// readSSE calls fn with the data of each complete SSE message until EOF,
// error, or fn returns false. Comment lines (heartbeats) are ignored.
func readSSE(ctx context.Context, r io.Reader, fn func(data []byte) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var data []string
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				if !fn([]byte(strings.Join(data, "\n"))) {
					return
				}
				data = data[:0]
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

func decodeEvent(data []byte) (Event, error) {
	var raw powermasterapi.Event
	if err := json.Unmarshal(data, &raw); err != nil {
		return Event{}, err
	}
	typ, err := raw.Discriminator()
	if err != nil {
		return Event{}, err
	}
	switch typ {
	case string(powermasterapi.EventTypeHost):
		h, err := raw.AsHostEvent()
		if err != nil {
			return Event{}, err
		}
		he := &HostEvent{Hostname: h.Hostname, State: string(h.State), StateSince: h.StateSince.UTC(), ActiveCommand: activeCommand(h.ActiveCommand)}
		if he.ActiveCommand != nil {
			id := he.ActiveCommand.ID
			he.ActiveCommandID = &id
		}
		return Event{Seq: h.Seq, Type: EventHost, Host: he}, nil
	case string(powermasterapi.EventTypeCommand):
		c, err := raw.AsCommandEvent()
		if err != nil {
			return Event{}, err
		}
		cmd := command(c.Command)
		return Event{Seq: c.Seq, Type: EventCommand, Command: &cmd}, nil
	case string(powermasterapi.EventTypeResync):
		r, err := raw.AsResyncEvent()
		if err != nil {
			return Event{}, err
		}
		return Event{Seq: r.Seq, Type: EventResync}, nil
	}
	return Event{}, fmt.Errorf("powermaster: unknown event type %q", typ)
}

func apiError(resp *http.Response) error {
	var body powermasterapi.Error
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if json.Unmarshal(b, &body) != nil || body.Error == "" {
		return &APIError{Status: resp.StatusCode, Code: strconv.Itoa(resp.StatusCode), Message: strings.TrimSpace(string(b))}
	}
	return &APIError{Status: resp.StatusCode, Code: string(body.Error), Message: body.Message}
}

func activeCommand(a *powermasterapi.ActiveCommand) *ActiveCommand {
	if a == nil {
		return nil
	}
	return &ActiveCommand{ID: a.Id, Action: string(a.Action), State: string(a.State), Deadline: a.Deadline.UTC()}
}

func command(c powermasterapi.Command) Command {
	return Command{
		ID: c.Id, Hostname: c.Hostname, Action: string(c.Action), State: string(c.State),
		RequestedBy: c.RequestedBy, Reason: c.Reason, Error: c.Error,
		Force: c.Force, Escalated: c.Escalated, Attempt: c.Attempt, StartedAt: utc(c.StartedAt),
		CreatedAt: c.CreatedAt.UTC(), FinishedAt: utc(c.FinishedAt), Deadline: c.Deadline.UTC(),
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
