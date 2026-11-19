package reconcile

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
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

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type harness struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	pm     *pm.Fake
	st     *store.Store
	r      *Reconciler
	events <-chan pm.Event
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openStore(t *testing.T, tm config.Timings) *store.Store {
	t.Helper()
	lim := config.Default().Limits
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Limits{
		DemandLeaseDefault: tm.DemandLeaseDefault, DemandLeaseMin: tm.DemandLeaseMin, DemandLeaseMax: tm.DemandLeaseMax,
		WorkerLeaseDefault: tm.WorkerLeaseDefault, WorkerLeaseMin: tm.WorkerLeaseMin, WorkerLeaseMax: tm.WorkerLeaseMax,
		MaxDemands: lim.MaxDemands, MaxTags: lim.MaxTags, MaxClaims: lim.MaxClaims,
		ClaimLeaseDefault: tm.ClaimLeaseDefault, ClaimLeaseMin: tm.ClaimLeaseMin, ClaimLeaseMax: tm.ClaimLeaseMax,
		ClaimCapDefault: tm.ClaimCapDefault, ClaimCapMin: tm.ClaimCapMin, ClaimCapMax: tm.ClaimCapMax,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// host is a fake powermaster host plus the capability tags powerwarden
// stores for it (nil: none stored).
type host struct {
	pm.Host
	tags []string
}

// newHarness builds a reconciler over a fake powermaster with
// the given hosts, synced once.
func newHarness(t *testing.T, hosts ...host) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clk := clock.NewFake(t0)
	f := pm.NewFake(clk.Now)
	for _, h := range hosts {
		f.AddHost(h.Host)
	}
	tm := config.Default().Timings
	st := openStore(t, tm)
	for _, h := range hosts {
		if h.tags != nil {
			if _, err := st.SetHostTags(ctx, h.Hostname, h.tags); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := New(tm, st, source.HTTP{Store: st}, f, hostview.New(), decisions.NewRing(100), clk, quietLog())
	evs, err := f.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: ctx, clk: clk, pm: f, st: st, r: r, events: evs}
	if err := r.SyncHosts(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func kernelHost(name string, state string) host {
	return host{pm.Host{Hostname: name, State: state}, []string{"kernel"}}
}

// drain feeds pending fake powermaster events to the reconciler.
func (h *harness) drain() {
	for {
		select {
		case ev := <-h.events:
			h.r.HandleEvent(h.ctx, ev)
		default:
			return
		}
	}
}

func (h *harness) advance(d time.Duration) { h.clk.Advance(d) }

func (h *harness) pass() PassResult {
	h.t.Helper()
	h.drain()
	_ = h.r.SyncHosts(h.ctx) // as the Run loop does every host_view_refresh
	res := h.r.Pass(h.ctx)
	if res.Err != nil {
		h.t.Fatal(res.Err)
	}
	h.drain()
	return res
}

func (h *harness) demand(id string, tags ...string) {
	h.t.Helper()
	if _, err := h.st.PutDemand(h.ctx, store.DemandPut{ID: id, Tags: tags, Source: "hourchd"}, h.clk.Now()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) worker(id, host, status, demandID string, tags ...string) {
	h.t.Helper()
	_, err := h.st.PutWorker(h.ctx, store.WorkerPut{WorkerID: id, Hostname: host, Tags: tags, JobStatus: status, DemandID: demandID}, h.clk.Now())
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) requests(action string) []pm.Request {
	var out []pm.Request
	for _, r := range h.pm.Requests() {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

func (h *harness) wantRequests(action string, hosts ...string) {
	h.t.Helper()
	got := h.requests(action)
	if len(got) != len(hosts) {
		h.t.Fatalf("%s requests: got %v, want hosts %v", action, got, hosts)
	}
	for i, r := range got {
		if r.Hostname != hosts[i] {
			h.t.Fatalf("%s request %d: got %s, want %s", action, i, r.Hostname, hosts[i])
		}
	}
}

func (h *harness) lastCmd(host string) pm.Command {
	h.t.Helper()
	c, ok := h.pm.LastCommand(host)
	if !ok {
		h.t.Fatalf("no command for %s", host)
	}
	return c
}

func (h *harness) hasDecision(action, host string) bool {
	for _, d := range h.r.Ring.List() {
		if d.Action == action && (host == "" || d.Hostname == host) {
			return true
		}
	}
	return false
}

// putClaim creates or renews a claim the way the API does: binding runs in
// the PUT transaction against the current host snapshot.
func (h *harness) putClaim(in store.ClaimPut) model.Claim {
	h.t.Helper()
	h.drain()
	if in.Owner == "" {
		in.Owner = "agent"
	}
	if in.Purpose == "" {
		in.Purpose = "test"
	}
	now := h.clk.Now()
	pol, err := h.st.HostPolicies(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	ws, err := h.st.ListWorkers(h.ctx, now, false)
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := h.st.PutClaim(h.ctx, in, BindHosts(now, h.r.Timings, h.r.View.Hosts(), pol, ws), now)
	if err != nil {
		h.t.Fatalf("PutClaim %s: %v", in.ID, err)
	}
	return c
}

// claim makes a claim with a one-hour lease.
func (h *harness) claim(id, mode string, tags ...string) model.Claim {
	h.t.Helper()
	return h.putClaim(store.ClaimPut{ID: id, Mode: mode, Tags: tags, LeaseSeconds: ip(3600)})
}

func (h *harness) getClaim(id string) model.Claim {
	h.t.Helper()
	c, err := h.st.GetClaim(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// wantClaim checks a claim's state, bound host and detail.
func (h *harness) wantClaim(id, state, host, detail string) model.Claim {
	h.t.Helper()
	c := h.getClaim(id)
	if c.State != state || c.Hostname != host || c.Detail != detail {
		h.t.Fatalf("claim %s: got %s on %q (%q), want %s on %q (%q)", id, c.State, c.Hostname, c.Detail, state, host, detail)
	}
	return c
}

func (h *harness) release(id string) {
	h.t.Helper()
	if err := h.st.ReleaseClaim(h.ctx, id, "", true); err != nil {
		h.t.Fatal(err)
	}
}

func ip(n int) *int { return &n }
