package pm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The fake follows powermaster's command rules, so reconcile tests exercise them.
func TestFakeCommandRules(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	f := NewFake(func() time.Time { return now })
	f.AddHost(Host{Hostname: "off", State: StateSoftOff})
	f.AddHost(Host{Hostname: "on", State: StateOn})
	f.AddHost(Host{Hostname: "locked", State: StateSoftOff, Locked: true})
	ctx := context.Background()

	c1, err := f.Power(ctx, "off", ActionPowerup, "d1")
	if err != nil || c1.State != CmdQueued {
		t.Fatalf("enqueue: %+v %v", c1, err)
	}
	if c2, _ := f.Power(ctx, "off", ActionPowerup, "d2"); c2.ID != c1.ID {
		t.Fatal("same action in flight should return the same command")
	}
	var apiErr *APIError
	if _, err := f.Power(ctx, "off", ActionPowerdown, ""); !errors.As(err, &apiErr) || apiErr.Code != "command_in_flight" {
		t.Fatalf("want command_in_flight, got %v", err)
	}
	if _, err := f.Power(ctx, "locked", ActionPowerup, ""); !errors.As(err, &apiErr) || apiErr.Code != "host_locked" {
		t.Fatalf("want host_locked, got %v", err)
	}
	if c, _ := f.Power(ctx, "on", ActionPowerup, ""); c.State != CmdSucceeded {
		t.Fatal("powerup of an on host should succeed immediately")
	}
	f.Finish(c1.ID, false)
	if h := f.Host("off"); h.State != StateSoftOff || h.PowerFailedAt == nil || h.ActiveCommand != nil {
		t.Fatalf("failed powerup: %+v", h)
	}
	f.SetUnreachable(true)
	if _, err := f.ListHosts(ctx); !errors.Is(err, ErrUnreachable) {
		t.Fatal("unreachable fake answered")
	}
}

func TestIsOff(t *testing.T) {
	for st, want := range map[string]bool{StateSoftOff: true, StateColdOff: true,
		StateOn: false, StatePoweringOn: false, StatePoweringOff: false, StateRebooting: false, StateUnknown: false, "": false} {
		if IsOff(st) != want {
			t.Errorf("IsOff(%q) = %v, want %v", st, !want, want)
		}
	}
}

func TestFakePowerdownEndsSoftOff(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	f := NewFake(func() time.Time { return now })
	f.AddHost(Host{Hostname: "on", State: StateOn})
	c, err := f.Power(context.Background(), "on", ActionPowerdown, "")
	if err != nil {
		t.Fatal(err)
	}
	f.Finish(c.ID, true)
	if h := f.Host("on"); h.State != StateSoftOff {
		t.Fatalf("state after powerdown = %q", h.State)
	}
}

// The legacy "off" state is gone: only soft_off and cold_off are off states.
func TestLegacyOffIsNotOff(t *testing.T) {
	if IsOff("off") {
		t.Error(`IsOff("off") = true, want false`)
	}
	if !IsOff(StateSoftOff) || !IsOff(StateColdOff) {
		t.Error("soft_off and cold_off must be off states")
	}
}
