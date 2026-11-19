package hostview

import (
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func TestApplyCommandLifecycle(t *testing.T) {
	v := New()
	v.Replace([]pm.Host{{Hostname: "a", State: pm.StateSoftOff}}, t0)
	if !v.Fresh(t0.Add(time.Minute), time.Minute) || v.Fresh(t0.Add(61*time.Second), time.Minute) {
		t.Fatal("freshness window wrong")
	}
	cmd := pm.Command{ID: 1, Hostname: "a", Action: pm.ActionPowerup, State: pm.CmdQueued}
	v.Apply(pm.Event{Type: pm.EventCommand, Command: &cmd}, t0)
	h, _ := v.Host("a")
	if !h.ActiveCommand.InFlightBoot() {
		t.Fatal("queued powerup not in flight")
	}
	// A host event carrying the same id keeps the command details.
	id := int64(1)
	v.Apply(pm.Event{Type: pm.EventHost, Host: &pm.HostEvent{Hostname: "a", State: pm.StatePoweringOn, ActiveCommandID: &id}}, t0)
	h, _ = v.Host("a")
	if !h.ActiveCommand.InFlightBoot() || h.State != pm.StatePoweringOn {
		t.Fatalf("host event lost details: %+v", h.ActiveCommand)
	}
	fin := t0.Add(5 * time.Minute)
	cmd.State, cmd.FinishedAt = pm.CmdFailed, &fin
	eff := v.Apply(pm.Event{Type: pm.EventCommand, Command: &cmd}, fin)
	if len(eff) != 1 || eff[0].Kind != EffectBootFailed || !eff[0].At.Equal(fin) {
		t.Fatalf("effects %+v", eff)
	}
	h, _ = v.Host("a")
	if h.ActiveCommand != nil || h.PowerFailedAt == nil || h.State != pm.StateUnknown {
		t.Fatalf("failed powerup not applied: %+v", h)
	}
	cmd2 := pm.Command{ID: 2, Hostname: "a", Action: pm.ActionPowerup, State: pm.CmdSucceeded, FinishedAt: &fin}
	if eff := v.NoteCommand(cmd2, fin); len(eff) != 1 || eff[0].Kind != EffectBootSucceeded {
		t.Fatalf("succeeded effect %+v", eff)
	}
}

// A failed boot leaves the state unknown (neither a candidate nor idle-eligible)
// until powermaster's host event arrives.
func TestFailedBootStateUnknownUntilHostEvent(t *testing.T) {
	v := New()
	v.Replace([]pm.Host{{Hostname: "a", State: pm.StatePoweringOn}}, t0)
	fin := t0.Add(time.Minute)
	cmd := pm.Command{ID: 3, Hostname: "a", Action: pm.ActionPowerup, State: pm.CmdFailed, FinishedAt: &fin}
	v.Apply(pm.Event{Type: pm.EventCommand, Command: &cmd}, fin)
	h, _ := v.Host("a")
	if h.State != pm.StateUnknown || pm.IsOff(h.State) || h.State == pm.StateOn {
		t.Fatalf("state after failed boot = %q", h.State)
	}
	v.Apply(pm.Event{Type: pm.EventHost, Host: &pm.HostEvent{Hostname: "a", State: pm.StateSoftOff}}, fin)
	if h, _ = v.Host("a"); h.State != pm.StateSoftOff {
		t.Fatalf("host event not applied: %q", h.State)
	}
}

func TestUnreachableIsStale(t *testing.T) {
	v := New()
	if v.Fresh(t0, time.Hour) {
		t.Fatal("unsynced view is fresh")
	}
	v.Replace(nil, t0)
	v.MarkUnreachable()
	if v.Fresh(t0, time.Hour) {
		t.Fatal("unreachable view is fresh")
	}
}

// A Power response processed after the command's terminal event must not
// resurrect the active command, and duplicate terminal updates have no effect.
func TestLateQueuedIgnored(t *testing.T) {
	v := New()
	v.Replace([]pm.Host{{Hostname: "a", State: pm.StateSoftOff}}, t0)
	fin := t0
	failed := pm.Command{ID: 7, Hostname: "a", Action: pm.ActionPowerup, State: pm.CmdFailed, FinishedAt: &fin}
	if eff := v.Apply(pm.Event{Type: pm.EventCommand, Command: &failed}, t0); len(eff) != 1 {
		t.Fatal("first terminal update should report")
	}
	if eff := v.NoteCommand(failed, t0); len(eff) != 0 {
		t.Fatal("duplicate terminal update reported twice")
	}
	v.NoteCommand(pm.Command{ID: 7, Hostname: "a", Action: pm.ActionPowerup, State: pm.CmdQueued}, t0)
	if h, _ := v.Host("a"); h.ActiveCommand != nil {
		t.Fatal("stale queued command resurrected")
	}
}

func TestHostEventCarriesStateSince(t *testing.T) {
	v := New()
	v.Replace([]pm.Host{{Hostname: "a", State: pm.StateSoftOff, StateSince: t0}}, t0)
	later := t0.Add(time.Hour)
	v.Apply(pm.Event{Type: pm.EventHost, Host: &pm.HostEvent{Hostname: "a", State: pm.StateColdOff, StateSince: later}}, later)
	if h, _ := v.Host("a"); h.State != pm.StateColdOff || !h.StateSince.Equal(later) {
		t.Fatalf("event state_since not applied: %+v", h)
	}
	// A state change from an old powermaster (no state_since) clears it: no cold-off timing.
	v.Apply(pm.Event{Type: pm.EventHost, Host: &pm.HostEvent{Hostname: "a", State: pm.StateSoftOff}}, later)
	if h, _ := v.Host("a"); !h.StateSince.IsZero() {
		t.Fatalf("state_since kept across a change with none reported: %+v", h)
	}
}
