package powermaster

import (
	"time"

	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// Status is a host's latest in-memory poll results. Members are nil before
// the first poll. IPMI is the chassis power bit.
type Status struct {
	PDU        *bool      `json:"pdu"`
	IPMI       *bool      `json:"ipmi"`
	Ping       *bool      `json:"ping"`
	SSH        *bool      `json:"ssh"`
	ObservedAt *time.Time `json:"observed_at"`
}

func eqBool(a, b *bool) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func (s Status) sameObservation(o Status) bool {
	return eqBool(s.PDU, o.PDU) && eqBool(s.IPMI, o.IPMI) && eqBool(s.Ping, o.Ping) && eqBool(s.SSH, o.SSH)
}

// ActiveCommand summarizes a host's queued or running command.
type ActiveCommand struct {
	ID       int64     `json:"id"`
	Action   string    `json:"action"`
	State    string    `json:"state"`
	Deadline time.Time `json:"deadline"`
}

// HostView is a host's power view. It has no secret fields: no IP, BMC
// credentials, SSH keys, PDU addresses or tokens.
type HostView struct {
	Hostname      string         `json:"hostname"`
	Locked        bool           `json:"locked"`
	Class         string         `json:"class"`
	State         string         `json:"state"`
	StateSince    time.Time      `json:"state_since"`
	Status        Status         `json:"status"`
	LastError     string         `json:"last_error"`
	PowerFailedAt *time.Time     `json:"power_failed_at"`
	ActiveCommand *ActiveCommand `json:"active_command"`
}

// HostDetail adds resolved budgets.
type HostDetail struct {
	HostView
	Budgets map[string]string `json:"budgets"`
}

// HostEvent is the payload of a `host` event.
type HostEvent struct {
	Hostname      string         `json:"hostname"`
	State         string         `json:"state"`
	StateSince    time.Time      `json:"state_since"`
	Status        Status         `json:"status"`
	ActiveCommand *ActiveCommand `json:"active_command"`
}

// CommandEvent is the payload of a `command` event.
type CommandEvent struct {
	Command store.Command `json:"command"`
}

func (r *hostRunner) statusCopy() Status {
	r.smu.Lock()
	defer r.smu.Unlock()
	return r.status
}

func (m *Manager) view(name string) (HostView, error) {
	r, ok := m.hosts[name]
	if !ok {
		return HostView{}, ErrUnknownHost
	}
	row, err := m.st.GetHost(name)
	if err != nil {
		return HostView{}, err
	}
	act, err := m.st.ActiveCommand(name)
	if err != nil {
		return HostView{}, err
	}
	v := HostView{
		Hostname: name, Locked: r.host.Locked, Class: r.plan.Class,
		State: row.State, StateSince: row.StateSince, Status: r.statusCopy(), LastError: row.LastError, PowerFailedAt: row.PowerFailedAt,
	}
	if act != nil {
		v.ActiveCommand = &ActiveCommand{ID: act.ID, Action: act.Action, State: act.State, Deadline: act.Deadline}
	}
	return v, nil
}

// Hosts lists every inventory host (hosts removed from inventory are hidden).
func (m *Manager) Hosts() ([]HostView, error) {
	out := make([]HostView, 0, len(m.names))
	for _, n := range m.names {
		v, err := m.view(n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Host returns one host with budgets.
func (m *Manager) Host(name string) (HostDetail, error) {
	v, err := m.view(name)
	if err != nil {
		return HostDetail{}, err
	}
	return HostDetail{HostView: v, Budgets: m.plans[name].Budgets.Strings()}, nil
}

func (m *Manager) publishHost(name string) {
	v, err := m.view(name)
	if err != nil {
		m.log.Error("host event", "hostname", name, "error", err)
		return
	}
	m.hub.Publish(events.TypeHost, HostEvent{Hostname: v.Hostname, State: v.State, StateSince: v.StateSince, Status: v.Status, ActiveCommand: v.ActiveCommand})
}

func (m *Manager) publishCommand(c store.Command) {
	m.hub.Publish(events.TypeCommand, CommandEvent{Command: c})
}
