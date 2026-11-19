// Package tui is `powermaster monitor`: a power-only operator TUI. It lists
// GET /v1/hosts, follows /v1/events and sends power actions. Keys follow the
// old powerwarden tui.go: u powerup, d powerdown, t toggle, plus r reboot.
// d on on sends powerdown, on soft_off sends powercut, and on cold_off does
// nothing (already cut). t sends powerup from either off state, else powerdown.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	api "github.com/resurgentech/powerwarden/powermaster/api"
)

// Config tunes the TUI (from the `tui` config section).
type Config struct {
	LockedTaps       int
	LockedTapGap     time.Duration
	LockedForceDelay time.Duration
}

// Model is the bubbletea model.
type Model struct {
	api    API
	ctx    context.Context
	events chan tea.Msg
	now    func() time.Time

	hosts   map[string]api.Host
	names   []string
	cursor  int
	taps    *Taps
	pending map[string]int64 // host -> command id until a terminal event
	escal   map[int64]bool
	blink   bool
	stats   *api.Stats
	status  string
	err     string
}

// Messages.
type (
	hostsMsg  []api.Host
	statsMsg  *api.Stats
	eventMsg  Event
	resyncMsg struct{}
	errMsg    struct{ err error }
	blinkMsg  struct{}
	statsTick struct{}
	sentMsg   struct {
		host   string
		action api.PowerAction
		force  bool
		cmd    api.Command
		code   int
		err    error
	}
)

// NewModel builds the model. Call Stream in a goroutine to feed events.
func NewModel(ctx context.Context, a API, cfg Config) *Model {
	if cfg.LockedTaps < 1 {
		cfg.LockedTaps = 4
	}
	return &Model{
		api: a, ctx: ctx, events: make(chan tea.Msg, 256), now: time.Now,
		hosts: map[string]api.Host{}, pending: map[string]int64{}, escal: map[int64]bool{},
		taps: &Taps{N: cfg.LockedTaps, Gap: cfg.LockedTapGap, Delay: cfg.LockedForceDelay},
	}
}

// Stream follows /v1/events, reconnecting with Last-Event-ID, and feeds the
// model until ctx ends.
func (m *Model) Stream(ctx context.Context) {
	last := int64(-1)
	for ctx.Err() == nil {
		err := m.api.Events(ctx, last, func(ev Event) {
			last = ev.ID
			if ev.Type == "resync" {
				m.events <- resyncMsg{}
				return
			}
			m.events <- eventMsg(ev)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.events <- errMsg{fmt.Errorf("events: %v (reconnecting)", err)}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-m.events:
			return msg
		case <-m.ctx.Done():
			return nil
		}
	}
}

func (m *Model) fetchHosts() tea.Cmd {
	return func() tea.Msg {
		h, err := m.api.ListHosts(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return hostsMsg(h)
	}
}

func (m *Model) fetchStats() tea.Cmd {
	return func() tea.Msg {
		s, err := m.api.Stats(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return statsMsg(s)
	}
}

func blinkTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return blinkMsg{} })
}

func statsTickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return statsTick{} })
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.fetchHosts(), m.fetchStats(), m.waitEvent(), blinkTick(), statsTickCmd())
}

func (m *Model) selected() (api.Host, bool) {
	if len(m.names) == 0 {
		return api.Host{}, false
	}
	return m.hosts[m.names[m.cursor]], true
}

// send returns a command that waits delay, then POSTs the action.
func (m *Model) send(host string, action api.PowerAction, force bool, delay time.Duration) tea.Cmd {
	do := func() tea.Msg {
		cmd, code, err := m.api.Power(m.ctx, host, action, force)
		return sentMsg{host: host, action: action, force: force, cmd: cmd, code: code, err: err}
	}
	if delay <= 0 {
		return do
	}
	return func() tea.Msg {
		select {
		case <-time.After(delay):
		case <-m.ctx.Done():
			return nil
		}
		return do()
	}
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m, m.key(msg.String())
	case hostsMsg:
		m.hosts = map[string]api.Host{}
		m.names = m.names[:0]
		for _, h := range msg {
			m.hosts[h.Hostname] = h
			m.names = append(m.names, h.Hostname)
			if h.ActiveCommand != nil {
				m.pending[h.Hostname] = h.ActiveCommand.Id
			} else {
				delete(m.pending, h.Hostname)
			}
		}
		sort.Strings(m.names)
		if m.cursor >= len(m.names) {
			m.cursor = max(0, len(m.names)-1)
		}
	case statsMsg:
		m.stats = msg
	case statsTick:
		return m, tea.Batch(m.fetchStats(), statsTickCmd())
	case resyncMsg:
		return m, tea.Batch(m.fetchHosts(), m.waitEvent())
	case eventMsg:
		m.applyEvent(Event(msg))
		return m, m.waitEvent()
	case errMsg:
		m.err = msg.err.Error()
		if strings.HasPrefix(m.err, "events:") {
			return m, m.waitEvent()
		}
	case blinkMsg:
		m.blink = !m.blink
		return m, blinkTick()
	case sentMsg:
		if msg.err != nil {
			m.err = fmt.Sprintf("%s %s: %v", msg.action, msg.host, msg.err)
			return m, nil
		}
		m.err = ""
		m.status = fmt.Sprintf("%s %s: command %d %s (HTTP %d)", msg.action, msg.host, msg.cmd.Id, msg.cmd.State, msg.code)
		if msg.cmd.State == api.Queued || msg.cmd.State == api.Running {
			m.pending[msg.host] = msg.cmd.Id
		}
	}
	return m, nil
}

func (m *Model) key(k string) tea.Cmd {
	switch k {
	case "q", "ctrl+c":
		return tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return nil
	case "down", "j":
		if m.cursor < len(m.names)-1 {
			m.cursor++
		}
		return nil
	}
	h, ok := m.selected()
	if !ok {
		return nil
	}
	var action api.PowerAction
	switch k {
	case "u":
		action = api.Powerup
	case "d":
		switch h.State {
		case api.ColdOff:
			m.status = fmt.Sprintf("%s is cold_off: already cut", h.Hostname)
			return nil
		case api.SoftOff:
			action = api.Powercut
		default:
			action = api.Powerdown
		}
	case "t":
		action = api.Powerdown
		if h.State == api.SoftOff || h.State == api.ColdOff {
			action = api.Powerup
		}
	case "r":
		action = api.Reboot
	default:
		return nil
	}
	d := m.taps.Press(h.Hostname, k, h.Locked, m.now())
	if !d.Send {
		m.status = fmt.Sprintf("%s is locked: tap %q %d more time(s) to force %s", h.Hostname, k, d.Remaining, action)
		return nil
	}
	if d.Force {
		m.status = fmt.Sprintf("forcing %s on locked %s in %s", action, h.Hostname, d.Delay)
	} else {
		m.status = fmt.Sprintf("sending %s to %s", action, h.Hostname)
	}
	return m.send(h.Hostname, action, d.Force, d.Delay)
}

func (m *Model) applyEvent(ev Event) {
	switch ev.Type {
	case "host":
		var he api.HostEvent
		if json.Unmarshal(ev.Data, &he) != nil {
			return
		}
		h, ok := m.hosts[he.Hostname]
		if !ok {
			return
		}
		h.State, h.Status, h.ActiveCommand = he.State, he.Status, he.ActiveCommand
		m.hosts[he.Hostname] = h
		if he.ActiveCommand != nil {
			m.pending[he.Hostname] = he.ActiveCommand.Id
		}
	case "command":
		var ce api.CommandEvent
		if json.Unmarshal(ev.Data, &ce) != nil {
			return
		}
		c := ce.Command
		if c.Escalated {
			m.escal[c.Id] = true
		}
		switch c.State {
		case api.Succeeded, api.Failed:
			if m.pending[c.Hostname] == c.Id {
				delete(m.pending, c.Hostname)
			}
			delete(m.escal, c.Id)
			if c.State == api.Failed {
				m.err = fmt.Sprintf("%s %s failed: %s", c.Action, c.Hostname, c.Error)
			}
		default:
			m.pending[c.Hostname] = c.Id
		}
	}
}

var (
	styleHeader = lipgloss.NewStyle().Bold(true)
	styleSel    = lipgloss.NewStyle().Reverse(true)
	styleOn     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleOff    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCold   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func flag(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "Y"
	}
	return "n"
}

// View implements tea.Model.
func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(fmt.Sprintf("%-28s %-13s %-4s %-4s %-4s %-4s %-6s %-14s %-12s %s",
		"HOST", "STATE", "PDU", "IPMI", "PING", "SSH", "LOCK", "CLASS", "COMMAND", "LAST ERROR")))
	b.WriteString("\n")
	for i, n := range m.names {
		h := m.hosts[n]
		cmd := ""
		if id, ok := m.pending[n]; ok {
			cmd = fmt.Sprintf("#%d", id)
			if h.ActiveCommand != nil && h.ActiveCommand.Id == id {
				cmd = fmt.Sprintf("%s #%d", h.ActiveCommand.Action, id)
			}
			if m.escal[id] {
				cmd += " PDU"
			}
			if m.blink {
				cmd = strings.Repeat(" ", len(cmd))
			}
		}
		lock := ""
		if h.Locked {
			lock = "LOCKED"
		}
		state := string(h.State)
		switch h.State {
		case api.On:
			state = styleOn.Render(fmt.Sprintf("%-13s", state))
		case api.ColdOff:
			state = styleCold.Render(fmt.Sprintf("%-13s", state))
		case api.SoftOff, api.Unknown:
			state = styleOff.Render(fmt.Sprintf("%-13s", state))
		default:
			state = styleWarn.Render(fmt.Sprintf("%-13s", state))
		}
		line := fmt.Sprintf("%-28s %s %-4s %-4s %-4s %-4s %-6s %-14s %-12s %s", n, state,
			flag(h.Status.Pdu), flag(h.Status.Ipmi), flag(h.Status.Ping), flag(h.Status.Ssh),
			lock, h.Class, cmd, h.LastError)
		if i == m.cursor {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	if m.stats != nil {
		var states []string
		for k, v := range m.stats.HostsByState {
			states = append(states, fmt.Sprintf("%s=%d", k, v))
		}
		sort.Strings(states)
		b.WriteString(fmt.Sprintf("hosts: %s  running: %d  subscribers: %d", strings.Join(states, " "), m.stats.CommandsRunning, m.stats.Subscribers))
		if e := m.stats.LastPowerError; e != nil {
			b.WriteString(fmt.Sprintf("  last error: %s #%d %s", e.Hostname, e.CommandId, e.Error))
		}
		b.WriteString("\n")
	}
	if m.status != "" {
		b.WriteString(m.status + "\n")
	}
	if m.err != "" {
		b.WriteString(styleErr.Render(m.err) + "\n")
	}
	b.WriteString("u powerup  d powerdown (soft_off: powercut)  t toggle  r reboot  ↑/↓ select  q quit\n")
	return b.String()
}

// Run starts the TUI.
func Run(ctx context.Context, a API, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := NewModel(ctx, a, cfg)
	go m.Stream(ctx)
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen()).Run()
	return err
}
