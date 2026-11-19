package powermaster

import (
	"context"

	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// RecoverAtStart fails every running command: no sequence goroutine survives
// a restart. Every interrupted command ends unknown and the next polls
// re-seed the host (an interrupted powerup or reboot also sets
// power_failed_at; a powerdown or powercut does not). Queued commands are
// left queued for the claim loop.
func (m *Manager) RecoverAtStart() error {
	running, err := m.st.RunningCommands()
	if err != nil {
		return err
	}
	for _, c := range running {
		m.recoverCommand(c, ErrInterrupted)
	}
	return nil
}

func (m *Manager) recoverCommand(c store.Command, msg string) {
	state, powerFailed := store.StateUnknown, true
	if c.Action == ActionPowerdown || c.Action == ActionPowercut {
		powerFailed = false
	}
	applied, err := m.st.Recover(c.ID, state, powerFailed, msg, m.clk.Now())
	if err != nil {
		m.log.Error("recovery failed", "command_id", c.ID, "hostname", c.Hostname, "error", err)
		return
	}
	done, err := m.st.GetCommand(c.ID)
	if err == nil {
		m.logCommand(done, "command recovered")
		m.publishCommand(done)
	}
	if applied {
		m.logState(c.Hostname, transitionalFor(c.Action), state, "recovery")
		if _, ok := m.hosts[c.Hostname]; ok {
			m.publishHost(c.Hostname)
		}
	} else {
		m.log.Warn("recovery left host state alone: attempt superseded", "command_id", c.ID, "hostname", c.Hostname)
	}
}

// CheckDeadlines handles running commands past their deadline. If the host's
// goroutine is running the command, it is cancelled through its context and
// the goroutine's own terminal write records the failure; recovery never
// writes state for a host whose goroutine is running. A running command with
// no goroutine (an orphan) is recovered as at start.
func (m *Manager) CheckDeadlines() {
	running, err := m.st.RunningCommands()
	if err != nil {
		m.log.Error("deadline check", "error", err)
		return
	}
	now := m.clk.Now()
	for _, c := range running {
		if !now.After(c.Deadline) {
			continue
		}
		r, ok := m.hosts[c.Hostname]
		if !ok {
			m.recoverCommand(c, ErrInterrupted)
			continue
		}
		if r.mu.TryLock() {
			// No sequence holds the host: the command is orphaned.
			m.recoverCommand(c, ErrInterrupted)
			r.mu.Unlock()
			continue
		}
		if r.cancelRunning(c.ID, errDeadline) {
			m.log.Warn("command past deadline; cancelling", "command_id", c.ID, "hostname", c.Hostname)
		}
	}
}

func (m *Manager) recoveryLoop(ctx context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(m.cfg.RecoveryInterval.D()):
			m.CheckDeadlines()
		}
	}
}
