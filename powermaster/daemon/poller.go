package powermaster

import (
	"context"
	"fmt"
	"time"

	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// Pollers update in-memory status and emit host events on change. They never
// write hosts.state; the only state write reachable from a poll is the seed of
// an `unknown` host from its PDU and IPMI observations (store.Seed is guarded
// by state='unknown').

// loop runs fn now and then every interval until ctx ends. The interval is the
// wait between polls: a poll runs, then the loop waits one interval, so polls
// never overlap or queue and a slow read stretches the cadence (an 8s Eaton
// read plus a 2s wait is about 10s).
//
// Each poll gets a context timeout equal to the interval, so a PDU read gets
// 2s by default. That is harmless only because neither PDU client takes the
// context (see pollPDU). Before making those clients honour it, give the PDU
// poll its own timeout.
func (m *Manager) loop(ctx context.Context, every time.Duration, fn func(context.Context)) {
	defer m.wg.Done()
	for {
		pctx, cancel := context.WithTimeout(ctx, every)
		fn(pctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(every):
		}
	}
}

// observe applies a status update and publishes a host event if it changed.
func (m *Manager) observe(r *hostRunner, apply func(*Status)) {
	now := m.clk.Now()
	r.smu.Lock()
	before := r.status
	apply(&r.status)
	r.status.ObservedAt = &now
	changed := !before.sameObservation(r.status)
	r.smu.Unlock()
	if changed {
		m.publishHost(r.host.Hostname)
	}
}

// pollIPMI reads the chassis bit. A failed poll clears Status.IPMI (the BMC
// does not answer) and publishes a host event if that changed it.
func (m *Manager) pollIPMI(ctx context.Context, r *hostRunner) {
	on, err := r.ipmi.ChassisPower(ctx)
	if err != nil {
		m.log.Debug("ipmi poll failed", "hostname", r.host.Hostname, "error", err)
		m.observe(r, func(s *Status) { s.IPMI = nil })
	} else {
		m.observe(r, func(s *Status) { s.IPMI = &on })
	}
	m.seed(r)
}

// seed moves an `unknown` host to its first state. It waits for a full PDU
// observation: all outlets off is cold_off whatever the BMC says; otherwise
// the chassis bit gives on or soft_off; outlets on (or mixed) with a BMC that
// does not answer stay unknown.
func (m *Manager) seed(r *hostRunner) {
	r.smu.Lock()
	known, allOff, ipmi := r.outletsKnown(), r.outletsAllOff(), r.status.IPMI
	r.smu.Unlock()
	var state string
	switch {
	case !known:
		return
	case allOff:
		state = store.StateColdOff
	case ipmi == nil:
		return
	case *ipmi:
		state = store.StateOn
	default:
		state = store.StateSoftOff
	}
	ok, err := m.st.Seed(r.host.Hostname, state, m.clk.Now())
	if err != nil {
		m.log.Error("seed failed", "hostname", r.host.Hostname, "error", err)
		return
	}
	if ok {
		m.logState(r.host.Hostname, store.StateUnknown, state, "seed")
		m.publishHost(r.host.Hostname)
	}
}

// outletsKnown reports whether every outlet of the host has been read.
// Callers hold r.smu.
func (r *hostRunner) outletsKnown() bool {
	for _, o := range r.plan.Outlets {
		if _, ok := r.outlets[outletKey(o)]; !ok {
			return false
		}
	}
	return len(r.plan.Outlets) > 0
}

// outletsAllOff reports whether every outlet is known to be off. Callers hold
// r.smu.
func (r *hostRunner) outletsAllOff() bool {
	if !r.outletsKnown() {
		return false
	}
	for _, o := range r.plan.Outlets {
		if r.outlets[outletKey(o)] {
			return false
		}
	}
	return true
}

func (m *Manager) pollNet(ctx context.Context, r *hostRunner) {
	ping := r.net.PingHost(ctx)
	ssh := r.net.SSH(ctx)
	m.observe(r, func(s *Status) { s.Ping, s.SSH = &ping, &ssh })
}

// pollPDU reads one PDU and demuxes outlet states to the hosts it feeds. A
// host's pdu status is true when all of its outlets are on; the all-off
// aggregate (outletsAllOff) is derived from the same readings. A PDU poll
// also seeds an `unknown` host.
//
// The ctx deadline is the poll interval (see loop), shorter than an Eaton
// read. The PDU clients are called without it (hw/real.go OutletStates) and
// this function does not check ctx after the read, so a slow read still
// lands. Anyone who makes the clients honour ctx must first give the PDU poll
// its own timeout (TestSlowPDUReadLongerThanIntervalStillUpdatesStatus).
func (m *Manager) pollPDU(ctx context.Context, name string) {
	states, err := m.pdus[name].OutletStates(ctx)
	if err != nil {
		m.log.Debug("pdu poll failed", "pdu", name, "error", err)
		return
	}
	for _, hn := range m.names {
		r := m.hosts[hn]
		feeds := false
		for _, o := range r.plan.Outlets {
			if o.Pdu == name {
				feeds = true
			}
		}
		if !feeds {
			continue
		}
		r.smu.Lock()
		for _, o := range r.plan.Outlets {
			if o.Pdu == name {
				r.outlets[outletKey(o)] = states[o.Outlet]
			}
		}
		all, known := true, true
		for _, o := range r.plan.Outlets {
			v, ok := r.outlets[outletKey(o)]
			known = known && ok
			all = all && v
		}
		r.smu.Unlock()
		if known {
			m.observe(r, func(s *Status) { s.PDU = &all })
			m.seed(r)
		}
	}
}

func outletKey(o OutletPlan) string { return fmt.Sprintf("%s/%d", o.Pdu, o.Outlet) }
