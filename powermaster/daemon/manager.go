package powermaster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/resurgentech/powerwarden/powermaster/daemon/hw"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/clock"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// Request errors.
var (
	ErrUnknownHost = errors.New("not_found")
	ErrLocked      = errors.New("host_locked")
	ErrInFlight    = errors.New("command_in_flight")
	ErrBadAction   = errors.New("bad_request")
)

// Errors recorded on failed commands.
const (
	ErrInterrupted            = "interrupted"
	ErrDeadlineExceeded       = "deadline_exceeded"
	ErrChassisDidNotPowerOff  = "chassis_did_not_power_off"
	ErrChassisDidNotPowerOn   = "chassis_did_not_power_on"
	ErrChassisStillOn         = "chassis_still_on"
	ErrIPMIUnreachable        = "ipmi_unreachable"
	ErrPDUOnFailed            = "pdu_on_failed"
	ErrPDUOffFailed           = "pdu_off_failed"
	ErrChassisOffAfterPowerOn = "chassis_off"
	ErrPingDown               = "ping_down"
	ErrSSHDown                = "ssh_down"
)

// errDeadline is the cancel cause when a command's deadline passes.
var errDeadline = errors.New(ErrDeadlineExceeded)

// Options configure a Manager.
type Options struct {
	Config    Config
	Inventory *inventory.Inventory
	Store     *store.Store
	HW        hw.Provider
	Hub       *events.Hub
	Clock     clock.Clock  // default clock.Real
	Log       *slog.Logger // default slog.Default()

	// Step is the retry/poll step inside a sequence stage. Default 1s.
	Step time.Duration
	// Resend is how long a sent chassis command may go unanswered before it
	// is sent again. Default 30s.
	Resend time.Duration
	// DisablePollers skips the background pollers (tests drive them).
	DisablePollers bool
}

// Manager owns per-host goroutines, pollers and recovery.
type Manager struct {
	cfg   Config
	inv   *inventory.Inventory
	st    *store.Store
	hub   *events.Hub
	clk   clock.Clock
	log   *slog.Logger
	plans map[string]HostPlan
	step  time.Duration
	resnd time.Duration
	noPol bool

	hosts map[string]*hostRunner
	names []string // sorted inventory hostnames
	pdus  map[string]hw.PDU

	ctx context.Context
	wg  sync.WaitGroup
}

type hostRunner struct {
	host inventory.Host
	plan HostPlan
	ipmi hw.IPMI
	net  hw.Net

	mu   sync.Mutex    // at most one sequence per host
	wake chan struct{} // enqueue -> goroutine, buffered 1

	smu     sync.Mutex
	status  Status
	outlets map[string]bool // "pdu/outlet" -> on
	prior   map[int64]string
	curID   int64
	curStop context.CancelCauseFunc
}

// New validates config against inventory, resolves plans and ensures a host
// row for every inventory host.
func New(o Options) (*Manager, error) {
	if o.Inventory == nil || o.Store == nil || o.HW == nil {
		return nil, errors.New("powermaster: inventory, store and hw are required")
	}
	plans, err := Resolve(o.Config, o.Inventory)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg: o.Config, inv: o.Inventory, st: o.Store, hub: o.Hub, clk: o.Clock, log: o.Log,
		plans: plans, step: o.Step, resnd: o.Resend, noPol: o.DisablePollers,
		hosts: map[string]*hostRunner{}, pdus: map[string]hw.PDU{},
	}
	if m.clk == nil {
		m.clk = clock.Real{}
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.hub == nil {
		m.hub = events.NewHub(o.Config.Events.RingSize)
	}
	if m.step <= 0 {
		m.step = time.Second
	}
	if m.resnd <= 0 {
		m.resnd = 30 * time.Second
	}
	for name, p := range o.Inventory.Pdus {
		c, err := o.HW.PDU(p)
		if err != nil {
			return nil, err
		}
		m.pdus[name] = c
	}
	now := m.clk.Now()
	for _, h := range o.Inventory.Hosts {
		if _, err := o.Store.EnsureHost(h.Hostname, now); err != nil {
			return nil, fmt.Errorf("ensure host %s: %w", h.Hostname, err)
		}
		m.hosts[h.Hostname] = &hostRunner{
			host: h, plan: plans[h.Hostname], ipmi: o.HW.IPMI(h), net: o.HW.Net(h),
			wake: make(chan struct{}, 1), outlets: map[string]bool{}, prior: map[int64]string{},
		}
		m.names = append(m.names, h.Hostname)
	}
	sort.Strings(m.names)
	return m, nil
}

// Hub returns the event hub.
func (m *Manager) Hub() *events.Hub { return m.hub }

// Store returns the store.
func (m *Manager) Store() *store.Store { return m.st }

// Plan returns a host's resolved plan.
func (m *Manager) Plan(hostname string) (HostPlan, bool) {
	p, ok := m.plans[hostname]
	return p, ok
}

// Start recovers interrupted commands, then starts per-host goroutines,
// pollers and the recovery loop. It returns once they are running; Wait
// blocks until ctx is cancelled and they have stopped.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx
	if err := m.RecoverAtStart(); err != nil {
		return err
	}
	for _, name := range m.names {
		r := m.hosts[name]
		m.wg.Add(1)
		go m.runHost(ctx, r)
		if !m.noPol {
			m.wg.Add(2)
			go m.loop(ctx, m.cfg.Polling.IPMIInterval.D(), func(c context.Context) { m.pollIPMI(c, r) })
			go m.loop(ctx, m.cfg.Polling.NetworkInterval.D(), func(c context.Context) { m.pollNet(c, r) })
		}
	}
	if !m.noPol {
		for name := range m.pdus {
			name := name
			m.wg.Add(1)
			go m.loop(ctx, m.cfg.Polling.PDUInterval.D(), func(c context.Context) { m.pollPDU(c, name) })
		}
	}
	m.wg.Add(1)
	go m.recoveryLoop(ctx)
	// Queued commands survive a restart; the claim loop starts them now.
	queued, err := m.st.HostsWithQueued()
	if err != nil {
		return err
	}
	for _, h := range queued {
		if r := m.hosts[h]; r != nil {
			r.signal()
		}
	}
	return nil
}

// Wait blocks until every goroutine started by Start has returned.
func (m *Manager) Wait() { m.wg.Wait() }

func (r *hostRunner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Request is one power request.
type Request struct {
	Hostname    string
	Action      string
	Force       bool
	RequestedBy string
	Reason      string
}

// Request enqueues a power action. ErrUnknownHost, ErrLocked, ErrBadAction
// and ErrInFlight (with the conflicting command in the result) map to API
// errors; otherwise Outcome says 202 (queued/existing) or 200 (already there).
func (m *Manager) Request(req Request) (store.EnqueueResult, error) {
	switch req.Action {
	case ActionPowerup, ActionPowerdown, ActionPowercut, ActionReboot:
	default:
		return store.EnqueueResult{}, ErrBadAction
	}
	r, ok := m.hosts[req.Hostname]
	if !ok {
		return store.EnqueueResult{}, ErrUnknownHost
	}
	if r.host.Locked && !req.Force {
		return store.EnqueueResult{}, ErrLocked
	}
	now := m.clk.Now()
	res, err := m.st.Enqueue(store.EnqueueParams{
		Hostname: req.Hostname, Action: req.Action, Force: req.Force,
		RequestedBy: req.RequestedBy, Reason: req.Reason,
		Now: now, Deadline: now.Add(r.plan.Deadline(req.Action)),
	})
	if err != nil {
		return res, err
	}
	c := res.Command
	switch res.Outcome {
	case store.OutcomeConflict:
		return res, ErrInFlight
	case store.OutcomeExisting:
		return res, nil
	case store.OutcomeAlreadyThere:
		m.logCommand(c, "already in requested state")
		m.publishCommand(c)
		return res, nil
	}
	r.smu.Lock()
	r.prior[c.ID] = res.PriorState
	r.smu.Unlock()
	m.logCommand(c, "command queued")
	m.logState(req.Hostname, res.PriorState, transitionalFor(req.Action), "enqueue")
	m.publishCommand(c)
	m.publishHost(req.Hostname)
	r.signal()
	return res, nil
}

func transitionalFor(action string) string {
	switch action {
	case ActionPowerup:
		return store.StatePoweringOn
	case ActionPowerdown, ActionPowercut:
		return store.StatePoweringOff
	}
	return store.StateRebooting
}

// runHost is the per-host goroutine: woken by enqueue, it claims and runs
// queued commands one at a time under the host mutex.
func (m *Manager) runHost(ctx context.Context, r *hostRunner) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		m.drain(ctx, r)
	}
}

func (m *Manager) drain(ctx context.Context, r *hostRunner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ctx.Err() == nil {
		c, err := m.st.ClaimNext(r.host.Hostname, m.clk.Now())
		if err != nil {
			m.log.Error("claim failed", "hostname", r.host.Hostname, "error", err)
			return
		}
		if c == nil {
			return
		}
		m.execute(ctx, r, *c)
	}
}

func (m *Manager) execute(ctx context.Context, r *hostRunner, c store.Command) {
	now := m.clk.Now()
	deadline := now.Add(r.plan.Budgets.For(c.Action))
	if c.Deadline.Before(deadline) {
		deadline = c.Deadline
	}
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.smu.Lock()
	r.curID, r.curStop = c.ID, cancel
	prior, hasPrior := r.prior[c.ID]
	delete(r.prior, c.ID)
	r.smu.Unlock()
	defer func() {
		r.smu.Lock()
		r.curID, r.curStop = 0, nil
		r.smu.Unlock()
	}()
	go func() {
		select {
		case <-m.clk.After(deadline.Sub(now)):
			cancel(errDeadline)
		case <-sctx.Done():
		}
	}()

	m.logCommand(c, "command running")
	m.publishCommand(c)

	s := &sequence{m: m, r: r, cmd: c, ctx: sctx, t: r.plan.Timings, prior: prior, hasPrior: hasPrior}
	var res result
	switch c.Action {
	case ActionPowerup:
		res = s.powerup()
	case ActionPowerdown:
		res = s.powerdown()
	case ActionPowercut:
		res = s.powercut()
	case ActionReboot:
		res = s.reboot()
	}

	if res.err != "" && ctx.Err() != nil {
		// Process shutdown: no terminal write; recovery at the next start
		// handles the running command.
		m.log.Warn("sequence interrupted by shutdown", "command_id", c.ID, "hostname", c.Hostname)
		return
	}
	m.finish(c, res)
}

func (m *Manager) finish(c store.Command, res result) {
	now := m.clk.Now()
	p := store.FinishParams{
		Hostname: c.Hostname, Attempt: c.Attempt, CommandID: c.ID,
		HostState: res.state, LastError: res.err, CommandState: store.CmdSucceeded, Error: res.err, Now: now,
	}
	if res.err != "" {
		p.CommandState = store.CmdFailed
	}
	if res.powerFailed {
		p.PowerFailedAt = &now
	}
	applied, done, err := m.st.Finish(p)
	if err != nil {
		m.log.Error("terminal write failed", "command_id", c.ID, "hostname", c.Hostname, "error", err)
		return
	}
	if !applied {
		m.log.Warn("stale attempt; terminal write dropped", "command_id", c.ID, "hostname", c.Hostname, "attempt", c.Attempt)
		return
	}
	m.logCommand(done, "command finished")
	m.logState(c.Hostname, transitionalFor(c.Action), res.state, "sequence")
	m.publishCommand(done)
	m.publishHost(c.Hostname)
}

// cancelRunning cancels the host's running sequence if it is command id.
func (r *hostRunner) cancelRunning(id int64, cause error) bool {
	r.smu.Lock()
	defer r.smu.Unlock()
	if r.curID != id || r.curStop == nil {
		return false
	}
	r.curStop(cause)
	return true
}

func (m *Manager) logCommand(c store.Command, msg string) {
	attrs := []any{"command_id", c.ID, "hostname", c.Hostname, "action", c.Action, "requested_by", c.RequestedBy,
		"reason", c.Reason, "state", c.State, "escalated", c.Escalated, "error", c.Error}
	if c.State == store.CmdFailed {
		m.log.Error("command failed", attrs...)
		return
	}
	m.log.Info(msg, attrs...)
}

func (m *Manager) logState(host, from, to, writer string) {
	m.log.Info("state write", "hostname", host, "from", from, "to", to, "writer", writer)
}
