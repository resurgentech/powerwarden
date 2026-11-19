package pm

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"
)

// ErrUnreachable is returned by Fake while SetUnreachable(true).
var ErrUnreachable = errors.New("fake powermaster unreachable")

// Request is one Power call recorded by Fake.
type Request struct {
	Hostname, Action, Reason string
	At                       time.Time
}

// Fake is an in-memory powermaster for tests. It follows powermaster's
// command rules (202 enqueue, same-action idempotency, 409s, 200 when the
// steady state already holds) and emits host and command events.
type Fake struct {
	mu          sync.Mutex
	now         func() time.Time
	order       []string
	hosts       map[string]*Host
	cmds        map[int64]*Command
	nextID      int64
	seq         int64
	requests    []Request
	unreachable bool
	failNext    map[string]error
	subs        []chan Event
}

// NewFake returns an empty fake using now for timestamps.
func NewFake(now func() time.Time) *Fake {
	return &Fake{now: now, hosts: map[string]*Host{}, cmds: map[int64]*Command{}, failNext: map[string]error{}}
}

// AddHost appends a host in inventory order.
func (f *Fake) AddHost(h Host) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := h
	f.order = append(f.order, h.Hostname)
	f.hosts[h.Hostname] = &cp
}

// SetUnreachable makes every call fail.
func (f *Fake) SetUnreachable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreachable = v
}

// FailNext makes the next Power call for hostname return err.
func (f *Fake) FailNext(hostname string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[hostname] = err
}

// Requests returns every Power call so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// Host returns a copy of a host.
func (f *Fake) Host(name string) Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.hosts[name]
}

func (f *Fake) ListHosts(ctx context.Context) ([]Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return nil, ErrUnreachable
	}
	out := make([]Host, 0, len(f.order))
	for _, n := range f.order {
		h := *f.hosts[n]
		if h.ActiveCommand != nil {
			ac := *h.ActiveCommand
			h.ActiveCommand = &ac
		}
		out = append(out, h)
	}
	return out, nil
}

func (f *Fake) Power(ctx context.Context, hostname, action, reason string) (Command, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.requests = append(f.requests, Request{Hostname: hostname, Action: action, Reason: reason, At: now})
	if f.unreachable {
		return Command{}, ErrUnreachable
	}
	if err, ok := f.failNext[hostname]; ok {
		delete(f.failNext, hostname)
		return Command{}, err
	}
	h, ok := f.hosts[hostname]
	if !ok {
		return Command{}, &APIError{Status: http.StatusNotFound, Code: "not_found"}
	}
	if h.Locked {
		return Command{}, &APIError{Status: http.StatusConflict, Code: "host_locked"}
	}
	if h.ActiveCommand != nil {
		c := f.cmds[h.ActiveCommand.ID]
		if c.Action == action {
			return *c, nil
		}
		return Command{}, &APIError{Status: http.StatusConflict, Code: "command_in_flight"}
	}
	f.nextID++
	c := &Command{ID: f.nextID, Hostname: hostname, Action: action, RequestedBy: "powerwarden",
		Reason: reason, CreatedAt: now, Deadline: now.Add(15 * time.Minute)}
	f.cmds[c.ID] = c
	if action == ActionPowercut && !IsOff(h.State) {
		return Command{}, &APIError{Status: http.StatusConflict, Code: "wrong_state"}
	}
	if (action == ActionPowercut && h.State != StateSoftOff) ||
		(action == ActionPowerup && h.State == StateOn) || (action == ActionPowerdown && IsOff(h.State)) {
		c.State = CmdSucceeded
		c.FinishedAt = &now
		f.emitLocked(Event{Type: EventCommand, Command: cloneCmd(c)})
		return *c, nil
	}
	c.State = CmdQueued
	h.ActiveCommand = &ActiveCommand{ID: c.ID, Action: action, State: CmdQueued, Deadline: c.Deadline}
	h.StateSince = now
	switch action {
	case ActionPowerup:
		h.State = StatePoweringOn
	case ActionPowerdown, ActionPowercut:
		h.State = StatePoweringOff
	case ActionReboot:
		h.State = StateRebooting
	}
	f.emitLocked(Event{Type: EventCommand, Command: cloneCmd(c)})
	f.emitHostLocked(h)
	return *c, nil
}

// Run moves a queued command to running and emits the event.
func (f *Fake) Run(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cmds[id]
	c.State = CmdRunning
	if h := f.hosts[c.Hostname]; h.ActiveCommand != nil && h.ActiveCommand.ID == id {
		h.ActiveCommand.State = CmdRunning
	}
	f.emitLocked(Event{Type: EventCommand, Command: cloneCmd(c)})
}

// Finish completes a command as powermaster's sequence terminal write would.
func (f *Fake) Finish(id int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	c := f.cmds[id]
	h := f.hosts[c.Hostname]
	c.FinishedAt = &now
	if ok {
		c.State = CmdSucceeded
	} else {
		c.State = CmdFailed
		c.Error = "ping_ssh_timeout"
	}
	h.ActiveCommand = nil
	h.StateSince = now
	switch {
	case ok && c.Action == ActionPowercut:
		h.State = StateColdOff
	case ok && c.Action != ActionPowerdown:
		h.State = StateOn
	case ok:
		h.State = StateSoftOff
	case c.Action == ActionPowercut:
		h.State = StateSoftOff
	case c.Action == ActionPowerdown:
		h.State = StateOn
	default:
		h.State = StateSoftOff
		h.PowerFailedAt = &now
	}
	f.emitLocked(Event{Type: EventCommand, Command: cloneCmd(c)})
	f.emitHostLocked(h)
}

// SetLocked sets a host's locked flag (inventory change).
func (f *Fake) SetLocked(hostname string, locked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[hostname].Locked = locked
}

// SetState sets a host's state directly (operator action, seeding) and emits a host event.
func (f *Fake) SetState(hostname, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[hostname]
	if h.State != state {
		h.StateSince = f.now()
	}
	h.State = state
	f.emitHostLocked(h)
}

// LastCommand returns the most recent command for hostname.
func (f *Fake) LastCommand(hostname string) (Command, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *Command
	for _, c := range f.cmds {
		if c.Hostname == hostname && (best == nil || c.ID > best.ID) {
			best = c
		}
	}
	if best == nil {
		return Command{}, false
	}
	return *best, true
}

// Subscribers returns the number of open event subscriptions.
func (f *Fake) Subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// Emit sends an arbitrary event (e.g. resync) to subscribers.
func (f *Fake) Emit(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitLocked(ev)
}

func (f *Fake) Events(ctx context.Context, lastEventID int64) (<-chan Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return nil, ErrUnreachable
	}
	ch := make(chan Event, 256)
	f.subs = append(f.subs, ch)
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, s := range f.subs {
			if s == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				close(ch)
				break
			}
		}
	}()
	return ch, nil
}

func (f *Fake) emitHostLocked(h *Host) {
	he := &HostEvent{Hostname: h.Hostname, State: h.State, StateSince: h.StateSince}
	if h.ActiveCommand != nil {
		id := h.ActiveCommand.ID
		he.ActiveCommandID = &id
	}
	f.emitLocked(Event{Type: EventHost, Host: he})
}

func (f *Fake) emitLocked(ev Event) {
	f.seq++
	ev.Seq = f.seq
	for _, s := range f.subs {
		select {
		case s <- ev:
		default: // a slow test subscriber drops events, like a lagging SSE client
		}
	}
}

func cloneCmd(c *Command) *Command { cp := *c; return &cp }
