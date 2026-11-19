package app_test

import (
	"context"
	"testing"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/apptest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

func ip(i int) *int { return &i }

// A pass publishes demand and worker changes the API did not (expiry), and
// nothing when nothing changed.
func TestPassEvents(t *testing.T) {
	env := apptest.New(t, func(c *config.Config) { c.HostTags = map[string][]string{"h1": {"kernel"}} }, pm.Host{Hostname: "h1", State: pm.StateOn})
	ctx := context.Background()
	if _, err := env.App.PutDemand(ctx, store.DemandPut{ID: "d1", Tags: []string{"tiny"}, Source: "manual", LeaseSeconds: ip(30)}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.PutWorker(ctx, store.WorkerPut{WorkerID: "w1", Hostname: "h1", Tags: []string{"kernel"}, JobStatus: "idle", LeaseSeconds: ip(15)}); err != nil {
		t.Fatal(err)
	}
	env.Pass()
	seq := env.App.Hub.Seq()
	env.Pass()
	if got := env.App.Hub.Seq(); got != seq {
		t.Fatalf("idle pass published %d events", got-seq)
	}
	env.Clock.Advance(time.Minute) // both leases expire
	env.App.Rec.SyncHosts(ctx)
	env.Pass()
	ds, _ := env.App.Demands(ctx, false)
	if len(ds) != 0 {
		t.Fatal("expired demand listed")
	}
	if env.App.Hub.Seq() < seq+2 {
		t.Fatalf("expiry did not publish demand and worker deletes (seq %d -> %d)", seq, env.App.Hub.Seq())
	}
	// Decisions reach the hub too.
	before := env.App.Hub.Seq()
	env.App.PutDemand(ctx, store.DemandPut{ID: "d2", Tags: []string{"kernel"}, Source: "manual"})
	env.PM.SetState("h1", pm.StateSoftOff)
	env.App.Rec.SyncHosts(ctx)
	env.Pass()
	if len(env.App.Decisions()) == 0 || env.App.Hub.Seq() <= before+1 {
		t.Fatal("decision not published")
	}
}

func TestHostViewColdOffFields(t *testing.T) {
	since := apptest.T0.Add(-10 * time.Minute)
	env := apptest.New(t, func(c *config.Config) { c.HostTags = map[string][]string{"h1": {"kernel"}} },
		pm.Host{Hostname: "h1", State: pm.StateSoftOff, StateSince: since},
		pm.Host{Hostname: "h2", State: pm.StateSoftOff})
	env.Pass()
	hs, err := env.App.Hosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h1, h2 := hs[0], hs[1]
	if h1.StateSince == nil || !h1.StateSince.Equal(since) || h1.ColdOffDueAt == nil || !h1.ColdOffDueAt.Equal(since.Add(time.Hour)) {
		t.Fatalf("h1 %+v", h1)
	}
	if h2.StateSince != nil || h2.ColdOffDueAt != nil {
		t.Fatalf("h2 without state_since must have neither: %+v", h2)
	}
}

// A pass binds a waiting claim once the claim in front of it lapses, publishes
// the lapse as a delete, and sweeps claims 24h after they lapsed.
func TestPassBindsWaiterOnLapseAndSweeps(t *testing.T) {
	env := apptest.New(t, nil, pm.Host{Hostname: "h1.lab", State: pm.StateOn})
	ctx := context.Background()
	holder := app.Caller{Name: "ann"}
	waiter := app.Caller{Name: "jared"}
	if _, err := env.App.PutClaim(ctx, holder, store.ClaimPut{ID: "h", Tags: []string{"h1"}, Purpose: "p", LeaseSeconds: ip(60)}); err != nil {
		t.Fatal(err)
	}
	d, err := env.App.PutClaim(ctx, waiter, store.ClaimPut{ID: "d", Tags: []string{"h1"}, Purpose: "p", Mode: "dedicated", Detached: true, MaxSeconds: ip(36000)})
	if err != nil || d.State != api.Pending {
		t.Fatalf("%+v %v", d, err)
	}
	env.Pass()
	seq := env.App.Hub.Seq()
	env.Pass()
	if env.App.Hub.Seq() != seq {
		t.Fatal("idle pass published events")
	}
	env.Clock.Advance(2 * time.Minute) // h's lease (60s) lapses; d is detached
	env.Pass()
	got, err := env.App.Claim(ctx, "d", false)
	if err != nil || got.State != api.Ready || got.Hostname == nil || *got.Hostname != "h1.lab" || got.Version != 2 {
		t.Fatalf("waiter not bound by the pass: %+v %v", got, err)
	}
	if env.App.Hub.Seq() < seq+2 { // h deleted, d changed
		t.Fatalf("lapse and bind not published (seq %d -> %d)", seq, env.App.Hub.Seq())
	}
	env.Clock.Advance(36 * time.Hour) // h and d (cap 10h) lapsed more than 24h ago
	env.Pass()
	cs, _ := env.Store.ListClaims(ctx, env.Clock.Now(), store.ClaimFilter{IncludeExpired: true})
	if len(cs) != 0 {
		t.Fatalf("not swept: %+v", cs)
	}
}
