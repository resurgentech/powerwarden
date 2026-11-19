// Package reconcile pairs demand with workers and hosts and asks powermaster
// to power hosts up or down.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/clock"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/decisions"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/hostview"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/source"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

const logEvery = time.Minute

// Reconciler runs passes on events and on a timer.
type Reconciler struct {
	Timings config.Timings
	Store   *store.Store
	Source  source.Source
	PM      pm.Client
	View    *hostview.View
	Ring    *decisions.Ring
	Clock   clock.Clock
	Log     *slog.Logger

	// OnPass, if set, runs after every pass (event hub, tests).
	OnPass func(PassResult)

	trigger chan struct{}
	passMu  sync.Mutex

	mu      sync.Mutex
	lastReq *PowerRequest
	last    PassResult
	lastLog map[string]time.Time
	lastSum string
	// lastNameProblems is the last logged duplicate-short-name and
	// stale-explicit-tag summary, so each change is logged once.
	lastNameProblems string
	passes           int
}

// PassResult is what the API and UI read about the last pass.
type PassResult struct {
	At        time.Time
	Output    Output
	Requested []pm.Command // power requests made this pass that succeeded
	Err       error
	// Demands, Workers and Claims are the snapshot the pass planned from
	// (claims after this pass's binding).
	Demands []model.Demand
	Workers []model.Worker
	Claims  []model.Claim
}

// PowerRequest summarizes the last power request (CommandID nil if it failed).
type PowerRequest struct {
	At        time.Time
	Hostname  string
	Action    string
	CommandID *int64
}

// New wires a reconciler.
func New(t config.Timings, st *store.Store, src source.Source, client pm.Client, view *hostview.View,
	ring *decisions.Ring, clk clock.Clock, log *slog.Logger) *Reconciler {
	return &Reconciler{Timings: t, Store: st, Source: src, PM: client, View: view, Ring: ring, Clock: clk, Log: log,
		trigger: make(chan struct{}, 1), lastLog: map[string]time.Time{}}
}

// Trigger requests a pass soon (after a demand or worker write, or an event).
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Last returns the most recent pass result.
func (r *Reconciler) Last() PassResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// LastRequest returns the last power request made, or nil.
func (r *Reconciler) LastRequest() *PowerRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastReq == nil {
		return nil
	}
	cp := *r.lastReq
	return &cp
}

func (r *Reconciler) noteRequest(at time.Time, host, action string, id *int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastReq = &PowerRequest{At: at, Hostname: host, Action: action, CommandID: id}
}

// Passes returns how many passes have run.
func (r *Reconciler) Passes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passes
}

// SyncHosts re-reads GET /v1/hosts into the view.
func (r *Reconciler) SyncHosts(ctx context.Context) error {
	hosts, err := r.PM.ListHosts(ctx)
	if err != nil {
		r.View.MarkUnreachable()
		r.rateLog("pm_unreachable", slog.LevelError, "powermaster host list failed", "error", err)
		return err
	}
	r.View.Replace(hosts, r.Clock.Now())
	return nil
}

// HandleEvent folds a powermaster event into the view and records its effects.
func (r *Reconciler) HandleEvent(ctx context.Context, ev pm.Event) {
	if ev.Type == pm.EventResync {
		_ = r.SyncHosts(ctx)
		return
	}
	r.applyEffects(ctx, r.View.Apply(ev, r.Clock.Now()))
}

func (r *Reconciler) applyEffects(ctx context.Context, effects []hostview.Effect) {
	for _, e := range effects {
		id := e.Command.ID
		switch e.Kind {
		case hostview.EffectBootSucceeded:
			at := e.At
			if err := r.Store.SetLastPowerupOK(ctx, e.Hostname, &at); err != nil {
				r.Log.Error("store last_powerup_ok_at", "host", e.Hostname, "error", err)
			}
			r.Log.Info("powerup succeeded", "host", e.Hostname, "command_id", id)
		case hostview.EffectBootFailed:
			r.Log.Error("powerup failed", "host", e.Hostname, "command_id", id, "error", e.Command.Error)
			r.Ring.Add(decisions.Decision{Ts: r.Clock.Now(), Action: decisions.CooldownStarted, Hostname: e.Hostname,
				CommandID: &id, Detail: fmt.Sprintf("%s failed: %s; cooldown %s", e.Command.Action, e.Command.Error, r.Timings.BootCooldown)})
		}
	}
}

// Pass runs one reconcile pass.
func (r *Reconciler) Pass(ctx context.Context) PassResult {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	res := r.pass(ctx)
	r.mu.Lock()
	r.last = res
	r.passes++
	r.mu.Unlock()
	if r.OnPass != nil {
		r.OnPass(res)
	}
	return res
}

func (r *Reconciler) pass(ctx context.Context) PassResult {
	now := r.Clock.Now()
	t := r.Timings
	res := PassResult{At: now}

	// 1. Load and sweep.
	if nd, nw, err := r.Store.Sweep(ctx, now, t.DemandSweepAfter, t.BusyGrace+t.WorkerSweepExtra); err != nil {
		r.Log.Error("sweep", "error", err)
	} else if nd+nw > 0 {
		r.Log.Info("swept", "demands", nd, "workers", nw)
	}
	if nc, err := r.Store.SweepClaims(ctx, now, t.ClaimSweepAfter); err != nil {
		r.Log.Error("claim sweep", "error", err)
	} else if nc > 0 {
		r.Log.Info("swept", "claims", nc)
	}
	if err := r.Store.SetLastAlive(ctx, now); err != nil {
		r.Log.Error("store last_alive_at", "error", err)
	}
	snap, err := r.Source.Load(ctx, now)
	if err != nil {
		res.Err = fmt.Errorf("load demands and workers: %w", err)
		r.rateLog("load_failed", slog.LevelError, "reconcile load failed", "error", err)
		return res
	}
	policy, err := r.Store.HostPolicies(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	hosts := r.View.Hosts()
	snap.Claims = r.bindClaims(ctx, now, hosts, policy, snap)
	res.Demands, res.Workers, res.Claims = snap.Demands, snap.Workers, snap.Claims
	lastReq, err := r.Store.LastPowerupRequest(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	out := Plan(Input{Claims: snap.Claims,
		Now: now, Timings: t, Hosts: hosts, Policy: policy,
		Demands: snap.Demands, Workers: snap.Workers, LastPowerupRequest: lastReq,
		ViewFresh: r.View.Fresh(now, t.HostViewRefresh),
	})

	r.logNameProblems(policy)

	for _, id := range out.UnknownHostWorkers {
		r.rateLog("unknown_host:"+id, slog.LevelError, "liveness hostname not in powermaster inventory", "worker", id)
	}
	for _, id := range out.TagMismatchWorkers {
		r.rateLog("tag_mismatch:"+id, slog.LevelError, "liveness tags differ from host tags", "worker", id)
	}
	for _, id := range out.NoCandidate {
		if r.rateOK("no_candidate:"+id, now) {
			r.Log.Warn("no candidate host for uncovered demand", "demand", id)
			r.Ring.Add(decisions.Decision{Ts: now, Action: decisions.NoCandidate, DemandID: id,
				Detail: "uncovered and no off, unlocked, unexcluded, cooled-down host covers its tags"})
		}
	}
	for _, h := range out.BootGraceExpired {
		r.Log.Warn("boot grace expired with no worker", "host", h)
		r.Ring.Add(decisions.Decision{Ts: now, Action: decisions.BootGraceExpired, Hostname: h,
			Detail: fmt.Sprintf("no live worker within %s of powerup", t.BootGrace)})
	}
	for _, h := range out.ClearPowerupOK {
		if err := r.Store.SetLastPowerupOK(ctx, h, nil); err != nil {
			r.Log.Error("clear last_powerup_ok_at", "host", h, "error", err)
		}
	}
	for h, since := range out.IdleSince {
		old := policy[h].IdleSince
		if (old == nil) != (since == nil) || (old != nil && !old.Equal(*since)) {
			if err := r.Store.SetIdleSince(ctx, h, since); err != nil {
				r.Log.Error("store idle_since", "host", h, "error", err)
			}
		}
	}

	// 6/7. One powerup per pass; a failed request skips that host this pass.
	requested := false
	for _, a := range out.PowerupAttempts {
		requested = true
		reason := a.DemandID
		if a.ClaimID != "" {
			reason = ClaimReason(a.ClaimID)
		}
		cmd, err := r.PM.Power(ctx, a.Hostname, pm.ActionPowerup, reason)
		if err != nil {
			if a.ClaimID != "" {
				err = fmt.Errorf("for claim %s: %w", a.ClaimID, err)
			}
			r.requestFailed(now, a.Hostname, a.DemandID, pm.ActionPowerup, err)
			continue
		}
		res.Requested = append(res.Requested, cmd)
		r.requestOK(ctx, now, cmd, a.DemandID, a.ClaimID)
		if err := r.Store.SetLastPowerupRequest(ctx, now); err != nil {
			r.Log.Error("store last_powerup_request", "error", err)
		}
		hs := out.Hosts[a.Hostname]
		hs.Booting = cmd.Active()
		if a.DemandID != "" {
			out.CoveredBy[a.DemandID] = "host:" + a.Hostname
			hs.PairedDemand = a.DemandID
		}
		out.Hosts[a.Hostname] = hs
		break
	}

	// 8-11. One power-off per pass, only when no powerup was requested:
	// the idle powerdown (10) first, else the cold-off powercut (11).
	if !requested && out.Powerdown == "" && out.Powercut != "" {
		h := out.Powercut
		cmd, err := r.PM.Power(ctx, h, pm.ActionPowercut, fmt.Sprintf("soft_off %s", t.ColdOffAfter))
		if err != nil {
			r.requestFailed(now, h, "", pm.ActionPowercut, err)
		} else {
			res.Requested = append(res.Requested, cmd)
			r.requestOK(ctx, now, cmd, "", "")
		}
	}
	if !requested && out.Powerdown != "" {
		h := out.Powerdown
		cmd, err := r.PM.Power(ctx, h, pm.ActionPowerdown, fmt.Sprintf("idle %s", t.MinIdleTime))
		if err != nil {
			r.requestFailed(now, h, "", pm.ActionPowerdown, err)
		} else {
			res.Requested = append(res.Requested, cmd)
			r.requestOK(ctx, now, cmd, "", "")
		}
	}

	res.Output = out
	r.summary(now, out)
	return res
}

func (r *Reconciler) requestOK(ctx context.Context, now time.Time, cmd pm.Command, demandID, claimID string) {
	id := cmd.ID
	r.noteRequest(now, cmd.Hostname, cmd.Action, &id)
	action := decisions.PowerupRequested
	switch cmd.Action {
	case pm.ActionPowerdown:
		action = decisions.PowerdownRequested
	case pm.ActionPowercut:
		action = decisions.PowercutRequested
	}
	detail := "command " + cmd.State
	if claimID != "" {
		detail = "for claim " + claimID + ": " + detail
	}
	r.Log.Info("power request", "action", cmd.Action, "host", cmd.Hostname, "demand", demandID, "claim", claimID,
		"command_id", id, "command_state", cmd.State)
	r.Ring.Add(decisions.Decision{Ts: now, Action: action, Hostname: cmd.Hostname, DemandID: demandID,
		CommandID: &id, Detail: detail})
	if err := r.Store.RecordRequest(ctx, cmd.Hostname, id, now); err != nil {
		r.Log.Error("store last request", "host", cmd.Hostname, "error", err)
	}
	r.applyEffects(ctx, r.View.NoteCommand(cmd, now))
}

func (r *Reconciler) requestFailed(now time.Time, host, demandID, action string, err error) {
	r.noteRequest(now, host, action, nil)
	var apiErr *pm.APIError
	level := slog.LevelError
	if errors.As(err, &apiErr) && apiErr.Status == 409 {
		level = slog.LevelWarn
	} else if !errors.As(err, &apiErr) {
		r.View.MarkUnreachable()
	}
	r.Log.Log(context.Background(), level, "power request failed", "action", action, "host", host, "demand", demandID, "error", err)
	r.Ring.Add(decisions.Decision{Ts: now, Action: decisions.RequestFailed, Hostname: host, DemandID: demandID,
		Detail: action + ": " + err.Error()})
}

func (r *Reconciler) summary(now time.Time, out Output) {
	covered, uncovered := 0, 0
	for _, by := range out.CoveredBy {
		if by == "" {
			uncovered++
		} else {
			covered++
		}
	}
	sum := fmt.Sprintf("covered=%d uncovered=%d powerup_attempts=%d powerdown=%q", covered, uncovered,
		len(out.PowerupAttempts), out.Powerdown)
	r.mu.Lock()
	changed := sum != r.lastSum
	r.lastSum = sum
	r.mu.Unlock()
	if changed || r.rateOK("summary", now) {
		r.Log.Info("reconcile", "demands_covered", covered, "demands_uncovered", uncovered,
			"powerup_attempts", len(out.PowerupAttempts), "powerdown_candidate", out.Powerdown)
	}
}

// rateOK reports whether key has not fired in the last minute, and marks it.
func (r *Reconciler) rateOK(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastLog[key]; ok && now.Sub(last) < logEvery {
		return false
	}
	r.lastLog[key] = now
	return true
}

func (r *Reconciler) rateLog(key string, level slog.Level, msg string, args ...any) {
	if r.rateOK(key, r.Clock.Now()) {
		r.Log.Log(context.Background(), level, msg, args...)
	}
}

// Run syncs the host view, subscribes to powermaster events, and runs passes
// on triggers, every reconcile_interval, and after each host_view_refresh.
// No pass runs before the first successful GET /v1/hosts.
func (r *Reconciler) Run(ctx context.Context) error {
	t := r.Timings
	r.ApplyRestartGrace(ctx)
	for r.SyncHosts(ctx) != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.Clock.After(t.ReconcileInterval):
		}
	}
	go r.eventLoop(ctx)

	now := r.Clock.Now()
	nextPass, nextRefresh := now, now.Add(t.HostViewRefresh)
	triggered := true
	for {
		now = r.Clock.Now()
		if !now.Before(nextRefresh) {
			_ = r.SyncHosts(ctx)
			nextRefresh = now.Add(t.HostViewRefresh)
			triggered = true
		}
		if triggered || !now.Before(nextPass) {
			r.Pass(ctx)
			nextPass = now.Add(t.ReconcileInterval)
			triggered = false
		}
		wait := nextPass.Sub(now)
		if d := nextRefresh.Sub(now); d < wait {
			wait = d
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.trigger:
			triggered = true
		case <-r.Clock.After(wait):
		}
	}
}

func (r *Reconciler) eventLoop(ctx context.Context) {
	var lastSeq int64
	backoff := time.Second
	for ctx.Err() == nil {
		ch, err := r.PM.Events(ctx, lastSeq)
		if err != nil {
			r.View.MarkUnreachable()
			r.rateLog("events_failed", slog.LevelError, "powermaster event stream failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-r.Clock.After(backoff):
			}
			if backoff *= 2; backoff > r.Timings.ReconcileInterval {
				backoff = r.Timings.ReconcileInterval
			}
			continue
		}
		backoff = time.Second
		for ev := range ch {
			if ev.Seq > 0 {
				lastSeq = ev.Seq
			}
			r.HandleEvent(ctx, ev)
			r.Trigger()
		}
	}
}

// NameProblems summarises name-tag trouble in the current host list: short
// names shared by several hosts, and explicit tags equal to a host name (which
// are ignored). Empty when there is none. Used for logs; the UI reads the
// NameIndex directly.
func NameProblems(idx *NameIndex, hosts []pm.Host, policy map[string]model.HostPolicy) string {
	var parts []string
	dups := idx.Duplicates()
	shorts := make([]string, 0, len(dups))
	for s := range dups {
		shorts = append(shorts, s)
	}
	sort.Strings(shorts)
	for _, s := range shorts {
		parts = append(parts, fmt.Sprintf("duplicate short name %q on %s", s, strings.Join(dups[s], ", ")))
	}
	for _, h := range hosts {
		if st := idx.Stale(h.Hostname, policy[h.Hostname].Tags); len(st) > 0 {
			parts = append(parts, fmt.Sprintf("host %s: explicit tags equal to a host name are ignored: %s", h.Hostname, strings.Join(st, ", ")))
		}
	}
	return strings.Join(parts, "; ")
}

// logNameProblems logs one error each time the set of name-tag problems changes.
func (r *Reconciler) logNameProblems(policy map[string]model.HostPolicy) {
	hosts := r.View.Hosts()
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Hostname)
	}
	sum := NameProblems(NewNameIndex(names), hosts, policy)
	r.mu.Lock()
	changed := sum != r.lastNameProblems
	r.lastNameProblems = sum
	r.mu.Unlock()
	if changed && sum != "" {
		r.Log.Error("host name tags: " + sum)
	}
}

// ClaimReason is the powermaster reason of a powerup powerwarden requests
// for a claim.
func ClaimReason(id string) string { return "claim:" + id }

// bindClaims re-evaluates the live claims against this pass's host snapshot
// (store.BindClaims, the same transaction a PUT uses) and returns the
// snapshot's claims with the changes applied. Before the first host sync
// the inventory is unknown and unbound claims stay pending.
func (r *Reconciler) bindClaims(ctx context.Context, now time.Time, hosts []pm.Host,
	policy map[string]model.HostPolicy, snap source.Snapshot) []model.Claim {
	var bh []store.BindHost
	if r.View.Synced() {
		bh = BindHosts(now, r.Timings, hosts, policy, snap.Workers)
	}
	changed, err := r.Store.BindClaims(ctx, bh, now)
	if err != nil {
		r.Log.Error("claim bind", "error", err)
		return snap.Claims
	}
	if len(changed) == 0 {
		return snap.Claims
	}
	byID := make(map[string]model.Claim, len(changed))
	for _, c := range changed {
		byID[c.ID] = c
		r.Log.Info("claim", "id", c.ID, "owner", c.Owner, "mode", c.Mode, "state", c.State,
			"host", c.Hostname, "detail", c.Detail)
	}
	out := make([]model.Claim, len(snap.Claims))
	for i, c := range snap.Claims {
		if n, ok := byID[c.ID]; ok {
			c = n
		}
		out[i] = c
	}
	return out
}

// ApplyRestartGrace gives each non-detached claim whose lease ran out while
// powerwarden was down (after the last pass, settings.last_alive_at) a lease
// of claim_restart_grace from now, capped at its expires_at. Run calls it
// once before the first pass.
func (r *Reconciler) ApplyRestartGrace(ctx context.Context) {
	n, err := r.Store.ApplyRestartGrace(ctx, r.Clock.Now(), r.Timings.ClaimRestartGrace)
	if err != nil {
		r.Log.Error("claim restart grace", "error", err)
		return
	}
	if n > 0 {
		r.Log.Info("claim restart grace", "claims", n, "grace", r.Timings.ClaimRestartGrace)
	}
}

// NoteCommand folds a command that did not come from a pass (a claim holder's
// power request) into the view and the host's last-request record, so the
// next pass and claim binding see it in flight before its event arrives.
func (r *Reconciler) NoteCommand(ctx context.Context, cmd pm.Command) {
	now := r.Clock.Now()
	if err := r.Store.RecordRequest(ctx, cmd.Hostname, cmd.ID, now); err != nil {
		r.Log.Error("store last request", "host", cmd.Hostname, "error", err)
	}
	r.applyEffects(ctx, r.View.NoteCommand(cmd, now))
}

// MarkPowermasterUnreachable records that a request to powermaster failed
// without an API answer, as a failed pass request does.
func (r *Reconciler) MarkPowermasterUnreachable() { r.View.MarkUnreachable() }
