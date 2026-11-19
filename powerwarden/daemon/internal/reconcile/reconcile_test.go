package reconcile

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/hostview"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

const (
	v1 = "veritech01.labtwo"
	v2 = "veritech02.labtwo"
	v3 = "veritech03.labtwo"
)

func TestCovers(t *testing.T) {
	cases := []struct {
		have, need []string
		want       bool
	}{
		{nil, nil, true},
		{[]string{"kernel"}, nil, true},
		{nil, []string{"kernel"}, false},
		{[]string{"kernel"}, []string{"kernel"}, true},
		{[]string{"kernel", "x86"}, []string{"kernel"}, true},
		{[]string{"kernel"}, []string{"kernel", "x86"}, false},
		{[]string{"tiny"}, []string{"kernel"}, false},
	}
	for _, c := range cases {
		if got := Covers(c.have, c.need); got != c.want {
			t.Errorf("Covers(%v, %v) = %v", c.have, c.need, got)
		}
	}
}

// W3: two kernel demands and no worker -> one request now, a second after power_on_spacing.
func TestTwoDemandsOneRequestPerSpacing(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff), kernelHost(v2, pm.StateSoftOff), kernelHost(v3, pm.StateSoftOff))
	h.demand("hourchd:a", "kernel")
	h.advance(time.Second)
	h.demand("hourchd:b", "kernel")

	res := h.pass()
	h.wantRequests(pm.ActionPowerup, v1)
	if r := h.requests(pm.ActionPowerup)[0]; r.Reason != "hourchd:a" {
		t.Fatalf("reason = %q, want oldest demand", r.Reason)
	}
	if res.Output.CoveredBy["hourchd:a"] != "host:"+v1 {
		t.Fatalf("covered_by %v", res.Output.CoveredBy)
	}

	// Immediately: v1's queued powerup covers a; b waits for spacing.
	h.advance(14 * time.Second)
	res = h.pass()
	h.wantRequests(pm.ActionPowerup, v1)
	if res.Output.CoveredBy["hourchd:a"] != "host:"+v1 || res.Output.CoveredBy["hourchd:b"] != "" {
		t.Fatalf("covered_by %v", res.Output.CoveredBy)
	}

	h.advance(time.Second)
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2)

	// Both covered by in-flight powerups: never a third.
	h.advance(time.Minute)
	res = h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2)
	if res.Output.CoveredBy["hourchd:b"] != "host:"+v2 {
		t.Fatalf("covered_by %v", res.Output.CoveredBy)
	}
}

// W3: powerup succeeded, no worker, inside boot_grace -> no second request;
// after boot_grace -> next host.
func TestBootGrace(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff), kernelHost(v2, pm.StateSoftOff))
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1)

	h.advance(3 * time.Minute)
	h.pm.Run(h.lastCmd(v1).ID)
	h.pm.Finish(h.lastCmd(v1).ID, true)
	res := h.pass()
	h.wantRequests(pm.ActionPowerup, v1)
	if res.Output.CoveredBy["hourchd:a"] != "host:"+v1 || res.Output.Hosts[v1].BootGraceUntil == nil {
		t.Fatalf("boot grace not covering: %+v", res.Output.Hosts[v1])
	}
	if res.Output.Hosts[v1].IdleEligible {
		t.Fatal("host in boot grace must not be idle-eligible")
	}

	h.advance(4*time.Minute + 59*time.Second)
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1)

	h.advance(time.Second) // grace (5m) over
	res = h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2)
	if !h.hasDecision("boot_grace_expired", v1) {
		t.Fatal("no boot_grace_expired decision")
	}
	if !res.Output.Hosts[v1].IdleEligible {
		t.Fatal("v1 should be idle-eligible after grace with no worker")
	}
}

// A fresh worker ends boot grace at once.
func TestWorkerEndsBootGrace(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff))
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.pm.Finish(h.lastCmd(v1).ID, true)
	h.pass()
	h.worker("veritech01", v1, "idle", "", "kernel")
	res := h.pass()
	if res.Output.CoveredBy["hourchd:a"] != "worker:veritech01" || res.Output.Hosts[v1].BootGraceUntil != nil {
		t.Fatalf("worker should cover and end grace: %v %+v", res.Output.CoveredBy, res.Output.Hosts[v1])
	}
	p, _ := h.st.HostPolicies(h.ctx)
	if p[v1].LastPowerupOKAt != nil {
		t.Fatal("last_powerup_ok_at not cleared")
	}
	if h.hasDecision("boot_grace_expired", v1) {
		t.Fatal("grace ended by worker is not an expiry")
	}
}

// W3: powerup failed -> cooldown and next host chosen at once.
func TestFailedPowerupCooldownNextHost(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff), kernelHost(v2, pm.StateSoftOff))
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1)

	h.advance(15 * time.Minute)
	h.pm.Finish(h.lastCmd(v1).ID, false)
	res := h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2)
	if !h.hasDecision("cooldown_started", v1) {
		t.Fatal("no cooldown_started decision")
	}
	if res.Output.Hosts[v1].CooldownUntil == nil {
		t.Fatal("v1 not in cooldown")
	}

	// v2 fails too; v1 is still cooling down: no candidate.
	h.advance(time.Minute)
	h.pm.Finish(h.lastCmd(v2).ID, false)
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2)
	if !h.hasDecision("no_candidate", "") {
		t.Fatal("no no_candidate decision")
	}

	// After boot_cooldown from v1's failure, v1 is a candidate again.
	h.advance(9 * time.Minute)
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2, v1)
}

// W3: a kernel host with a tiny worker is idle-eligible.
func TestKernelHostWithTinyWorkerIdleEligible(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn))
	h.demand("hourchd:a", "kernel")
	h.worker("veritech01", v1, "idle", "", "tiny")
	res := h.pass()
	if res.Output.CoveredBy["hourchd:a"] != "" {
		t.Fatal("tiny worker must not cover a kernel demand")
	}
	if !res.Output.Hosts[v1].IdleEligible {
		t.Fatal("kernel host with tiny worker should be idle-eligible")
	}
	h.advance(9 * time.Minute)
	h.worker("veritech01", v1, "idle", "", "tiny")
	h.r.SyncHosts(h.ctx)
	h.pass()
	h.wantRequests(pm.ActionPowerdown)
	h.advance(time.Minute)
	h.worker("veritech01", v1, "idle", "", "tiny")
	h.r.SyncHosts(h.ctx)
	h.pass()
	h.wantRequests(pm.ActionPowerdown, v1)
}

// W3: busy row with expired lease inside busy_grace holds the host and a newer idle releases it.
func TestBusyHold(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn))
	h.worker("veritech01", v1, "busy", "hourchd:gone", "kernel")
	h.pass()

	// Lease (90s) long expired, dwell long past, still inside busy_grace (2h).
	h.advance(time.Hour)
	h.r.SyncHosts(h.ctx)
	res := h.pass()
	if res.Output.Hosts[v1].BusyHeldUntil == nil || res.Output.Hosts[v1].IdleEligible {
		t.Fatalf("expired busy row should hold: %+v", res.Output.Hosts[v1])
	}
	h.advance(30 * time.Minute)
	h.r.SyncHosts(h.ctx)
	h.pass()
	h.wantRequests(pm.ActionPowerdown)

	// A newer idle releases the hold at once; dwell then runs from now.
	h.worker("veritech01", v1, "idle", "", "kernel")
	res = h.pass()
	if res.Output.Hosts[v1].BusyHeldUntil != nil || !res.Output.Hosts[v1].IdleEligible {
		t.Fatalf("idle should release hold: %+v", res.Output.Hosts[v1])
	}
	h.advance(10 * time.Minute)
	h.r.SyncHosts(h.ctx)
	h.pass()
	h.wantRequests(pm.ActionPowerdown, v1)
}

// Busy hold ends after busy_grace even with no idle.
func TestBusyHoldExpires(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn))
	h.worker("veritech01", v1, "busy", "", "kernel")
	h.advance(2 * time.Hour)
	h.r.SyncHosts(h.ctx)
	res := h.pass()
	if res.Output.Hosts[v1].BusyHeldUntil != nil || !res.Output.Hosts[v1].IdleEligible {
		t.Fatalf("hold should end at busy_grace: %+v", res.Output.Hosts[v1])
	}
}

// W3: powermaster unreachable -> no requests and no powerdowns.
func TestPowermasterUnreachable(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff), kernelHost(v2, pm.StateOn))
	h.pass() // starts v2's idle dwell
	h.pm.SetUnreachable(true)
	h.advance(61 * time.Second) // view older than host_view_refresh
	if err := h.r.SyncHosts(h.ctx); err == nil {
		t.Fatal("sync should fail")
	}
	h.demand("hourchd:a", "kernel")
	h.advance(20 * time.Minute)
	res := h.pass()
	if len(h.pm.Requests()) != 0 {
		t.Fatalf("requests while unreachable: %v", h.pm.Requests())
	}
	if len(res.Output.PowerupAttempts) != 0 || res.Output.Powerdown != "" {
		t.Fatalf("planned actions on stale view: %+v", res.Output)
	}

	// Reachable again: acts.
	h.pm.SetUnreachable(false)
	h.r.SyncHosts(h.ctx)
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1)
}

// Even a reachable powermaster whose view was not refreshed is stale.
func TestStaleViewNoPowerdown(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn))
	h.pass()
	h.advance(11 * time.Minute)
	res := h.r.Pass(h.ctx) // no host view refresh
	if res.Output.Powerdown != "" || len(h.pm.Requests()) != 0 {
		t.Fatal("powerdown on a stale view")
	}
}

// Rules 2 and 3: one-to-one pairing; busy covers only its demand_id.
func TestPairingOneToOne(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn), kernelHost(v2, pm.StateOn))
	h.demand("hourchd:a", "kernel")
	h.advance(time.Second)
	h.demand("hourchd:b", "kernel")
	h.advance(time.Second)
	h.demand("hourchd:c", "kernel")
	h.worker("w-busy", v1, "busy", "hourchd:c", "kernel")
	h.worker("w-busy-nodemand", v1, "busy", "", "kernel")
	h.worker("w-error", v2, "error", "", "kernel")
	h.worker("w-idle", v2, "idle", "", "kernel")
	h.worker("w-unknown", "sdf1.labtwo", "idle", "", "kernel")
	res := h.pass()
	want := map[string]string{"hourchd:a": "worker:w-idle", "hourchd:b": "", "hourchd:c": "worker:w-busy"}
	for id, by := range want {
		if res.Output.CoveredBy[id] != by {
			t.Errorf("%s covered_by %q, want %q", id, res.Output.CoveredBy[id], by)
		}
	}
	if len(res.Output.UnknownHostWorkers) != 1 || res.Output.UnknownHostWorkers[0] != "w-unknown" {
		t.Fatalf("unknown host workers %v", res.Output.UnknownHostWorkers)
	}
}

// Idle workers on a host that is not "on" cover nothing.
func TestIdleWorkerHostNotOn(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StatePoweringOff))
	h.demand("hourchd:a", "kernel")
	h.worker("veritech01", v1, "idle", "", "kernel")
	if res := h.pass(); res.Output.CoveredBy["hourchd:a"] != "" {
		t.Fatal("worker on powering_off host covered demand")
	}
}

// Rule 7: locked, excluded, non-off, tagless and non-covering hosts are never candidates;
// locked and excluded hosts are never powered down.
func TestCandidateFilterAndNeverManaged(t *testing.T) {
	locked := kernelHost("locked", pm.StateSoftOff)
	locked.Locked = true
	h := newHarness(t,
		locked,
		kernelHost("excluded", pm.StateSoftOff),
		host{Host: pm.Host{Hostname: "tagless", State: pm.StateSoftOff}},
		host{pm.Host{Hostname: "tiny", State: pm.StateSoftOff}, []string{"tiny"}},
		kernelHost("unknown-state", pm.StateUnknown),
		kernelHost("ok", pm.StateSoftOff),
	)
	h.st.SetExclude(h.ctx, "excluded", true)
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, "ok")

	lockedOn := kernelHost("locked-on", pm.StateOn)
	lockedOn.Locked = true
	h2 := newHarness(t, lockedOn, kernelHost("excluded-on", pm.StateOn))
	h2.st.SetExclude(h2.ctx, "excluded-on", true)
	h2.pass()
	h2.advance(time.Hour)
	h2.r.SyncHosts(h2.ctx)
	h2.pass()
	if len(h2.pm.Requests()) != 0 {
		t.Fatalf("locked/excluded host powered down: %v", h2.pm.Requests())
	}
}

// W6: unset (nil) and empty host tags both mean no capability. Such hosts are
// not candidates even for a tagless demand; there is no legacy fallback.
func TestTaglessHostHasNoCapability(t *testing.T) {
	h := newHarness(t,
		host{Host: pm.Host{Hostname: "unset", State: pm.StateSoftOff}},
		host{pm.Host{Hostname: "empty", State: pm.StateSoftOff}, []string{}},
	)
	h.demand("hourchd:a")
	res := h.pass()
	if len(h.pm.Requests()) != 0 {
		t.Fatalf("tagless host powered up: %v", h.pm.Requests())
	}
	if res.Output.CoveredBy["hourchd:a"] != "" || len(res.Output.NoCandidate) != 1 {
		t.Fatalf("covered_by %q, no candidate %v", res.Output.CoveredBy["hourchd:a"], res.Output.NoCandidate)
	}
}

// Rule 7: a 409 or 5xx skips that host this pass; the next candidate is tried.
func TestRequestFailureSkipsHost(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff), kernelHost(v2, pm.StateSoftOff))
	h.pm.FailNext(v1, &pm.APIError{Status: http.StatusConflict, Code: "command_in_flight"})
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1, v2)
	if !h.hasDecision("request_failed", v1) || !h.hasDecision("powerup_requested", v2) {
		t.Fatal("missing decisions")
	}
}

// Rule 8: no powerdown in a pass that requested a powerup; one powerdown per pass.
func TestPowerdownOnlyWithoutPowerup(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn), kernelHost(v2, pm.StateOn), kernelHost(v3, pm.StateSoftOff))
	h.pass()
	h.advance(10 * time.Minute)
	h.r.SyncHosts(h.ctx)
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v3)
	h.wantRequests(pm.ActionPowerdown)

	h.pass()
	h.wantRequests(pm.ActionPowerdown, v1)
	h.pass()
	h.wantRequests(pm.ActionPowerdown, v1, v2)
}

// Rule 10: a restart (no idle_since) restarts the dwell; idle_since persists otherwise.
func TestIdleDwellPersisted(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn))
	h.pass()
	p, _ := h.st.HostPolicies(h.ctx)
	if p[v1].IdleSince == nil || !p[v1].IdleSince.Equal(t0) {
		t.Fatalf("idle_since not stored: %+v", p[v1])
	}
	h.worker("veritech01", v1, "busy", "", "kernel")
	h.pass()
	p, _ = h.st.HostPolicies(h.ctx)
	if p[v1].IdleSince != nil {
		t.Fatal("idle_since not cleared when not idle-eligible")
	}
}

// In flight from an operator's reboot counts like a powerup (Key Decision 5).
func TestRebootInFlightCovers(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn), kernelHost(v2, pm.StateSoftOff))
	if _, err := h.pm.Power(h.ctx, v1, pm.ActionReboot, "manual"); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.demand("hourchd:a", "kernel")
	res := h.pass()
	if res.Output.CoveredBy["hourchd:a"] != "host:"+v1 {
		t.Fatalf("reboot in flight should cover: %v", res.Output.CoveredBy)
	}
	h.wantRequests(pm.ActionPowerup)
}

// Event-driven: a demand write plus Trigger yields a powerup with no clock advance.
func TestRunEventDriven(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff), kernelHost(v2, pm.StateSoftOff))
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.r.Run(ctx) }()
	waitFor(t, func() bool { return h.r.Passes() >= 1 && h.pm.Subscribers() == 2 })

	h.demand("hourchd:a", "kernel")
	h.r.Trigger()
	waitFor(t, func() bool { return len(h.requests(pm.ActionPowerup)) == 1 })

	// A powermaster event (command failed) triggers a pass that picks v2 once spacing allows.
	h.clk.Advance(16 * time.Second)
	h.pm.Finish(h.lastCmd(v1).ID, false)
	waitFor(t, func() bool { return len(h.requests(pm.ActionPowerup)) == 2 }, func() {
		for _, d := range h.r.Ring.List() {
			t.Logf("decision %+v", d)
		}
		t.Logf("last pass %+v view %+v", h.r.Last().Output.Hosts, h.r.View.Hosts())
	})
	if got := h.requests(pm.ActionPowerup)[1].Hostname; got != v2 {
		t.Fatalf("second powerup to %s", got)
	}

	// The timer backstop runs passes without triggers.
	n := h.r.Passes()
	waitForAdvance(t, h, func() bool { return h.r.Passes() > n })

	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run returned %v", err)
	}
}

// Run waits for the first host view before any pass.
func TestRunNoPassBeforeHostView(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateSoftOff))
	h.pm.SetUnreachable(true)
	h.r.View = hostview.New()
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	go h.r.Run(ctx)
	time.Sleep(20 * time.Millisecond)
	if h.r.Passes() != 0 {
		t.Fatal("pass ran before the host view loaded")
	}
	h.pm.SetUnreachable(false)
	waitForAdvance(t, h, func() bool { return h.r.Passes() >= 1 })
}

func waitFor(t *testing.T, cond func() bool, onFail ...func()) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			for _, f := range onFail {
				f()
			}
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForAdvance(t *testing.T, h *harness, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		h.clk.Advance(15 * time.Second)
		time.Sleep(time.Millisecond)
	}
}

// Rule 7: soft_off candidates come before cold_off, regardless of inventory order.
func TestSoftOffCandidateChosenFirst(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateColdOff), kernelHost(v2, pm.StateColdOff), kernelHost(v3, pm.StateSoftOff))
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v3)
}

// With no soft_off host, cold_off hosts are candidates in inventory order.
func TestColdOffCandidatesInInventoryOrder(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateColdOff), kernelHost(v2, pm.StateColdOff))
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1)
}

// ---- PWC6 rule 11: two-stage power-off ----

func softOffHost(name string, since time.Time) host {
	return host{pm.Host{Hostname: name, State: pm.StateSoftOff, StateSince: since}, []string{"kernel"}}
}

func TestColdOffFiresAfterDwell(t *testing.T) {
	h := newHarness(t, softOffHost(v1, t0))
	h.advance(59 * time.Minute)
	res := h.pass()
	h.wantRequests(pm.ActionPowercut)
	if res.Output.Hosts[v1].ColdOffDueAt == nil || !res.Output.Hosts[v1].ColdOffDueAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("cold_off_due_at = %v", res.Output.Hosts[v1].ColdOffDueAt)
	}
	h.advance(time.Minute)
	h.pass()
	h.wantRequests(pm.ActionPowercut, v1)
	if !h.hasDecision("powercut_requested", v1) {
		t.Fatal("no powercut_requested decision")
	}
	// The fake finishes it: host is cold_off and is not cut again.
	h.pm.Finish(h.lastCmd(v1).ID, true)
	h.advance(2 * time.Hour)
	h.pass()
	h.wantRequests(pm.ActionPowercut, v1)
}

func TestColdOffBlockers(t *testing.T) {
	cases := map[string]func(t *testing.T) *harness{
		"locked": func(t *testing.T) *harness {
			x := softOffHost(v1, t0)
			x.Locked = true
			return newHarness(t, x)
		},
		"active command": func(t *testing.T) *harness {
			x := softOffHost(v1, t0)
			x.ActiveCommand = &pm.ActiveCommand{ID: 99, Action: pm.ActionPowerdown, State: pm.CmdRunning}
			return newHarness(t, x)
		},
		"excluded": func(t *testing.T) *harness {
			h := newHarness(t, softOffHost(v1, t0))
			if err := h.st.SetExclude(h.ctx, v1, true); err != nil {
				t.Fatal(err)
			}
			return h
		},
		"claim bound": func(t *testing.T) *harness {
			// A dedicated claim: powerwarden neither boots nor cuts the host.
			h := newHarness(t, softOffHost(v1, t0))
			h.putClaim(store.ClaimPut{ID: "d", Mode: model.ClaimDedicated, Tags: []string{v1}, Detached: true, MaxSeconds: ip(72 * 3600)})
			return h
		},
		"candidate for uncovered demand": func(t *testing.T) *harness {
			// Spacing not satisfied, so no powerup is planned, yet the host is wanted.
			h := newHarness(t, softOffHost(v1, t0))
			h.advance(2 * time.Hour) // the demand must be created after the dwell: its lease is 1h
			if err := h.st.SetLastPowerupRequest(h.ctx, h.clk.Now()); err != nil {
				t.Fatal(err)
			}
			h.demand("hourchd:a", "kernel")
			return h
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			h := mk(t)
			if h.clk.Now().Before(t0.Add(2 * time.Hour)) {
				h.advance(2 * time.Hour)
			}
			res := h.pass()
			h.wantRequests(pm.ActionPowercut)
			if res.Output.Hosts[v1].ColdOffDueAt != nil {
				t.Fatalf("cold_off_due_at set while blocked: %v", res.Output.Hosts[v1].ColdOffDueAt)
			}
		})
	}
}

func TestColdOffNeverForColdOffOrMissingStateSince(t *testing.T) {
	h := newHarness(t, host{pm.Host{Hostname: v1, State: "off", StateSince: t0}, []string{"kernel"}},
		host{pm.Host{Hostname: v2, State: pm.StateColdOff, StateSince: t0}, []string{"kernel"}},
		host{pm.Host{Hostname: v3, State: pm.StateSoftOff}, []string{"kernel"}}) // no state_since: old powermaster
	h.advance(48 * time.Hour)
	h.pass()
	h.wantRequests(pm.ActionPowercut)
}

func TestColdOffAfterZeroDisables(t *testing.T) {
	h := newHarness(t, softOffHost(v1, t0))
	h.r.Timings.ColdOffAfter = 0
	h.advance(48 * time.Hour)
	res := h.pass()
	h.wantRequests(pm.ActionPowercut)
	if res.Output.Hosts[v1].ColdOffDueAt != nil {
		t.Fatal("due set while disabled")
	}
}

func TestSoftOffWantedByDemandIsPoweredUpNotCut(t *testing.T) {
	h := newHarness(t, softOffHost(v1, t0))
	h.advance(2 * time.Hour)
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup, v1)
	h.wantRequests(pm.ActionPowercut)
}

func TestOnePowerOffPerPassSharedWithPowerdown(t *testing.T) {
	h := newHarness(t, kernelHost(v1, pm.StateOn), softOffHost(v2, t0))
	h.pass() // idle dwell starts for v1
	h.advance(2 * time.Hour)
	h.pass()
	h.wantRequests(pm.ActionPowerdown, v1)
	h.wantRequests(pm.ActionPowercut)
	h.advance(time.Second)
	h.pass()
	h.wantRequests(pm.ActionPowercut, v2)
}

// A host reporting the removed legacy state "off" is not a powerup candidate.
func TestLegacyOffStringIsNotACandidate(t *testing.T) {
	h := newHarness(t, kernelHost(v1, "off"))
	h.demand("hourchd:a", "kernel")
	h.pass()
	h.wantRequests(pm.ActionPowerup)
}
