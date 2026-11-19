// Package apptest builds a complete app over a fake powermaster and a temp
// SQLite store, for httpapi and ui tests. Not used by the daemon.
package apptest

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/clock"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/decisions"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/hostview"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/reconcile"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/source"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

var T0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// Env is a wired app plus its fakes.
type Env struct {
	App   *app.App
	Clock *clock.Fake
	PM    *pm.Fake
	Store *store.Store
	// Logs collects the daemon log (all levels) for tests that assert on it.
	Logs *LogBuf
}

// LogBuf is a goroutine-safe log sink.
type LogBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *LogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns everything logged so far.
func (l *LogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// New builds an app with the given config tweak and hosts, and syncs hosts once.
func New(t *testing.T, tweak func(*config.Config), hosts ...pm.Host) *Env {
	t.Helper()
	cfg := config.Default()
	cfg.Events.Heartbeat = 50 * time.Millisecond
	if tweak != nil {
		tweak(&cfg)
	}
	tm := cfg.Timings
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Limits{
		DemandLeaseDefault: tm.DemandLeaseDefault, DemandLeaseMin: tm.DemandLeaseMin, DemandLeaseMax: tm.DemandLeaseMax,
		WorkerLeaseDefault: tm.WorkerLeaseDefault, WorkerLeaseMin: tm.WorkerLeaseMin, WorkerLeaseMax: tm.WorkerLeaseMax,
		MaxDemands: cfg.Limits.MaxDemands, MaxTags: cfg.Limits.MaxTags, MaxClaims: cfg.Limits.MaxClaims,
		ClaimLeaseDefault: tm.ClaimLeaseDefault, ClaimLeaseMin: tm.ClaimLeaseMin, ClaimLeaseMax: tm.ClaimLeaseMax,
		ClaimCapDefault: tm.ClaimCapDefault, ClaimCapMin: tm.ClaimCapMin, ClaimCapMax: tm.ClaimCapMax,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SeedHostTags(context.Background(), cfg.HostTags); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(T0)
	f := pm.NewFake(clk.Now)
	for _, h := range hosts {
		f.AddHost(h)
	}
	logs := &LogBuf{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	view := hostview.New()
	ring := decisions.NewRing(cfg.Limits.DecisionsKept)
	rec := reconcile.New(tm, st, source.HTTP{Store: st}, f, view, ring, clk, log)
	hub := events.New(cfg.Events.RingSize, cfg.Events.Heartbeat)
	a := app.New(cfg, st, rec, view, ring, hub, clk, log)
	if err := rec.SyncHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Env{App: a, Clock: clk, PM: f, Store: st, Logs: logs}
}

// Pass runs one reconcile pass.
func (e *Env) Pass() reconcile.PassResult { return e.App.Rec.Pass(context.Background()) }
