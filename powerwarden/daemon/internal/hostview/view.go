// Package hostview caches powermaster's host list and folds its events in.
package hostview

import (
	"sync"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

// Effect is something an event implies beyond the cached host fields.
type Effect struct {
	Kind     string // EffectBootSucceeded or EffectBootFailed
	Hostname string
	Command  pm.Command
	At       time.Time
}

const (
	EffectBootSucceeded = "boot_succeeded" // powerup or reboot succeeded: starts boot grace
	EffectBootFailed    = "boot_failed"    // powerup or reboot failed: cooldown starts
)

// View is the last-known powermaster host list.
type View struct {
	mu        sync.Mutex
	order     []string
	hosts     map[string]*pm.Host
	cmds      map[int64]pm.Command // non-terminal commands seen in events
	done      map[int64]bool       // terminal command ids; a late "queued" for one is stale
	lastSync  time.Time            // last full read or event
	synced    bool
	connected bool
}

func New() *View {
	return &View{hosts: map[string]*pm.Host{}, cmds: map[int64]pm.Command{}, done: map[int64]bool{}}
}

// Replace installs a full GET /v1/hosts result.
func (v *View) Replace(hosts []pm.Host, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.order = v.order[:0]
	v.hosts = make(map[string]*pm.Host, len(hosts))
	for _, h := range hosts {
		h := cloneHost(h)
		v.order = append(v.order, h.Hostname)
		v.hosts[h.Hostname] = &h
	}
	v.lastSync, v.synced, v.connected = now, true, true
}

// MarkUnreachable records a failed powermaster call.
func (v *View) MarkUnreachable() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.connected = false
}

// Synced reports whether at least one full read has succeeded.
func (v *View) Synced() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.synced
}

// Connected reports whether the last powermaster interaction succeeded.
func (v *View) Connected() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.connected
}

// Fresh reports whether the view is usable for power decisions: synced, and
// refreshed (full read or event) no longer than maxAge ago.
func (v *View) Fresh(now time.Time, maxAge time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.synced && v.connected && now.Sub(v.lastSync) <= maxAge
}

// Hosts returns copies of the hosts in inventory order.
func (v *View) Hosts() []pm.Host {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]pm.Host, 0, len(v.order))
	for _, n := range v.order {
		out = append(out, cloneHost(*v.hosts[n]))
	}
	return out
}

// Host returns one host.
func (v *View) Host(name string) (pm.Host, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	h, ok := v.hosts[name]
	if !ok {
		return pm.Host{}, false
	}
	return cloneHost(*h), true
}

// NoteCommand applies the command returned by a Power call, so the next pass
// sees the in-flight command before its event arrives.
func (v *View) NoteCommand(c pm.Command, now time.Time) []Effect {
	return v.Apply(pm.Event{Type: pm.EventCommand, Command: &c}, now)
}

// Apply folds one event into the view and returns its effects.
func (v *View) Apply(ev pm.Event, now time.Time) []Effect {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ev.Type != pm.EventResync {
		v.lastSync, v.connected = now, true
	}
	switch {
	case ev.Type == pm.EventHost && ev.Host != nil:
		h, ok := v.hosts[ev.Host.Hostname]
		if !ok {
			return nil // unknown host: the next full read picks it up
		}
		if ev.Host.State != h.State || !ev.Host.StateSince.IsZero() {
			h.StateSince = ev.Host.StateSince
		}
		h.State = ev.Host.State
		if ev.Host.ActiveCommandID == nil {
			h.ActiveCommand = nil
		} else if id := *ev.Host.ActiveCommandID; v.done[id] {
			h.ActiveCommand = nil
		} else if ev.Host.ActiveCommand != nil {
			ac := *ev.Host.ActiveCommand
			h.ActiveCommand = &ac
		} else if h.ActiveCommand == nil || h.ActiveCommand.ID != id {
			ac := &pm.ActiveCommand{ID: id}
			if c, ok := v.cmds[id]; ok {
				ac.Action, ac.State, ac.Deadline = c.Action, c.State, c.Deadline
			}
			h.ActiveCommand = ac
		}
	case ev.Type == pm.EventCommand && ev.Command != nil:
		c := *ev.Command
		h, ok := v.hosts[c.Hostname]
		if c.Active() {
			if v.done[c.ID] {
				return nil // the terminal event already arrived (Power response processed late)
			}
			v.cmds[c.ID] = c
			if ok && (h.ActiveCommand == nil || h.ActiveCommand.ID <= c.ID) {
				h.ActiveCommand = &pm.ActiveCommand{ID: c.ID, Action: c.Action, State: c.State, Deadline: c.Deadline}
			}
			return nil
		}
		delete(v.cmds, c.ID)
		if v.done[c.ID] {
			return nil // duplicate terminal update: effects already reported
		}
		if len(v.done) > 4096 {
			v.done = map[int64]bool{}
		}
		v.done[c.ID] = true
		if !ok {
			return nil
		}
		if h.ActiveCommand != nil && h.ActiveCommand.ID == c.ID {
			h.ActiveCommand = nil
		}
		if c.Action != pm.ActionPowerup && c.Action != pm.ActionReboot {
			return nil
		}
		at := now
		if c.FinishedAt != nil {
			at = *c.FinishedAt
		}
		if c.State == pm.CmdSucceeded {
			return []Effect{{Kind: EffectBootSucceeded, Hostname: c.Hostname, Command: c, At: at}}
		}
		if c.State == pm.CmdFailed {
			h.PowerFailedAt = &at
			if h.State == pm.StatePoweringOn || h.State == pm.StateRebooting {
				// Unknown (not a candidate, not idle-eligible) until powermaster's host event.
				h.State = pm.StateUnknown
			}
			return []Effect{{Kind: EffectBootFailed, Hostname: c.Hostname, Command: c, At: at}}
		}
	}
	return nil
}

func cloneHost(h pm.Host) pm.Host {
	if h.ActiveCommand != nil {
		ac := *h.ActiveCommand
		h.ActiveCommand = &ac
	}
	if h.PowerFailedAt != nil {
		t := *h.PowerFailedAt
		h.PowerFailedAt = &t
	}
	return h
}
