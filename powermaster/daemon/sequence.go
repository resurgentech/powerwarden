package powermaster

import (
	"context"
	"errors"
	"time"

	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// result is a sequence's terminal outcome. err == "" means success.
type result struct {
	state       string
	err         string
	powerFailed bool
}

// sequence runs one command under one context whose deadline is the budget.
// A cancelled context is a failure.
type sequence struct {
	m        *Manager
	r        *hostRunner
	cmd      store.Command
	ctx      context.Context
	t        ResolvedTimings
	prior    string
	hasPrior bool

	lastChassis *bool // most recent chassis reading inside this attempt
}

func (s *sequence) logger() []any {
	return []any{"command_id", s.cmd.ID, "hostname", s.cmd.Hostname, "action", s.cmd.Action}
}

// aborted maps a done context to the recorded error.
func (s *sequence) aborted() string {
	if errors.Is(context.Cause(s.ctx), errDeadline) {
		return ErrDeadlineExceeded
	}
	return ErrInterrupted
}

// fail returns a failure, preferring the context's reason when it is done.
func (s *sequence) fail(state, err string, powerFailed bool) result {
	if s.ctx.Err() != nil {
		err = s.aborted()
	}
	return result{state: state, err: err, powerFailed: powerFailed}
}

// until calls fn every step until it returns true, the stage time d passes,
// or the context ends. Each call runs under the stage's context.
func (s *sequence) until(d time.Duration, fn func(ctx context.Context) bool) bool {
	ctx, cancel := context.WithTimeout(s.ctx, d)
	defer cancel()
	for {
		if fn(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(s.m.step):
		}
	}
}

func (s *sequence) sleep(d time.Duration) bool {
	select {
	case <-s.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// chassis reads the chassis bit directly from the BMC.
func (s *sequence) chassis(ctx context.Context) (on bool, ok bool) {
	on, err := s.r.ipmi.ChassisPower(ctx)
	if err != nil {
		return false, false
	}
	s.lastChassis = &on
	return on, true
}

func (s *sequence) chassisOff(ctx context.Context) bool {
	on, ok := s.chassis(ctx)
	return ok && !on
}

// polledState is the host state implied by the last IPMI poll.
func (s *sequence) polledState() string {
	st := s.r.statusCopy()
	switch {
	case st.IPMI == nil:
		return store.StateUnknown
	case *st.IPMI:
		return store.StateOn
	}
	return store.StateSoftOff
}

// outletReadTimeout bounds the end-of-sequence outlet read, which runs even
// after the sequence's own context has ended.
const outletReadTimeout = 3 * time.Second

// offState is the off state that matches the outlets: cold_off when every
// outlet is off, otherwise soft_off (also when there are no outlets or they
// cannot be read). It reads the PDUs, falling back to the last PDU poll.
func (s *sequence) offState() string {
	if len(s.r.plan.Outlets) == 0 {
		return store.StateSoftOff
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), outletReadTimeout)
	defer cancel()
	read := map[string]map[int]bool{}
	for _, o := range s.r.plan.Outlets {
		if _, ok := read[o.Pdu]; ok {
			continue
		}
		states, err := s.m.pdus[o.Pdu].OutletStates(ctx)
		if err != nil {
			s.m.log.Debug("outlet read failed; using last pdu poll", append(s.logger(), "pdu", o.Pdu, "error", err)...)
			states = nil
		}
		read[o.Pdu] = states
	}
	s.r.smu.Lock()
	defer s.r.smu.Unlock()
	for _, o := range s.r.plan.Outlets {
		on, ok := read[o.Pdu][o.Outlet]
		if !ok {
			on, ok = s.r.outlets[outletKey(o)]
		}
		if !ok || on {
			return store.StateSoftOff
		}
	}
	return store.StateColdOff
}

// failOff fails into the off state that matches the outlets.
func (s *sequence) failOff(err string, powerFailed bool) result {
	return s.fail(s.offState(), err, powerFailed)
}

// outlets switches every host outlet, each retried within its pdu_outlet_wait.
func (s *sequence) outlets(on bool) bool {
	for _, o := range s.r.plan.Outlets {
		pdu := s.m.pdus[o.Pdu]
		ok := s.until(o.Wait, func(ctx context.Context) bool {
			if err := pdu.SetOutlet(ctx, o.Outlet, on); err != nil {
				s.m.log.Debug("pdu outlet command failed; retrying", append(s.logger(), "pdu", o.Pdu, "outlet", o.Outlet, "on", on, "error", err)...)
				return false
			}
			return true
		})
		if !ok {
			s.m.log.Warn("pdu outlet command did not succeed", append(s.logger(), "pdu", o.Pdu, "outlet", o.Outlet, "on", on)...)
			return false
		}
	}
	return true
}

// chassisOn sends chassis power-on, resending on failure (and when a sent
// command goes unanswered for the resend interval), until the chassis reads
// on or chassis_on_wait ends.
func (s *sequence) chassisOn() bool {
	var sentAt time.Time
	return s.until(s.t.ChassisOnWait, func(ctx context.Context) bool {
		if !sentAt.IsZero() {
			if on, ok := s.chassis(ctx); ok && on {
				return true
			}
			if time.Since(sentAt) < s.m.resnd {
				return false
			}
		}
		if err := s.r.ipmi.SetChassisPower(ctx, true); err != nil {
			s.m.log.Info("ipmi power-on failed; resending", append(s.logger(), "error", err)...)
			sentAt = time.Time{}
			return false
		}
		sentAt = time.Now()
		on, ok := s.chassis(ctx)
		return ok && on
	})
}

// sendOff sends chassis power-off until the send succeeds or d ends. With
// verify, it also waits within d for the chassis to read off (resending as
// chassisOn does).
func (s *sequence) sendOff(d time.Duration, verify bool) bool {
	var sentAt time.Time
	return s.until(d, func(ctx context.Context) bool {
		if s.chassisOff(ctx) {
			return true
		}
		if !sentAt.IsZero() && time.Since(sentAt) < s.m.resnd {
			return false
		}
		if err := s.r.ipmi.SetChassisPower(ctx, false); err != nil {
			s.m.log.Info("ipmi power-off failed; resending", append(s.logger(), "error", err)...)
			sentAt = time.Time{}
			return false
		}
		sentAt = time.Now()
		if !verify {
			return true
		}
		return s.chassisOff(ctx)
	})
}

// upCheck waits for chassis on, ping and SSH inside this attempt.
func (s *sequence) upCheck() result {
	why := ErrSSHDown
	ok := s.until(s.t.PingSSHWait, func(ctx context.Context) bool {
		on, ok := s.chassis(ctx)
		if !ok {
			// A read cut off by the attempt's own timeout keeps the last
			// reason; only a failed read inside the window is the BMC's fault.
			if ctx.Err() == nil {
				why = ErrIPMIUnreachable
			}
			return false
		}
		if !on {
			why = ErrChassisOffAfterPowerOn
			return false
		}
		if !s.r.net.PingHost(ctx) {
			why = ErrPingDown
			return false
		}
		if !s.r.net.SSH(ctx) {
			why = ErrSSHDown
			return false
		}
		return true
	})
	if !ok {
		return s.failOff(why, true)
	}
	return result{state: store.StateOn}
}

// powerup: skip PDU and chassis-on when the last IPMI poll says the chassis
// is on; otherwise PDU outlets on, BMC reachable, chassis on (resent). Then
// ping and SSH. Failure ends in the off state matching the outlets, with
// power_failed_at.
func (s *sequence) powerup() result {
	st := s.r.statusCopy()
	if st.IPMI != nil && *st.IPMI {
		s.m.log.Info("chassis already on; skipping PDU and chassis power-on", s.logger()...)
	} else {
		if !s.outlets(true) {
			return s.failOff(ErrPDUOnFailed, true)
		}
		if !s.until(s.t.IpmiPingWait, s.r.net.PingBMC) {
			return s.failOff(ErrIPMIUnreachable, true)
		}
		if !s.chassisOn() {
			return s.failOff(ErrChassisDidNotPowerOn, true)
		}
	}
	return s.upCheck()
}

// powerdown: SSH shutdown, wait for chassis off, IPMI power-off, PDU outlets
// off, final off wait. Each step runs only while the chassis is still on.
// Chassis off at any point is success (soft_off, or cold_off when the PDU
// fallback cut the outlets); still on after every fallback is failure with
// state on.
func (s *sequence) powerdown() result {
	done := result{state: store.StateSoftOff}
	if s.chassisOff(s.ctx) {
		return done
	}
	shutdown := s.until(s.t.SSHShutdownWait, func(ctx context.Context) bool {
		if err := s.r.net.Shutdown(ctx); err != nil {
			s.m.log.Debug("ssh shutdown failed", append(s.logger(), "error", err)...)
			return false
		}
		return true
	})
	if shutdown {
		s.m.log.Info("ssh shutdown sent; waiting for chassis off", s.logger()...)
		if s.until(s.t.PostSSHOffWait, s.chassisOff) {
			return done
		}
	}
	if s.ctx.Err() == nil {
		s.m.log.Warn("falling back to ipmi power-off", s.logger()...)
		if s.sendOff(s.t.IpmiOffCommandWait, true) {
			return done
		}
	}
	cut := false
	if len(s.r.plan.Outlets) > 0 && s.ctx.Err() == nil {
		s.m.log.Warn("falling back to pdu power-off", s.logger()...)
		cut = s.outlets(false)
	}
	if s.ctx.Err() == nil {
		// With the outlets cut, a BMC that no longer answers has no power.
		ok := s.until(s.t.FinalOffWait, func(ctx context.Context) bool {
			on, ok := s.chassis(ctx)
			return (ok && !on) || (!ok && cut)
		})
		if ok {
			if cut {
				done.state = store.StateColdOff
			}
			return done
		}
	}
	state := store.StateOn
	if s.lastChassis != nil && !*s.lastChassis {
		state = store.StateSoftOff
		if cut {
			state = store.StateColdOff
		}
	}
	return s.fail(state, ErrChassisStillOn, false)
}

// powercut: from soft_off, cut the outlets only. Otherwise run the powerdown
// stages, then cut any outlets still on. Success ends cold_off. A failed cut
// ends soft_off (the chassis is off); a powerdown that never got the chassis
// off ends as powerdown does (on).
func (s *sequence) powercut() result {
	if s.prior != store.StateSoftOff {
		res := s.powerdown()
		if res.err != "" || res.state == store.StateColdOff {
			return res
		}
	}
	if !s.outlets(false) {
		return s.fail(store.StateSoftOff, ErrPDUOffFailed, false)
	}
	return result{state: store.StateColdOff}
}

// reboot: IPMI power-off (resent), wait for chassis off; if still on, cycle
// the PDU outlets (escalated=1). Then power on as in powerup. A reboot of a
// host in either off state runs only the power-on half (outlets first).
func (s *sequence) reboot() result {
	fromOff := s.prior == store.StateSoftOff || s.prior == store.StateColdOff
	if !s.hasPrior {
		// Prior state is lost across a restart; fall back to the chassis.
		fromOff = s.chassisOff(s.ctx)
	}
	if !fromOff {
		s.sendOff(s.t.IpmiOffCommandWait, false)
		if !s.until(s.t.RebootChassisOffWait, s.chassisOff) {
			if s.ctx.Err() != nil {
				return s.fail(s.polledState(), "", false)
			}
			s.escalate()
			if !s.outlets(false) {
				return s.fail(s.polledState(), ErrPDUOffFailed, false)
			}
			if !s.sleep(s.t.RebootPduSettle) {
				return s.failOff("", true)
			}
			if !s.outlets(true) {
				return s.failOff(ErrPDUOnFailed, true)
			}
		}
	} else if len(s.r.plan.Outlets) > 0 {
		// From off the outlets may have been cut by a powerdown.
		if !s.outlets(true) {
			return s.failOff(ErrPDUOnFailed, true)
		}
	}
	if !s.chassisOn() {
		return s.failOff(ErrChassisDidNotPowerOn, true)
	}
	return s.upCheck()
}

func (s *sequence) escalate() {
	s.m.log.Warn("chassis still on after ipmi power-off; escalating reboot to pdu cycle", s.logger()...)
	c, err := s.m.st.MarkEscalated(s.cmd.ID)
	if err != nil {
		s.m.log.Error("record escalation", append(s.logger(), "error", err)...)
		return
	}
	s.cmd = c
	s.m.publishCommand(c)
}
