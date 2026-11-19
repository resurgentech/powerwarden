package powermaster

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powermaster/daemon/hw"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/clock"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// Test lab: h1 is fed by p1 outlet 1; h2 by p1 outlet 2; h3 is locked and fed
// by p1 outlets 3 and 4 (every inventory host must have an outlet).
func labInventory() *inventory.Inventory {
	inv := &inventory.Inventory{
		Hosts: []inventory.Host{
			{Hostname: "h1", IP: "10.9.9.1", User: "root", Ipmi: inventory.Ipmi{IP: "10.9.8.1", Username: "admin", Password: "s3cret-ipmi"},
				Pdu: []inventory.PduRef{{Name: "p1", Outlet: 1}}, SSHKey: inventory.SSHKey{PrivateKey: "UFJJVktFWQ=="}},
			{Hostname: "h2", IP: "10.9.9.2", Ipmi: inventory.Ipmi{IP: "10.9.8.2", Username: "admin", Password: "s3cret-ipmi"},
				Pdu: []inventory.PduRef{{Name: "p1", Outlet: 2}}},
			{Hostname: "h3", IP: "10.9.9.3", Locked: true, Ipmi: inventory.Ipmi{IP: "10.9.8.3", Username: "admin", Password: "s3cret-ipmi"},
				Pdu: []inventory.PduRef{{Name: "p1", Outlet: 3}, {Name: "p1", Outlet: 4}}},
		},
		Pdus: map[string]inventory.Pdu{"p1": {IP: "10.9.7.1", Username: "pduadmin", Password: "s3cret-pdu", PduStyle: "kasa_hs300"}},
	}
	if err := inv.Validate(); err != nil {
		panic(err)
	}
	return inv
}

// fastConfig shrinks every stage so sequences finish in milliseconds.
func fastConfig() Config {
	cfg := DefaultConfig()
	ms := func(n int) Duration { return Duration(time.Duration(n) * time.Millisecond) }
	cfg.Timings = Timings{
		PduOutletWait: ms(150), IpmiPingWait: ms(150), ChassisOnWait: ms(150), PingSSHWait: ms(150),
		SSHShutdownWait: ms(60), PostSSHOffWait: ms(80), IpmiOffCommandWait: ms(80), FinalOffWait: ms(80),
		RebootChassisOffWait: ms(80), RebootPduSettle: ms(10), DeadlineSlack: ms(500),
	}
	return cfg
}

type harness struct {
	t     *testing.T
	m     *Manager
	lab   *hw.FakeLab
	st    *store.Store
	inv   *inventory.Inventory
	path  string
	clk   clock.Clock
	ctx   context.Context
	stop  context.CancelFunc
	sub   *events.Subscription
	start bool
	wrap  func(hw.Provider) hw.Provider
}

type hopt func(*harness, *Options)

func withClock(c clock.Clock) hopt { return func(_ *harness, o *Options) { o.Clock = c } }
func withPath(p string) hopt       { return func(h *harness, _ *Options) { h.path = p } }
func withPollers() hopt            { return func(_ *harness, o *Options) { o.DisablePollers = false } }
func wrapHW(f func(hw.Provider) hw.Provider) hopt {
	return func(h *harness, _ *Options) { h.wrap = f }
}
func noStart() hopt { return func(h *harness, _ *Options) { h.start = false } }

func newHarness(t *testing.T, opts ...hopt) *harness {
	t.Helper()
	h := &harness{t: t, inv: labInventory(), path: filepath.Join(t.TempDir(), "state.db"), clk: clock.Real{}, start: true}
	o := Options{Config: fastConfig(), Step: 2 * time.Millisecond, Resend: 20 * time.Millisecond, DisablePollers: true,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, f := range opts {
		f(h, &o)
	}
	st, err := store.Open(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	h.lab = hw.NewFakeLab(h.inv)
	o.Inventory, o.Store, o.HW = h.inv, st, h.lab
	if h.wrap != nil {
		o.HW = h.wrap(h.lab)
	}
	if o.Clock != nil {
		h.clk = o.Clock
	}
	h.m, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	h.sub = h.m.Hub().Subscribe(0, false)
	h.ctx, h.stop = context.WithCancel(context.Background())
	t.Cleanup(func() {
		h.stop()
		h.m.Wait()
		st.Close()
	})
	if h.start {
		if err := h.m.Start(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) setState(host, state string) {
	h.t.Helper()
	if _, err := h.st.Seed(host, state, h.clk.Now()); err != nil {
		h.t.Fatal(err)
	}
	if got := h.host(host).State; got != state {
		h.t.Fatalf("setState: %s is %s", host, got)
	}
}

func (h *harness) host(name string) store.Host {
	h.t.Helper()
	row, err := h.st.GetHost(name)
	if err != nil {
		h.t.Fatal(err)
	}
	return row
}

func (h *harness) request(host, action string) store.Command {
	h.t.Helper()
	res, err := h.m.Request(Request{Hostname: host, Action: action, RequestedBy: "jared", Reason: "test"})
	if err != nil {
		h.t.Fatalf("request %s %s: %v", host, action, err)
	}
	return res.Command
}

// wait polls the command until it is terminal.
func (h *harness) wait(id int64) store.Command {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := h.st.GetCommand(id)
		if err != nil {
			h.t.Fatal(err)
		}
		if !c.InFlight() {
			return c
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("command %d not terminal", id)
	return store.Command{}
}

func (h *harness) idle(host string) {
	h.t.Helper()
	r := h.m.hosts[host]
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.mu.TryLock() {
			r.mu.Unlock()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("%s never idle", host)
}

func (h *harness) pollAll() {
	ctx := context.Background()
	for _, r := range h.m.hosts {
		h.m.pollIPMI(ctx, r)
		h.m.pollNet(ctx, r)
	}
	for name := range h.m.pdus {
		h.m.pollPDU(ctx, name)
	}
}

func callsMatching(calls []string, sub string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func indexOf(calls []string, sub string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, sub) })
}

func TestPowerupSucceeds(t *testing.T) {
	h := newHarness(t)
	h.setState("h1", store.StateSoftOff)
	c := h.wait(h.request("h1", ActionPowerup).ID)
	if c.State != store.CmdSucceeded || c.Error != "" {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h1"); row.State != store.StateOn || row.LastError != "" {
		t.Fatalf("host = %+v", row)
	}
	calls := h.lab.Calls()
	if indexOf(calls, "p1 pdu.1.on") > indexOf(calls, "h1 ipmi.on") || indexOf(calls, "h1 ipmi.on") > indexOf(calls, "h1 ssh") {
		t.Fatalf("order: %v", calls)
	}
}

func TestPowerupResendsOnIPMIFailure(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateSoftOff)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.PowerOnFailures = 3 })
	c := h.wait(h.request("h2", ActionPowerup).ID)
	if c.State != store.CmdSucceeded {
		t.Fatalf("command = %+v", c)
	}
	if n := callsMatching(h.lab.Calls(), "h2 ipmi.on"); n != 4 {
		t.Fatalf("ipmi.on sent %d times, want 4", n)
	}
}

func TestPowerupChassisOnSSHDownEndsSoftOff(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateSoftOff)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	c := h.wait(h.request("h2", ActionPowerup).ID)
	if c.State != store.CmdFailed || c.Error != ErrSSHDown {
		t.Fatalf("command = %+v", c)
	}
	row := h.host("h2")
	if row.State != store.StateSoftOff || row.LastError != ErrSSHDown || row.PowerFailedAt == nil {
		t.Fatalf("host = %+v", row)
	}
	if !h.lab.Chassis("h2") {
		t.Fatal("test premise: chassis should be on")
	}
}

func TestPowerupOfAlreadyOnChassisSkipsPDUAndChassisOn(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = true })
	h.pollAll() // last IPMI poll: chassis on
	// The stored state says off (e.g. an earlier powerup failed on SSH).
	if _, err := h.st.DB().Exec(`UPDATE hosts SET state='soft_off' WHERE hostname='h1'`); err != nil {
		t.Fatal(err)
	}
	h.lab.ResetCalls()

	got := h.wait(h.request("h1", ActionPowerup).ID)
	if got.State != store.CmdSucceeded {
		t.Fatalf("command = %+v", got)
	}
	calls := h.lab.Calls()
	if callsMatching(calls, "ipmi.on") != 0 || callsMatching(calls, "pdu.") != 0 {
		t.Fatalf("live chassis was power-cycled or PDU touched: %v", calls)
	}
	if callsMatching(calls, "h1 ssh") == 0 || callsMatching(calls, "h1 ping") == 0 {
		t.Fatalf("ping and SSH still required: %v", calls)
	}
}

func TestPowerupOfOnHostIsAlreadyThere(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateOn)
	res, err := h.m.Request(Request{Hostname: "h2", Action: ActionPowerup, RequestedBy: "jared"})
	if err != nil || res.Outcome != store.OutcomeAlreadyThere || res.Command.State != store.CmdSucceeded {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if len(h.lab.Calls()) != 0 {
		t.Fatalf("hardware touched: %v", h.lab.Calls())
	}
}

func TestPowerdownEscalation(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		script    func(*hw.FakeHost)
		wantState string
		wantCmd   string
		want      []string // calls that must appear
		absent    []string // calls that must not appear
	}{
		{"ssh shutdown works", "h1", func(f *hw.FakeHost) { f.ShutdownWorks = true },
			store.StateSoftOff, store.CmdSucceeded, []string{"h1 ssh.shutdown"}, []string{"ipmi.off", "pdu.1.off"}},
		{"ssh fails, ipmi works", "h1", func(f *hw.FakeHost) { f.ShutdownFails = true },
			store.StateSoftOff, store.CmdSucceeded, []string{"h1 ssh.shutdown", "h1 ipmi.off"}, []string{"pdu.1.off"}},
		{"ssh no-op, ipmi ignored, pdu cuts", "h1", func(f *hw.FakeHost) { f.PowerOffIgnored = true },
			store.StateColdOff, store.CmdSucceeded, []string{"h1 ssh.shutdown", "h1 ipmi.off", "p1 pdu.1.off"}, nil},
		{"everything fails, pdu cut fails too", "h2", func(f *hw.FakeHost) { f.ShutdownFails = true; f.PowerOffIgnored = true },
			store.StateOn, store.CmdFailed, []string{"h2 ssh.shutdown", "h2 ipmi.off"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.lab.Set(tc.host, func(f *hw.FakeHost) { f.Chassis = true; tc.script(f) })
			if tc.wantCmd == store.CmdFailed {
				h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.SetFails = 1000 })
			}
			h.setState(tc.host, store.StateOn)
			c := h.wait(h.request(tc.host, ActionPowerdown).ID)
			if c.State != tc.wantCmd {
				t.Fatalf("command = %+v", c)
			}
			if row := h.host(tc.host); row.State != tc.wantState {
				t.Fatalf("host state = %s", row.State)
			}
			calls := h.lab.Calls()
			last := -1
			for _, w := range tc.want {
				i := indexOf(calls, w)
				if i < 0 || i < last {
					t.Fatalf("want %q after previous step in %v", w, calls)
				}
				last = i
			}
			for _, a := range tc.absent {
				if callsMatching(calls, a) != 0 {
					t.Fatalf("%q must not run: %v", a, calls)
				}
			}
			if tc.wantCmd == store.CmdFailed && c.Error != ErrChassisStillOn {
				t.Fatalf("error = %q", c.Error)
			}
		})
	}
}

func TestPowerdownOfOffHostIsAlreadyThere(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateSoftOff)
	res, err := h.m.Request(Request{Hostname: "h2", Action: ActionPowerdown, RequestedBy: "jared"})
	if err != nil || res.Outcome != store.OutcomeAlreadyThere {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestRebootIPMIOnly(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = true })
	h.setState("h1", store.StateOn)
	c := h.wait(h.request("h1", ActionReboot).ID)
	if c.State != store.CmdSucceeded || c.Escalated {
		t.Fatalf("command = %+v", c)
	}
	calls := h.lab.Calls()
	if callsMatching(calls, "pdu.") != 0 || callsMatching(calls, "ssh.shutdown") != 0 {
		t.Fatalf("unexpected PDU or SSH shutdown: %v", calls)
	}
	if indexOf(calls, "h1 ipmi.off") > indexOf(calls, "h1 ipmi.on") {
		t.Fatalf("order: %v", calls)
	}
	if h.host("h1").State != store.StateOn {
		t.Fatal("want on")
	}
}

func TestRebootChassisStuckOnEscalatesToPDU(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = true; f.PowerOffIgnored = true })
	h.setState("h1", store.StateOn)
	id := h.request("h1", ActionReboot).ID
	c := h.wait(id)
	if c.State != store.CmdSucceeded || !c.Escalated {
		t.Fatalf("command = %+v", c)
	}
	calls := h.lab.Calls()
	order := []string{"h1 ipmi.off", "p1 pdu.1.off", "p1 pdu.1.on", "h1 ipmi.on", "h1 ssh"}
	last := -1
	for _, w := range order {
		i := slices.IndexFunc(calls[last+1:], func(c string) bool { return strings.Contains(c, w) })
		if i < 0 {
			t.Fatalf("want %q in order %v; calls %v", w, order, calls)
		}
		last += i + 1
	}
	if h.host("h1").State != store.StateOn {
		t.Fatal("want on")
	}
	// An escalation event carries escalated=true while the command runs.
	sawEscalation := false
	for ev := range drain(h.sub) {
		if ev.Type != events.TypeCommand {
			continue
		}
		var ce struct{ Command store.Command }
		json.Unmarshal(ev.Data, &ce)
		if ce.Command.ID == id && ce.Command.Escalated && ce.Command.State == store.CmdRunning {
			sawEscalation = true
		}
	}
	if !sawEscalation {
		t.Fatal("no escalation command event")
	}
}

func drain(s *events.Subscription) <-chan events.Event {
	out := make(chan events.Event, 1024)
	for {
		select {
		case ev := <-s.C:
			out <- ev
		default:
			close(out)
			return out
		}
	}
}

func (h *harness) drainEvents() {
	for range drain(h.sub) {
	}
}

// hostEvents counts (and consumes) the queued host events for one host.
func (h *harness) hostEvents(host string) int {
	n := 0
	for ev := range drain(h.sub) {
		var he HostEvent
		if ev.Type == events.TypeHost && json.Unmarshal(ev.Data, &he) == nil && he.Hostname == host {
			n++
		}
	}
	return n
}

// setStateForce is setState for a host already seeded.
func (h *harness) setStateForce(host, state string) {
	h.t.Helper()
	row := h.host(host)
	if row.State == state {
		return
	}
	h.setState(host, state)
}

func TestRebootFromOffRunsOnlyPowerOnHalf(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateSoftOff)
	c := h.wait(h.request("h2", ActionReboot).ID)
	if c.State != store.CmdSucceeded {
		t.Fatalf("command = %+v", c)
	}
	calls := h.lab.Calls()
	if callsMatching(calls, "ipmi.off") != 0 {
		t.Fatalf("power-off half ran: %v", calls)
	}
	if callsMatching(calls, "h2 ipmi.on") == 0 {
		t.Fatalf("power-on half missing: %v", calls)
	}
}

func TestRebootFailureAfterPowerOnEndsSoftOff(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.Chassis = true; f.SSHDown = true })
	h.setState("h2", store.StateOn)
	c := h.wait(h.request("h2", ActionReboot).ID)
	if c.State != store.CmdFailed || c.Error != ErrSSHDown {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h2"); row.State != store.StateSoftOff || row.PowerFailedAt == nil {
		t.Fatalf("host = %+v", row)
	}
}

func TestEnqueueSignalsImmediatelyAndPublishes(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateSoftOff)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	start := time.Now()
	c := h.request("h2", ActionPowerup)
	if c.State != store.CmdQueued {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h2"); row.State != store.StatePoweringOn || row.Attempt != c.Attempt {
		t.Fatalf("enqueue did not set transitional state and attempt: %+v", row)
	}
	for {
		got, _ := h.st.GetCommand(c.ID)
		if got.State != store.CmdQueued {
			break
		}
		if time.Since(start) > 100*time.Millisecond {
			t.Fatal("not claimed within 100ms")
		}
		time.Sleep(time.Millisecond)
	}
	var types []string
	timeout := time.After(time.Second)
	for len(types) < 3 {
		select {
		case ev := <-h.sub.C:
			types = append(types, ev.Type)
			if ev.Type == events.TypeHost {
				var he HostEvent
				json.Unmarshal(ev.Data, &he)
				if he.ActiveCommand == nil || he.ActiveCommand.ID != c.ID || he.State != store.StatePoweringOn {
					t.Fatalf("host event = %s", ev.Data)
				}
			}
		case <-timeout:
			t.Fatalf("events = %v", types)
		}
	}
	if !slices.Equal(types, []string{events.TypeCommand, events.TypeHost, events.TypeCommand}) {
		t.Fatalf("event order = %v (enqueue command, host, claim command)", types)
	}
	h.wait(c.ID)
}

func TestRequestErrors(t *testing.T) {
	h := newHarness(t)
	if _, err := h.m.Request(Request{Hostname: "nope", Action: ActionPowerup}); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("unknown host: %v", err)
	}
	if _, err := h.m.Request(Request{Hostname: "h2", Action: "explode"}); !errors.Is(err, ErrBadAction) {
		t.Fatalf("bad action: %v", err)
	}
	if _, err := h.m.Request(Request{Hostname: "h3", Action: ActionPowerup}); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked: %v", err)
	}
	res, err := h.m.Request(Request{Hostname: "h3", Action: ActionPowerup, Force: true, RequestedBy: "jared"})
	if err != nil || res.Outcome != store.OutcomeQueued || !res.Command.Force {
		t.Fatalf("forced: %+v %v", res, err)
	}
	// Different action while in flight.
	h.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	h.setState("h2", store.StateSoftOff)
	first := h.request("h2", ActionPowerup)
	res, err = h.m.Request(Request{Hostname: "h2", Action: ActionReboot})
	if !errors.Is(err, ErrInFlight) || res.Command.ID != first.ID {
		t.Fatalf("conflict: %+v %v", res, err)
	}
	h.wait(first.ID)
	h.wait(res.Command.ID)
}

func TestStaleAttemptTerminalWriteIsDropped(t *testing.T) {
	h := newHarness(t)
	h.setState("h2", store.StateSoftOff)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	c := h.request("h2", ActionPowerup)
	// Supersede the attempt while the sequence waits for SSH.
	for {
		got, _ := h.st.GetCommand(c.ID)
		if got.State == store.CmdRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := h.st.Recover(c.ID, store.StateRebooting, false, "superseded-by-test", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	h.idle("h2")
	row := h.host("h2")
	if row.State != store.StateRebooting || row.LastError != "superseded-by-test" {
		t.Fatalf("stale terminal write landed: %+v", row)
	}
}

func TestRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// First process: a running powerup on h1, a running powerdown on h2 and a
	// queued powerup on h3, then "crash".
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	far := now.Add(15 * time.Minute)
	for _, x := range []struct{ host, state, action string }{
		{"h1", store.StateSoftOff, ActionPowerup}, {"h2", store.StateOn, ActionPowerdown}, {"h3", store.StateSoftOff, ActionPowerup},
	} {
		st.EnsureHost(x.host, now)
		st.Seed(x.host, x.state, now)
		if _, err := st.Enqueue(store.EnqueueParams{Hostname: x.host, Action: x.action, RequestedBy: "pw", Now: now, Deadline: far}); err != nil {
			t.Fatal(err)
		}
	}
	st.ClaimNext("h1", now)
	st.ClaimNext("h2", now)
	st.Close()

	h := newHarness(t, withPath(path), noStart())
	h.lab.Set("h2", func(f *hw.FakeHost) { f.Chassis = true })
	if err := h.m.Start(h.ctx); err != nil {
		t.Fatal(err)
	}

	h1 := h.host("h1")
	if h1.State != store.StateUnknown || h1.PowerFailedAt == nil || h1.LastError != ErrInterrupted {
		t.Fatalf("h1 = %+v", h1)
	}
	cmds, _ := h.st.ListCommands(store.CommandFilter{Hostname: "h1"})
	if cmds[0].State != store.CmdFailed || cmds[0].Error != ErrInterrupted {
		t.Fatalf("h1 command = %+v", cmds[0])
	}

	h2 := h.host("h2")
	if h2.State != store.StateUnknown || h2.PowerFailedAt != nil {
		t.Fatalf("h2 = %+v", h2)
	}
	// Re-seeded from the next IPMI poll.
	h.m.pollPDU(context.Background(), "p1") // the seed needs a full PDU observation
	h.m.pollIPMI(context.Background(), h.m.hosts["h2"])
	if got := h.host("h2").State; got != store.StateOn {
		t.Fatalf("h2 after poll = %s", got)
	}

	// The queued command was kept and the claim loop ran it.
	q, _ := h.st.ListCommands(store.CommandFilter{Hostname: "h3"})
	if got := h.wait(q[0].ID); got.State != store.CmdSucceeded {
		t.Fatalf("queued command = %+v", got)
	}
}

func TestDeadlineCancelsThroughContext(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, withClock(fc), func(_ *harness, o *Options) {
		o.Config.Timings.PingSSHWait = Duration(time.Minute) // sequence would wait a long time
	})
	h.setState("h2", store.StateSoftOff)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	c := h.request("h2", ActionPowerup)
	for {
		got, _ := h.st.GetCommand(c.ID)
		if got.State == store.CmdRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.m.CheckDeadlines() // not past deadline: nothing happens
	time.Sleep(20 * time.Millisecond)
	if got, _ := h.st.GetCommand(c.ID); got.State != store.CmdRunning {
		t.Fatalf("cancelled early: %+v", got)
	}
	fc.Advance(time.Hour) // past the recorded deadline (budget + slack)
	h.m.CheckDeadlines()
	got := h.wait(c.ID)
	if got.State != store.CmdFailed || got.Error != ErrDeadlineExceeded {
		t.Fatalf("command = %+v", got)
	}
	if row := h.host("h2"); row.State != store.StateSoftOff || row.PowerFailedAt == nil {
		t.Fatalf("host = %+v", row)
	}
}

func TestRecoveryCancelsOnlyViaContextAndRecoversOrphans(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, withClock(fc), func(_ *harness, o *Options) {
		o.Config.Timings.PingSSHWait = Duration(time.Minute)
	})
	h.setState("h2", store.StateSoftOff)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	c := h.request("h2", ActionPowerup)
	for {
		got, _ := h.st.GetCommand(c.ID)
		if got.State == store.CmdRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Pull only this command's recorded deadline into the past: recovery
	// must cancel through the context, not write state itself.
	if _, err := h.st.DB().Exec(`UPDATE commands SET deadline='2000-01-01T00:00:00.000000000Z' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	h.m.CheckDeadlines()
	got := h.wait(c.ID)
	if got.Error != ErrDeadlineExceeded {
		t.Fatalf("goroutine should write deadline_exceeded, got %+v", got)
	}

	// An orphan: running with no goroutine (h1's runner is idle).
	enq, _ := h.st.Enqueue(store.EnqueueParams{Hostname: "h1", Action: ActionPowerdown, RequestedBy: "x", Now: fc.Now(), Deadline: fc.Now().Add(time.Second)})
	h.idle("h1")
	if _, err := h.st.ClaimNext("h1", fc.Now()); err != nil {
		t.Fatal(err)
	}
	fc.Advance(2 * time.Second)
	h.m.CheckDeadlines()
	o, _ := h.st.GetCommand(enq.Command.ID)
	if o.State != store.CmdFailed || o.Error != ErrInterrupted || h.host("h1").State != store.StateUnknown {
		t.Fatalf("orphan = %+v host = %+v", o, h.host("h1"))
	}
}

func TestPollersNeverWriteState(t *testing.T) {
	h := newHarness(t)
	// Every host's chassis disagrees with its stored state.
	h.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = false })
	h.lab.Set("h2", func(f *hw.FakeHost) { f.Chassis = true })
	h.setState("h1", store.StateOn)
	h.setState("h2", store.StateSoftOff)
	if _, err := h.st.DB().Exec(`UPDATE hosts SET state='powering_on', attempt='a3' WHERE hostname='h3'`); err != nil {
		t.Fatal(err)
	}
	before := map[string]store.Host{}
	for _, n := range []string{"h1", "h2", "h3"} {
		before[n] = h.host(n)
	}
	for i := 0; i < 3; i++ {
		h.pollAll()
	}
	for n, b := range before {
		if a := h.host(n); a != b {
			t.Fatalf("poller wrote %s: %+v -> %+v", n, b, a)
		}
	}
	// Status is updated in memory.
	st := h.m.hosts["h2"].statusCopy()
	if st.IPMI == nil || !*st.IPMI || st.Ping == nil || !*st.Ping || st.ObservedAt == nil {
		t.Fatalf("h2 status = %+v", st)
	}
	if st1 := h.m.hosts["h1"].statusCopy(); st1.PDU == nil || !*st1.PDU {
		t.Fatalf("h1 pdu status = %+v", st1)
	}
	if st2 := h.m.hosts["h2"].statusCopy(); st2.PDU == nil || !*st2.PDU {
		t.Fatalf("h2 pdu status = %+v", st2)
	}
}

func TestSeedUnknownFromFirstObservation(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h2", func(f *hw.FakeHost) { f.Chassis = true; f.SSHDown = true })
	if h.host("h2").State != store.StateUnknown || h.host("h1").State != store.StateUnknown {
		t.Fatal("new hosts start unknown")
	}
	h.pollAll()
	// Chassis on seeds on without requiring ping or SSH.
	if got := h.host("h2").State; got != store.StateOn {
		t.Fatalf("h2 = %s", got)
	}
	// Outlets on, chassis off: soft_off.
	if got := h.host("h1").State; got != store.StateSoftOff {
		t.Fatalf("h1 = %s", got)
	}
}

func TestSeedWaitsForFullPDUObservation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.m.pollIPMI(ctx, h.m.hosts["h1"])
	if got := h.host("h1").State; got != store.StateUnknown {
		t.Fatalf("seeded from IPMI alone: %s", got)
	}
	h.m.pollPDU(ctx, "p1")
	if got := h.host("h1").State; got != store.StateSoftOff {
		t.Fatalf("after pdu poll h1 = %s", got)
	}
}

func TestSeedDeadBMCAndCutOutletsIsColdOff(t *testing.T) {
	h := newHarness(t)
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[1] = false })
	h.pollAll()
	if got := h.host("h1").State; got != store.StateColdOff {
		t.Fatalf("h1 = %s", got)
	}
	if st := h.m.hosts["h1"].statusCopy(); st.IPMI != nil || st.PDU == nil || *st.PDU {
		t.Fatalf("h1 status = %+v", st)
	}
}

func TestSeedStaysUnknownWhenOutletsOnAndBMCSilent(t *testing.T) {
	h := newHarness(t)
	// h2: outlets on but the BMC does not answer (X10SDV02, vocal-dove).
	h.lab.Set("h2", func(f *hw.FakeHost) { f.ReadErrors = 1000 })
	// h3: mixed outlets; its BMC is unpowered, so it does not answer either.
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[3] = false })
	for i := 0; i < 3; i++ {
		h.pollAll()
	}
	for _, n := range []string{"h2", "h3"} {
		if got := h.host(n).State; got != store.StateUnknown {
			t.Fatalf("%s = %s, want unknown", n, got)
		}
	}
}

func TestSeedFromChassisBitWithMixedOutlets(t *testing.T) {
	h := newHarness(t)
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[3] = false; p.Outlets[4] = true })
	h.pollAll()
	// The fake BMC is unpowered with a cut outlet, so h3 stays unknown...
	if got := h.host("h3").State; got != store.StateUnknown {
		t.Fatalf("h3 = %s", got)
	}
	// ...until every outlet is back and the chassis bit answers.
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[3] = true })
	h.lab.Set("h3", func(f *hw.FakeHost) { f.Chassis = true })
	h.pollAll()
	if got := h.host("h3").State; got != store.StateOn {
		t.Fatalf("h3 = %s", got)
	}
}

func TestIPMIPollErrorClearsStatusAndPublishes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r := h.m.hosts["h1"]
	h.m.pollIPMI(ctx, r)
	if st := r.statusCopy(); st.IPMI == nil {
		t.Fatalf("status = %+v", st)
	}
	h.drainEvents()
	h.lab.Set("h1", func(f *hw.FakeHost) { f.ReadErrors = 1000 })
	h.m.pollIPMI(ctx, r)
	if st := r.statusCopy(); st.IPMI != nil {
		t.Fatalf("ipmi must be nil after a failed poll: %+v", st)
	}
	if n := h.hostEvents("h1"); n != 1 {
		t.Fatalf("host events on change = %d", n)
	}
	h.m.pollIPMI(ctx, r) // no change, no event
	if n := h.hostEvents("h1"); n != 0 {
		t.Fatalf("host events without change = %d", n)
	}
}

func TestHostViewHasNoSecrets(t *testing.T) {
	h := newHarness(t)
	h.pollAll()
	d, err := h.m.Host("h1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Budgets["powerup"] == "" || d.Class != "" {
		t.Fatalf("detail = %+v", d)
	}
	list, _ := h.m.Hosts()
	b1, _ := json.Marshal(d)
	b2, _ := json.Marshal(list)
	var evs []string
	for ev := range drain(h.sub) {
		evs = append(evs, string(ev.Data))
	}
	all := string(b1) + string(b2) + strings.Join(evs, "")
	for _, secret := range []string{"10.9.", "s3cret", "admin", "UFJJVktFWQ", "pduadmin", "\"ip\""} {
		if strings.Contains(all, secret) {
			t.Fatalf("leaked %q in %s", secret, all)
		}
	}
}

func TestPowercutFromOnShutsDownThenCutsOutlets(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = true; f.ShutdownWorks = true })
	h.setState("h1", store.StateOn)
	c := h.wait(h.request("h1", ActionPowercut).ID)
	if c.State != store.CmdSucceeded || c.Error != "" {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h1"); row.State != store.StateColdOff || row.LastError != "" {
		t.Fatalf("host = %+v", row)
	}
	if h.lab.Outlet("p1", 1) {
		t.Fatal("outlet still on")
	}
	calls := h.lab.Calls()
	if i, j := indexOf(calls, "h1 ssh.shutdown"), indexOf(calls, "p1 pdu.1.off"); i < 0 || j < i {
		t.Fatalf("shutdown must precede the cut: %v", calls)
	}
}

func TestPowercutFromSoftOffCutsOutletsOnly(t *testing.T) {
	h := newHarness(t)
	h.setState("h1", store.StateSoftOff)
	c := h.wait(h.request("h1", ActionPowercut).ID)
	if c.State != store.CmdSucceeded {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h1"); row.State != store.StateColdOff {
		t.Fatalf("host = %+v", row)
	}
	calls := h.lab.Calls()
	if callsMatching(calls, "p1 pdu.1.off") == 0 || callsMatching(calls, "ssh.shutdown") != 0 || callsMatching(calls, "ipmi.off") != 0 {
		t.Fatalf("only the cut should run: %v", calls)
	}
}

func TestPowercutFailureFromSoftOffReturnsSoftOff(t *testing.T) {
	h := newHarness(t)
	h.setState("h1", store.StateSoftOff)
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.SetFails = 1000 })
	c := h.wait(h.request("h1", ActionPowercut).ID)
	if c.State != store.CmdFailed || c.Error != ErrPDUOffFailed {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h1"); row.State != store.StateSoftOff || row.LastError != ErrPDUOffFailed {
		t.Fatalf("host = %+v", row)
	}
}

func TestPowercutFromOnFailingBeforeChassisOffEndsOn(t *testing.T) {
	h := newHarness(t)
	// The chassis ignores power-off and the PDU cut fails, so it never goes off.
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.SetFails = 1000 })
	h.lab.Set("h2", func(f *hw.FakeHost) { f.Chassis = true; f.ShutdownFails = true; f.PowerOffIgnored = true })
	h.setState("h2", store.StateOn)
	c := h.wait(h.request("h2", ActionPowercut).ID)
	if c.State != store.CmdFailed || c.Error != ErrChassisStillOn {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h2"); row.State != store.StateOn {
		t.Fatalf("host = %+v", row)
	}
}

func TestPowercutFromOnFailingAfterChassisOffEndsSoftOff(t *testing.T) {
	h := newHarness(t)
	h.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = true; f.ShutdownWorks = true })
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.SetFails = 1000 })
	h.setState("h1", store.StateOn)
	c := h.wait(h.request("h1", ActionPowercut).ID)
	if c.State != store.CmdFailed || c.Error != ErrPDUOffFailed {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h1"); row.State != store.StateSoftOff || row.LastError != ErrPDUOffFailed {
		t.Fatalf("host = %+v", row)
	}
}

func TestPowercutOfColdOffIsAlreadyThere(t *testing.T) {
	h := newHarness(t)
	h.setState("h1", store.StateColdOff)
	res, err := h.m.Request(Request{Hostname: "h1", Action: ActionPowercut, RequestedBy: "jared"})
	if err != nil || res.Outcome != store.OutcomeAlreadyThere || res.Command.State != store.CmdSucceeded {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if len(h.lab.Calls()) != 0 {
		t.Fatalf("hardware touched: %v", h.lab.Calls())
	}
}

func TestPowerdownOfColdOffIsAlreadyThere(t *testing.T) {
	h := newHarness(t)
	h.setState("h1", store.StateColdOff)
	res, err := h.m.Request(Request{Hostname: "h1", Action: ActionPowerdown, RequestedBy: "jared"})
	if err != nil || res.Outcome != store.OutcomeAlreadyThere {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestPowerupFromColdOffTurnsOutletsOn(t *testing.T) {
	h := newHarness(t)
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[1] = false })
	h.setState("h1", store.StateColdOff)
	c := h.wait(h.request("h1", ActionPowerup).ID)
	if c.State != store.CmdSucceeded {
		t.Fatalf("command = %+v", c)
	}
	if !h.lab.Outlet("p1", 1) {
		t.Fatal("outlet not turned on")
	}
	if row := h.host("h1"); row.State != store.StateOn {
		t.Fatalf("host = %+v", row)
	}
}

func TestRebootFromColdOffTurnsOutletsOn(t *testing.T) {
	h := newHarness(t)
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[1] = false })
	h.setState("h1", store.StateColdOff)
	c := h.wait(h.request("h1", ActionReboot).ID)
	if c.State != store.CmdSucceeded || !h.lab.Outlet("p1", 1) || h.host("h1").State != store.StateOn {
		t.Fatalf("command = %+v host = %+v", c, h.host("h1"))
	}
}

func TestFailedPowerupEndsInOffStateMatchingOutlets(t *testing.T) {
	// Outlets cannot be turned on and are all off: cold_off.
	h := newHarness(t)
	h.lab.SetPDU("p1", func(p *hw.FakePDU) { p.Outlets[1] = false; p.SetFails = 1000 })
	h.setState("h1", store.StateColdOff)
	c := h.wait(h.request("h1", ActionPowerup).ID)
	if c.State != store.CmdFailed || c.Error != ErrPDUOnFailed {
		t.Fatalf("command = %+v", c)
	}
	if row := h.host("h1"); row.State != store.StateColdOff || row.PowerFailedAt == nil {
		t.Fatalf("host = %+v", row)
	}

	// Outlets on and the host fails later (SSH): soft_off.
	h2 := newHarness(t)
	h2.lab.Set("h1", func(f *hw.FakeHost) { f.SSHDown = true })
	h2.setState("h1", store.StateSoftOff)
	c = h2.wait(h2.request("h1", ActionPowerup).ID)
	if c.State != store.CmdFailed || c.Error != ErrSSHDown {
		t.Fatalf("command = %+v", c)
	}
	if row := h2.host("h1"); row.State != store.StateSoftOff {
		t.Fatalf("host = %+v", row)
	}
}

func TestRecoveryOfInterruptedPowercutIsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	st.EnsureHost("h1", now)
	st.Seed("h1", store.StateSoftOff, now)
	if _, err := st.Enqueue(store.EnqueueParams{Hostname: "h1", Action: ActionPowercut, RequestedBy: "pw", Now: now, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	st.ClaimNext("h1", now)
	st.Close()
	h := newHarness(t, withPath(path), noStart())
	if err := h.m.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	if row := h.host("h1"); row.State != store.StateUnknown || row.PowerFailedAt != nil || row.LastError != ErrInterrupted {
		t.Fatalf("h1 = %+v", row)
	}
}

// slowPDUProvider returns PDU clients that ignore their context and block for
// delay, like the real Eaton and Kasa clients (hw/real.go OutletStates).
type slowPDUProvider struct {
	hw.Provider
	delay time.Duration
}

func (p slowPDUProvider) PDU(pdu inventory.Pdu) (hw.PDU, error) {
	inner, err := p.Provider.PDU(pdu)
	if err != nil {
		return nil, err
	}
	return slowPDU{PDU: inner, delay: p.delay}, nil
}

type slowPDU struct {
	hw.PDU
	delay time.Duration
}

func (s slowPDU) OutletStates(_ context.Context) (map[int]bool, error) {
	time.Sleep(s.delay)
	return s.PDU.OutletStates(context.Background())
}

// Pins poller.go's loop/pollPDU note: the loop gives each poll a context
// timeout equal to the interval, and a PDU read slower than that still lands
// because the PDU clients ignore the context. Scaled down from an 8s Eaton
// read against a 2s interval. If this fails, the PDU poll needs its own
// timeout before the clients honour the context.
func TestSlowPDUReadLongerThanIntervalStillUpdatesStatus(t *testing.T) {
	cfg := fastConfig()
	cfg.Polling = PollingConfig{
		IPMIInterval: Duration(time.Hour), NetworkInterval: Duration(time.Hour),
		PDUInterval: Duration(20 * time.Millisecond),
	}
	h := newHarness(t, withPollers(), func(_ *harness, o *Options) { o.Config = cfg },
		wrapHW(func(p hw.Provider) hw.Provider { return slowPDUProvider{Provider: p, delay: 100 * time.Millisecond} }))
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := h.m.hosts["h1"].statusCopy()
		if st.PDU != nil && *st.PDU {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status.pdu never updated: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
