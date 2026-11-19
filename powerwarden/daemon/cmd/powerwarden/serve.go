package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/clock"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/decisions"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/hostview"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/httpapi"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/reconcile"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/source"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/ui"
)

// daemon is a wired powerwarden ready to run.
type daemon struct {
	cfg     config.Config
	log     *slog.Logger
	store   *store.Store
	rec     *reconcile.Reconciler
	handler http.Handler
	closers []func()
}

func (d *daemon) Close() {
	for i := len(d.closers) - 1; i >= 0; i-- {
		d.closers[i]()
	}
}

// build wires every component from cfg. transport is nil in production.
func build(cfg config.Config, log *slog.Logger, transport http.RoundTripper) (*daemon, error) {
	d := &daemon{cfg: cfg, log: log}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
	t := cfg.Timings
	st, err := store.Open(cfg.StatePath, store.Limits{
		DemandLeaseDefault: t.DemandLeaseDefault, DemandLeaseMin: t.DemandLeaseMin, DemandLeaseMax: t.DemandLeaseMax,
		WorkerLeaseDefault: t.WorkerLeaseDefault, WorkerLeaseMin: t.WorkerLeaseMin, WorkerLeaseMax: t.WorkerLeaseMax,
		MaxDemands: cfg.Limits.MaxDemands, MaxTags: cfg.Limits.MaxTags, MaxClaims: cfg.Limits.MaxClaims,
		ClaimLeaseDefault: t.ClaimLeaseDefault, ClaimLeaseMin: t.ClaimLeaseMin, ClaimLeaseMax: t.ClaimLeaseMax,
		ClaimCapDefault: t.ClaimCapDefault, ClaimCapMin: t.ClaimCapMin, ClaimCapMax: t.ClaimCapMax,
	})
	if err != nil {
		return nil, err
	}
	d.store = st
	d.closers = append(d.closers, func() { st.Close() })
	if err := st.SeedHostTags(context.Background(), cfg.HostTags); err != nil {
		return nil, err
	}

	tokens, err := httpapi.LoadTokens(cfg.TokensFile)
	if err != nil {
		return nil, err
	}
	pmToken, err := os.ReadFile(cfg.Powermaster.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("powermaster token: %w", err)
	}
	client, err := pm.NewHTTP(cfg.Powermaster.URL, strings.TrimSpace(string(pmToken)), cfg.Powermaster.RequestTimeout, transport)
	if err != nil {
		return nil, err
	}

	clk := clock.Real{}
	view := hostview.New()
	ring := decisions.NewRing(cfg.Limits.DecisionsKept)
	hub := events.New(cfg.Events.RingSize, cfg.Events.Heartbeat)
	d.rec = reconcile.New(t, st, source.HTTP{Store: st}, client, view, ring, clk, log)
	a := app.New(cfg, st, d.rec, view, ring, hub, clk, log)

	mux := http.NewServeMux()
	(&httpapi.Server{App: a, Tokens: tokens, Log: log}).Register(mux)
	if cfg.UI.Enabled {
		var u *ui.UI
		if cfg.UI.Auth == "proxy" {
			// Proxy identity: the htpasswd file is not read.
			p, err := ui.LoadProxyAuth(cfg.UI.Proxy.SecretFile, cfg.UI.Proxy.SecretHeader, cfg.UI.Proxy.UserHeader)
			if err != nil {
				return nil, err
			}
			if u, err = ui.NewProxy(a, p, log); err != nil {
				return nil, err
			}
		} else {
			users, err := ui.LoadHtpasswd(cfg.UI.HtpasswdFile)
			if err != nil {
				return nil, err
			}
			if u, err = ui.New(a, users, log); err != nil {
				return nil, err
			}
		}
		log.Info("ui auth", "mode", cfg.UI.Auth)
		u.Register(mux)
	}
	d.handler = mux
	ok = true
	return d, nil
}

// serve runs the API, UI and reconciler until ctx is cancelled.
func serve(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	d, err := build(cfg, log, nil)
	if err != nil {
		return err
	}
	defer d.Close()

	srv := &http.Server{Addr: cfg.Listen, Handler: d.handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- d.rec.Run(ctx) }()
	log.Info("powerwarden serving", "listen", cfg.Listen,
		"powermaster", cfg.Powermaster.URL, "ui", cfg.UI.Enabled)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
