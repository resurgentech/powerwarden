// Package app is the operation layer shared by the HTTP API and the web UI.
// Both call these functions; neither talks to powermaster (only the
// reconciler does), and every write triggers a reconcile pass and an event.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/clock"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/decisions"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/hostview"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/reconcile"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// App ties the store, reconciler, host view, decisions, and event hub together.
type App struct {
	Cfg   config.Config
	Store *store.Store
	Rec   *reconcile.Reconciler
	View  *hostview.View
	Ring  *decisions.Ring
	Hub   *events.Hub
	Clock clock.Clock
	Log   *slog.Logger

	mu         sync.Mutex
	prevDem    map[string]string // id -> demandKey
	prevWork   map[string]string // fresh worker id -> workerKey
	prevClaims map[string]string // live claim id -> claimKey
	prevHosts  map[string]string // hostname -> encoded api.HostView

	claimWake    map[string]chan struct{} // claim id -> closed on its next version change
	claimWaiters map[string]int           // claim id -> parked blocking reads
	waitWakes    int64
	claimVer     map[string]int64  // claim id -> version at its last publish
	hostSeen     map[string]string // claim id -> hostKey at its last publish
}

// New wires the app and hooks reconcile passes and decisions to the hub.
func New(cfg config.Config, st *store.Store, rec *reconcile.Reconciler, view *hostview.View,
	ring *decisions.Ring, hub *events.Hub, clk clock.Clock, log *slog.Logger) *App {
	a := &App{Cfg: cfg, Store: st, Rec: rec, View: view, Ring: ring, Hub: hub, Clock: clk, Log: log,
		prevDem: map[string]string{}, prevWork: map[string]string{}, prevClaims: map[string]string{}, prevHosts: map[string]string{},
		claimWake: map[string]chan struct{}{}, claimWaiters: map[string]int{}, hostSeen: map[string]string{}, claimVer: map[string]int64{}}
	ring.OnAdd(func(d decisions.Decision) {
		hub.Publish(events.TypeDecision, func(seq int64) any {
			return api.DecisionEvent{Seq: seq, Type: api.DecisionEventTypeDecision, Decision: Decision(d)}
		})
	})
	rec.OnPass = a.afterPass
	return a
}

// ---- demands ----

// PutDemand creates or refreshes a demand (hourchd or manual).
func (a *App) PutDemand(ctx context.Context, in store.DemandPut) (api.Demand, error) {
	d, err := a.Store.PutDemand(ctx, in, a.Clock.Now())
	if err != nil {
		return api.Demand{}, err
	}
	out := a.demand(d)
	a.publishDemand(api.Put, d.ID, &out)
	a.Rec.Trigger()
	return out, nil
}

// DeleteDemand deletes a demand; unknown ids are fine.
func (a *App) DeleteDemand(ctx context.Context, id string) error {
	if err := a.Store.DeleteDemand(ctx, id); err != nil {
		return err
	}
	a.publishDemand(api.Delete, id, nil)
	a.Rec.Trigger()
	return nil
}

// Demands lists demands oldest first with covered_by from the last pass.
func (a *App) Demands(ctx context.Context, includeExpired bool) ([]api.Demand, error) {
	ds, err := a.Store.ListDemands(ctx, a.Clock.Now(), includeExpired)
	if err != nil {
		return nil, err
	}
	out := make([]api.Demand, 0, len(ds))
	for _, d := range ds {
		out = append(out, a.demand(d))
	}
	return out, nil
}

// Demand returns one demand; expired rows only with includeExpired.
func (a *App) Demand(ctx context.Context, id string, includeExpired bool) (api.Demand, error) {
	d, err := a.Store.GetDemand(ctx, id)
	if err != nil {
		return api.Demand{}, err
	}
	if !includeExpired && !d.Open(a.Clock.Now()) {
		return api.Demand{}, store.ErrNotFound
	}
	return a.demand(d), nil
}

func (a *App) demand(d model.Demand) api.Demand {
	out := api.Demand{Id: d.ID, Tags: d.Tags, LeaseSeconds: d.LeaseSeconds, LeaseUntil: d.LeaseUntil,
		Source: api.DemandSource(d.Source), ExternalRef: d.ExternalRef, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if by := a.Rec.Last().Output.CoveredBy[d.ID]; by != "" {
		out.CoveredBy = &by
	}
	return out
}

func (a *App) publishDemand(op api.ChangeOp, id string, d *api.Demand) {
	a.mu.Lock()
	if d == nil {
		delete(a.prevDem, id)
	} else {
		a.prevDem[id] = demandKey(*d)
	}
	a.mu.Unlock()
	a.Hub.Publish(events.TypeDemand, func(seq int64) any {
		return api.DemandEvent{Seq: seq, Type: api.DemandEventTypeDemand, Op: op, Id: id, Demand: d}
	})
}

// ---- workers ----

// PutWorker creates or refreshes a liveness record.
func (a *App) PutWorker(ctx context.Context, in store.WorkerPut) (api.Worker, error) {
	w, err := a.Store.PutWorker(ctx, in, a.Clock.Now())
	if err != nil {
		return api.Worker{}, err
	}
	if _, known := a.View.Host(w.Hostname); !known {
		a.Log.Error("liveness hostname not in powermaster inventory", "worker", w.WorkerID, "hostname", w.Hostname)
	}
	out := Worker(w)
	a.publishWorker(api.Put, w.WorkerID, &out)
	a.Rec.Trigger()
	return out, nil
}

// DeleteWorker removes a liveness record; unknown ids are fine.
func (a *App) DeleteWorker(ctx context.Context, id string) error {
	if err := a.Store.DeleteWorker(ctx, id); err != nil {
		return err
	}
	a.publishWorker(api.Delete, id, nil)
	a.Rec.Trigger()
	return nil
}

// Workers lists liveness records; expired rows only with includeExpired.
func (a *App) Workers(ctx context.Context, includeExpired bool) ([]api.Worker, error) {
	ws, err := a.Store.ListWorkers(ctx, a.Clock.Now(), includeExpired)
	if err != nil {
		return nil, err
	}
	out := make([]api.Worker, 0, len(ws))
	for _, w := range ws {
		out = append(out, Worker(w))
	}
	return out, nil
}

func (a *App) publishWorker(op api.ChangeOp, id string, w *api.Worker) {
	a.mu.Lock()
	if w == nil {
		delete(a.prevWork, id)
	} else {
		a.prevWork[id] = workerKey(*w)
	}
	a.mu.Unlock()
	a.Hub.Publish(events.TypeWorker, func(seq int64) any {
		return api.WorkerEvent{Seq: seq, Type: api.WorkerEventTypeWorker, Op: op, WorkerId: id, Worker: w}
	})
}

// Worker converts a store row.
func Worker(w model.Worker) api.Worker {
	return api.Worker{WorkerId: w.WorkerID, Hostname: w.Hostname, Tags: w.Tags, JobStatus: api.JobStatus(w.JobStatus),
		DemandId: w.DemandID, LeaseSeconds: w.LeaseSeconds, LeaseUntil: w.LeaseUntil, LastBusyAt: w.LastBusyAt,
		UpdatedAt: w.UpdatedAt}
}

// ---- hosts ----

// Hosts returns the policy view joined with the powermaster view, in inventory order.
func (a *App) Hosts(ctx context.Context) ([]api.HostView, error) {
	pol, err := a.Store.HostPolicies(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := a.Store.ListWorkers(ctx, a.Clock.Now(), true)
	if err != nil {
		return nil, err
	}
	live, err := a.Store.ListClaims(ctx, a.Clock.Now(), store.ClaimFilter{})
	if err != nil {
		return nil, err
	}
	claims, blocked := hostClaims(live)
	last := a.Rec.Last().Output.Hosts
	idx := a.NameIndex()
	var out []api.HostView
	for _, h := range a.View.Hosts() {
		hv := hostView(h, pol[h.Hostname], last[h.Hostname], ws, idx)
		if cs := claims[h.Hostname]; cs != nil {
			hv.Claims = cs
		}
		hv.JobsBlocked = blocked[h.Hostname]
		out = append(out, hv)
	}
	if out == nil {
		out = []api.HostView{}
	}
	return out, nil
}

// Host returns one host view, or store.ErrNotFound if powermaster does not know it.
func (a *App) Host(ctx context.Context, hostname string) (api.HostView, error) {
	hs, err := a.Hosts(ctx)
	if err != nil {
		return api.HostView{}, err
	}
	for _, h := range hs {
		if h.Hostname == hostname {
			return h, nil
		}
	}
	return api.HostView{}, store.ErrNotFound
}

// NameIndex indexes the name tags of the hosts in the current powermaster
// view. It is one snapshot: reuse it for every match in one operation.
func (a *App) NameIndex() *reconcile.NameIndex {
	hs := a.View.Hosts()
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.Hostname)
	}
	return reconcile.NewNameIndex(names)
}

// SetHostTags replaces the capability tags of a host powermaster knows.
// Name tags are reserved: an explicit tag equal to any known host's full or
// short name is a bad request.
func (a *App) SetHostTags(ctx context.Context, hostname string, tags []string) (api.HostView, error) {
	if _, ok := a.View.Host(hostname); !ok {
		return api.HostView{}, store.ErrNotFound
	}
	idx := a.NameIndex()
	for _, t := range tags {
		if idx.Reserved(t) {
			return api.HostView{}, fmt.Errorf("%w: tag %q is reserved: it is the name of a host", store.ErrBadRequest, t)
		}
	}
	norm, err := a.Store.SetHostTags(ctx, hostname, tags)
	if err != nil {
		return api.HostView{}, err
	}
	a.Log.Info("host tags changed", "host", hostname, "tags", norm)
	hv, err := a.Host(ctx, hostname)
	if err != nil {
		return api.HostView{}, err
	}
	a.publishHost(hv)
	a.Rec.Trigger()
	return hv, nil
}

// SetExclude sets or clears exclude on a host powermaster knows.
func (a *App) SetExclude(ctx context.Context, hostname string, exclude bool) (api.HostView, error) {
	if _, ok := a.View.Host(hostname); !ok {
		return api.HostView{}, store.ErrNotFound
	}
	if err := a.Store.SetExclude(ctx, hostname, exclude); err != nil {
		return api.HostView{}, err
	}
	if !exclude {
		// Leaving exclude restarts the dwell rather than powering off at once.
		_ = a.Store.SetIdleSince(ctx, hostname, nil)
	}
	a.Log.Info("exclude changed", "host", hostname, "exclude", exclude)
	hv, err := a.Host(ctx, hostname)
	if err != nil {
		return api.HostView{}, err
	}
	a.publishHost(hv)
	a.Rec.Trigger()
	return hv, nil
}

func hostView(h pm.Host, p model.HostPolicy, st reconcile.HostStatus, ws []model.Worker, idx *reconcile.NameIndex) api.HostView {
	hv := api.HostView{Hostname: h.Hostname, Tags: p.Tags, State: api.HostState(h.State), Locked: h.Locked,
		Exclude: p.Exclude, IdleSince: p.IdleSince, CooldownUntil: st.CooldownUntil,
		BootGraceUntil: st.BootGraceUntil, BusyHeldUntil: st.BusyHeldUntil, Workers: []string{},
		NameTags: idx.NameTags(h.Hostname), Claims: []api.HostClaim{}}
	if hv.NameTags == nil {
		hv.NameTags = []string{}
	}
	if hv.Tags == nil {
		hv.Tags = []string{}
	}
	if !h.StateSince.IsZero() {
		since := h.StateSince
		hv.StateSince = &since
	}
	hv.ColdOffDueAt = st.ColdOffDueAt
	if st.PairedDemand != "" {
		d := st.PairedDemand
		hv.PairedDemand = &d
	}
	for _, w := range ws {
		if w.Hostname == h.Hostname {
			hv.Workers = append(hv.Workers, w.WorkerID)
		}
	}
	return hv
}

func (a *App) publishHost(hv api.HostView) {
	a.mu.Lock()
	a.prevHosts[hv.Hostname] = enc(hv)
	a.mu.Unlock()
	a.Hub.Publish(events.TypeHost, func(seq int64) any {
		return api.HostEvent{Seq: seq, Type: api.Host, Host: hv}
	})
}

// afterPass publishes what changed since the previous pass: demands that
// appeared, changed coverage, or went away (expiry, sweep), workers
// that became fresh or expired, and host views that changed.
func (a *App) afterPass(res reconcile.PassResult) {
	if res.Err != nil {
		return
	}
	now := res.At
	curDem := map[string]api.Demand{}
	for _, d := range res.Demands {
		curDem[d.ID] = a.demand(d)
	}
	curWork := map[string]api.Worker{}
	for _, w := range res.Workers {
		if w.Fresh(now) {
			curWork[w.WorkerID] = Worker(w)
		}
	}
	ctx := context.Background()
	a.afterPassClaims(ctx, now) // before host views, so they show fresh bindings
	hosts, err := a.Hosts(ctx)
	if err != nil {
		a.Log.Error("host views", "error", err)
	}

	a.mu.Lock()
	var dPut, wPut []string
	var dDel, wDel []string
	for id, d := range curDem {
		if a.prevDem[id] != demandKey(d) {
			dPut = append(dPut, id)
		}
	}
	for id := range a.prevDem {
		if _, ok := curDem[id]; !ok {
			dDel = append(dDel, id)
		}
	}
	for id, w := range curWork {
		if a.prevWork[id] != workerKey(w) {
			wPut = append(wPut, id)
		}
	}
	for id := range a.prevWork {
		if _, ok := curWork[id]; !ok {
			wDel = append(wDel, id)
		}
	}
	var hChanged []api.HostView
	for _, hv := range hosts {
		if a.prevHosts[hv.Hostname] != enc(hv) {
			hChanged = append(hChanged, hv)
		}
	}
	a.mu.Unlock()

	sort.Strings(dPut)
	sort.Strings(dDel)
	sort.Strings(wPut)
	sort.Strings(wDel)
	for _, id := range dPut {
		d := curDem[id]
		a.publishDemand(api.Put, id, &d)
	}
	for _, id := range dDel {
		a.publishDemand(api.Delete, id, nil)
	}
	for _, id := range wPut {
		w := curWork[id]
		a.publishWorker(api.Put, id, &w)
	}
	for _, id := range wDel {
		a.publishWorker(api.Delete, id, nil)
	}
	for _, hv := range hChanged {
		a.publishHost(hv)
	}
}

// ---- decisions, stats, health ----

// Decisions returns the ring newest first.
func (a *App) Decisions() []api.Decision {
	ds := a.Ring.List()
	out := make([]api.Decision, 0, len(ds))
	for _, d := range ds {
		out = append(out, Decision(d))
	}
	return out
}

// Decision converts a ring entry.
func Decision(d decisions.Decision) api.Decision {
	out := api.Decision{Ts: d.Ts, Action: api.DecisionAction(d.Action), CommandId: d.CommandID, Detail: d.Detail}
	if d.Hostname != "" {
		h := d.Hostname
		out.Hostname = &h
	}
	if d.DemandID != "" {
		id := d.DemandID
		out.DemandId = &id
	}
	return out
}

// Stats is the UI header and GET /v1/stats.
func (a *App) Stats() api.Stats {
	last := a.Rec.Last()
	st := api.Stats{HostsByPolicy: map[string]int{}}
	if !last.At.IsZero() {
		t := last.At
		st.LastReconcileAt = &t
	}
	for _, by := range last.Output.CoveredBy {
		st.DemandsOpen++
		if by == "" {
			st.DemandsUncovered++
		} else {
			st.DemandsCovered++
		}
	}
	for _, w := range last.Workers {
		if w.Fresh(last.At) {
			st.WorkersFresh++
		}
	}
	pol, _ := a.Store.HostPolicies(context.Background())
	for _, h := range a.View.Hosts() {
		for _, s := range PolicyStatus(h, pol[h.Hostname], last.Output.Hosts[h.Hostname]) {
			st.HostsByPolicy[s]++
		}
	}
	if r := a.Rec.LastRequest(); r != nil {
		st.LastPowerRequest = &api.PowerRequestSummary{At: r.At, Hostname: r.Hostname,
			Action: api.PowerRequestSummaryAction(r.Action), CommandId: r.CommandID}
	}
	return st
}

// PolicyStatus names the policy states a host is in (for stats and the UI).
func PolicyStatus(h pm.Host, p model.HostPolicy, st reconcile.HostStatus) []string {
	var out []string
	if h.Locked {
		out = append(out, "locked")
	}
	if p.Exclude {
		out = append(out, "excluded")
	}
	if st.Booting {
		out = append(out, "booting")
	}
	if st.BootGraceUntil != nil {
		out = append(out, "boot_grace")
	}
	if st.CooldownUntil != nil {
		out = append(out, "cooldown")
	}
	if st.BusyHeldUntil != nil {
		out = append(out, "busy_held")
	}
	if st.IdleEligible {
		out = append(out, "idle_dwell")
	}
	return out
}

// Healthy reports whether SQLite answers.
func (a *App) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return a.Store.Ping(ctx) == nil
}

// demandKey and workerKey leave out fields that move on every refresh
// (lease_until, updated_at, last_busy_at), so a pass publishes only real
// changes; API writes publish their own events.
func demandKey(d api.Demand) string {
	d.LeaseUntil, d.UpdatedAt = time.Time{}, time.Time{}
	return enc(d)
}

func workerKey(w api.Worker) string {
	w.LeaseUntil, w.UpdatedAt, w.LastBusyAt = time.Time{}, time.Time{}, nil
	return enc(w)
}

func enc(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("app: marshal: %v", err))
	}
	return string(b)
}
