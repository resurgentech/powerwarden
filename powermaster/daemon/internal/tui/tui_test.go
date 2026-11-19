package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	api "github.com/resurgentech/powerwarden/powermaster/api"
)

func TestTapsFourthTapForcesAfterDelay(t *testing.T) {
	tp := &Taps{N: 4, Gap: 3 * time.Second, Delay: 2 * time.Second}
	t0 := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		d := tp.Press("h", "t", true, t0.Add(time.Duration(i)*time.Second))
		if d.Send || d.Remaining != 3-i {
			t.Fatalf("tap %d: %+v", i+1, d)
		}
	}
	d := tp.Press("h", "t", true, t0.Add(3*time.Second))
	if !d.Send || !d.Force || d.Delay != 2*time.Second {
		t.Fatalf("fourth tap: %+v", d)
	}
	// Sequence resets after firing.
	if d := tp.Press("h", "t", true, t0.Add(4*time.Second)); d.Send {
		t.Fatalf("fifth tap should start over: %+v", d)
	}
}

func TestTapsGapAndKeyReset(t *testing.T) {
	tp := &Taps{N: 4, Gap: 3 * time.Second, Delay: 2 * time.Second}
	t0 := time.Unix(1000, 0)
	tp.Press("h", "r", true, t0)
	tp.Press("h", "r", true, t0.Add(time.Second))
	// A gap of 3s or more restarts the count.
	if d := tp.Press("h", "r", true, t0.Add(4*time.Second)); d.Remaining != 3 {
		t.Fatalf("after gap: %+v", d)
	}
	// Switching keys restarts too.
	if d := tp.Press("h", "t", true, t0.Add(5*time.Second)); d.Remaining != 3 {
		t.Fatalf("after key switch: %+v", d)
	}
	// Unlocked hosts and u/d send at once, without force.
	if d := tp.Press("x", "t", false, t0); !d.Send || d.Force || d.Delay != 0 {
		t.Fatalf("unlocked: %+v", d)
	}
	if d := tp.Press("h", "u", true, t0); !d.Send || d.Force {
		t.Fatalf("u on locked: %+v", d)
	}
}

type fakeAPI struct {
	mu    sync.Mutex
	hosts []api.Host
	posts []post
}

type post struct {
	host   string
	action api.PowerAction
	force  bool
	at     time.Time
}

func (f *fakeAPI) ListHosts(context.Context) ([]api.Host, error) { return f.hosts, nil }
func (f *fakeAPI) Stats(context.Context) (*api.Stats, error)     { return &api.Stats{}, nil }
func (f *fakeAPI) Events(ctx context.Context, _ int64, _ func(Event)) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeAPI) Power(_ context.Context, host string, a api.PowerAction, force bool) (api.Command, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, post{host, a, force, time.Now()})
	return api.Command{Id: 7, Hostname: host, Action: a, State: api.Queued, Force: force}, 202, nil
}

func keys(m *Model, k string) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return cmd
}

func TestModelLockedFourthTapPostsForceAfterDelay(t *testing.T) {
	fa := &fakeAPI{hosts: []api.Host{{Hostname: "locked1", Locked: true, State: api.On}}}
	m := NewModel(context.Background(), fa, Config{LockedTaps: 4, LockedTapGap: 3 * time.Second, LockedForceDelay: 150 * time.Millisecond})
	m.Update(hostsMsg(fa.hosts))
	for i := 0; i < 3; i++ {
		if cmd := keys(m, "r"); cmd != nil {
			t.Fatalf("tap %d returned a command", i+1)
		}
		if !strings.Contains(m.status, "locked") {
			t.Fatalf("status = %q", m.status)
		}
	}
	cmd := keys(m, "r")
	if cmd == nil {
		t.Fatal("fourth tap did nothing")
	}
	start := time.Now()
	msg := cmd()
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("posted after %v; want the configured delay", el)
	}
	m.Update(msg)
	if len(fa.posts) != 1 || !fa.posts[0].force || fa.posts[0].action != api.Reboot {
		t.Fatalf("posts = %+v", fa.posts)
	}
	if m.pending["locked1"] != 7 {
		t.Fatalf("pending = %v", m.pending)
	}
}

func TestModelToggleAndPendingUntilTerminal(t *testing.T) {
	fa := &fakeAPI{hosts: []api.Host{{Hostname: "h1", State: api.SoftOff}}}
	m := NewModel(context.Background(), fa, Config{LockedTaps: 4, LockedTapGap: 3 * time.Second})
	m.Update(hostsMsg(fa.hosts))
	m.Update(keys(m, "t")())
	if len(fa.posts) != 1 || fa.posts[0].action != api.Powerup || fa.posts[0].force {
		t.Fatalf("toggle from soft_off: %+v", fa.posts)
	}
	m.Update(eventMsg{Type: "command", Data: []byte(`{"seq":3,"type":"command","command":{"id":7,"hostname":"h1","action":"reboot","state":"running","escalated":true}}`)})
	if !strings.Contains(m.View(), "PDU") && !m.blink {
		t.Fatalf("escalated reboot should show PDU:\n%s", m.View())
	}
	m.Update(eventMsg{Type: "command", Data: []byte(`{"seq":4,"type":"command","command":{"id":7,"hostname":"h1","action":"reboot","state":"succeeded"}}`)})
	if _, ok := m.pending["h1"]; ok {
		t.Fatal("pending not cleared by terminal event")
	}
	m.Update(eventMsg{Type: "host", Data: []byte(`{"seq":5,"type":"host","hostname":"h1","state":"on","status":{"pdu":null,"ipmi":true,"ping":true,"ssh":true,"observed_at":null},"active_command":null}`)})
	if m.hosts["h1"].State != api.On {
		t.Fatal("host event not applied")
	}
	m.Update(keys(m, "t")())
	if fa.posts[1].action != api.Powerdown {
		t.Fatalf("toggle from on: %+v", fa.posts[1])
	}
}

func TestModelKeyStatePairs(t *testing.T) {
	none := api.PowerAction("")
	cases := []struct {
		state api.HostState
		key   string
		want  api.PowerAction // none: nothing sent
	}{
		{api.On, "u", api.Powerup}, {api.SoftOff, "u", api.Powerup}, {api.ColdOff, "u", api.Powerup},
		{api.On, "d", api.Powerdown}, {api.SoftOff, "d", api.Powercut}, {api.ColdOff, "d", none},
		{api.On, "t", api.Powerdown}, {api.SoftOff, "t", api.Powerup}, {api.ColdOff, "t", api.Powerup},
		{api.Unknown, "t", api.Powerdown}, {api.PoweringOn, "t", api.Powerdown},
		{api.On, "r", api.Reboot}, {api.ColdOff, "r", api.Reboot},
	}
	for _, c := range cases {
		fa := &fakeAPI{hosts: []api.Host{{Hostname: "h1", State: c.state}}}
		m := NewModel(context.Background(), fa, Config{LockedTaps: 4, LockedTapGap: 3 * time.Second})
		m.Update(hostsMsg(fa.hosts))
		cmd := keys(m, c.key)
		if c.want == none {
			if cmd != nil || len(fa.posts) != 0 || !strings.Contains(m.status, "cold_off") {
				t.Errorf("%s on %s: cmd=%v posts=%v status=%q; want no-op with message", c.key, c.state, cmd != nil, fa.posts, m.status)
			}
			continue
		}
		if cmd == nil {
			t.Errorf("%s on %s: no command", c.key, c.state)
			continue
		}
		m.Update(cmd())
		if len(fa.posts) != 1 || fa.posts[0].action != c.want {
			t.Errorf("%s on %s: posts=%+v want %s", c.key, c.state, fa.posts, c.want)
		}
	}
}

func TestModelColdOffHasOwnStyle(t *testing.T) {
	if styleCold.GetForeground() == styleOff.GetForeground() {
		t.Fatal("cold_off must not share the soft_off style")
	}
}

func TestParseSSE(t *testing.T) {
	in := ": heartbeat\n\nid: 5\nevent: host\ndata: {\"seq\":5}\n\nid: 6\nevent: resync\ndata: {\"seq\":6,\"type\":\"resync\"}\n\n"
	var got []Event
	ParseSSE(strings.NewReader(in), func(e Event) { got = append(got, e) })
	if len(got) != 2 || got[0].ID != 5 || got[0].Type != "host" || got[1].Type != "resync" || string(got[0].Data) != `{"seq":5}` {
		t.Fatalf("got %+v", got)
	}
}
