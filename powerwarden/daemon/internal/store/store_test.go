package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func testLimits() Limits {
	return Limits{
		DemandLeaseDefault: 3600 * time.Second, DemandLeaseMin: 30 * time.Second, DemandLeaseMax: 3600 * time.Second,
		WorkerLeaseDefault: 90 * time.Second, WorkerLeaseMin: 15 * time.Second, WorkerLeaseMax: 300 * time.Second,
		MaxDemands: 2, MaxTags: 3,
		ClaimLeaseDefault: 15 * time.Minute, ClaimLeaseMin: time.Minute, ClaimLeaseMax: time.Hour,
		ClaimCapDefault: 8 * time.Hour, ClaimCapMin: 15 * time.Minute, ClaimCapMax: 72 * time.Hour,
		MaxClaims: 6,
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ip(i int) *int { return &i }

func TestDemandPutIdempotentAndLeaseKept(t *testing.T) {
	s, ctx := open(t), context.Background()
	d, err := s.PutDemand(ctx, DemandPut{ID: "hourchd:1", Tags: []string{"kernel", "kernel"}, LeaseSeconds: ip(60), Source: "hourchd"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tags) != 1 || d.LeaseSeconds != 60 || !d.LeaseUntil.Equal(t0.Add(60*time.Second)) {
		t.Fatalf("unexpected %+v", d)
	}
	// Refresh without lease_seconds keeps 60 and created_at; lease_until moves.
	d2, err := s.PutDemand(ctx, DemandPut{ID: "hourchd:1", Tags: []string{"kernel"}, Source: "hourchd"}, t0.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if d2.LeaseSeconds != 60 || !d2.CreatedAt.Equal(t0) || !d2.LeaseUntil.Equal(t0.Add(70*time.Second)) {
		t.Fatalf("refresh wrong: %+v", d2)
	}
	got, err := s.GetDemand(ctx, "hourchd:1")
	if err != nil || got.LeaseSeconds != 60 || !got.CreatedAt.Equal(t0) {
		t.Fatalf("get: %+v %v", got, err)
	}
	// New demand without lease gets the default.
	d3, _ := s.PutDemand(ctx, DemandPut{ID: "m1", Source: "manual"}, t0)
	if d3.LeaseSeconds != 3600 || len(d3.Tags) != 0 {
		t.Fatalf("default lease: %+v", d3)
	}
}

func TestDemandValidationAndCapacity(t *testing.T) {
	s, ctx := open(t), context.Background()
	bad := []DemandPut{
		{ID: "bad id", Source: "manual"},
		{ID: "a", Source: "redis"},
		{ID: "a", Source: "manual", Tags: []string{"no spaces"}},
		{ID: "a", Source: "manual", Tags: []string{"a", "b", "c", "d"}},
		{ID: "a", Source: "manual", LeaseSeconds: ip(10)},
		{ID: "a", Source: "manual", LeaseSeconds: ip(4000)},
	}
	for _, in := range bad {
		if _, err := s.PutDemand(ctx, in, t0); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: want bad request, got %v", in, err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if _, err := s.PutDemand(ctx, DemandPut{ID: id, Source: "manual"}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutDemand(ctx, DemandPut{ID: "c", Source: "manual"}, t0); !errors.Is(err, ErrCapacity) {
		t.Fatalf("want capacity, got %v", err)
	}
	// Refreshing an existing id at capacity is fine.
	if _, err := s.PutDemand(ctx, DemandPut{ID: "a", Source: "manual"}, t0); err != nil {
		t.Fatal(err)
	}
	// Deleting unknown ids is fine.
	if err := s.DeleteDemand(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredDemandHiddenUntilSwept(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.PutDemand(ctx, DemandPut{ID: "a", Source: "manual", LeaseSeconds: ip(30)}, t0)
	later := t0.Add(time.Minute)
	if l, _ := s.ListDemands(ctx, later, false); len(l) != 0 {
		t.Fatal("expired demand listed")
	}
	if l, _ := s.ListDemands(ctx, later, true); len(l) != 1 {
		t.Fatal("expired demand not listed with includeExpired")
	}
	n, _, _ := s.Sweep(ctx, later, 24*time.Hour, 26*time.Hour)
	if n != 0 {
		t.Fatal("swept too early")
	}
	n, _, _ = s.Sweep(ctx, t0.Add(25*time.Hour), 24*time.Hour, 26*time.Hour)
	if n != 1 {
		t.Fatal("not swept")
	}
}

func TestWorkerLastBusyAt(t *testing.T) {
	s, ctx := open(t), context.Background()
	put := func(status string, at time.Time) model.Worker {
		w, err := s.PutWorker(ctx, WorkerPut{WorkerID: "v1", Hostname: "veritech01.labtwo", Tags: []string{"kernel"}, JobStatus: status, DemandID: "hourchd:1"}, at)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := put("busy", t0)
	if w.LastBusyAt == nil || !w.LastBusyAt.Equal(t0) || w.LeaseSeconds != 90 {
		t.Fatalf("busy: %+v", w)
	}
	w = put("error", t0.Add(time.Minute))
	if w.LastBusyAt == nil || !w.LastBusyAt.Equal(t0) {
		t.Fatal("error should keep last_busy_at")
	}
	w = put("idle", t0.Add(2*time.Minute))
	if w.LastBusyAt != nil {
		t.Fatal("idle should clear last_busy_at")
	}
	// Expired workers are hidden but kept.
	late := t0.Add(time.Hour)
	if l, _ := s.ListWorkers(ctx, late, false); len(l) != 0 {
		t.Fatal("expired worker listed")
	}
	if l, _ := s.ListWorkers(ctx, late, true); len(l) != 1 {
		t.Fatal("expired worker lost")
	}
	if _, err := s.PutWorker(ctx, WorkerPut{WorkerID: "v1", Hostname: "h", JobStatus: "sleeping"}, t0); !errors.Is(err, ErrBadRequest) {
		t.Fatal("bad job_status accepted")
	}
	if _, err := s.PutWorker(ctx, WorkerPut{WorkerID: "v1", Hostname: "h", JobStatus: "idle", LeaseSeconds: ip(5)}, t0); !errors.Is(err, ErrBadRequest) {
		t.Fatal("short lease accepted")
	}
}

func TestPolicyAndSettings(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.SetExclude(ctx, "h1", true); err != nil {
		t.Fatal(err)
	}
	s.SetIdleSince(ctx, "h1", &t0)
	s.RecordRequest(ctx, "h1", 17, t0)
	s.SetLastPowerupOK(ctx, "h2", &t0)
	p, err := s.HostPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h1 := p["h1"]
	if !h1.Exclude || h1.IdleSince == nil || !h1.IdleSince.Equal(t0) || *h1.LastRequestID != 17 {
		t.Fatalf("h1 %+v", h1)
	}
	if p["h2"].LastPowerupOKAt == nil || p["h2"].Exclude {
		t.Fatalf("h2 %+v", p["h2"])
	}
	s.SetIdleSince(ctx, "h1", nil)
	p, _ = s.HostPolicies(ctx)
	if p["h1"].IdleSince != nil || !p["h1"].Exclude {
		t.Fatal("clearing idle_since touched exclude or failed")
	}
	if lr, _ := s.LastPowerupRequest(ctx); !lr.IsZero() {
		t.Fatal("expected zero")
	}
	s.SetLastPowerupRequest(ctx, t0)
	if lr, _ := s.LastPowerupRequest(ctx); !lr.Equal(t0) {
		t.Fatal("last powerup request not stored")
	}
}

func TestReopenKeepsState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, _ := Open(path, testLimits())
	s.PutDemand(context.Background(), DemandPut{ID: "a", Source: "manual"}, t0)
	s.Close()
	s, err := Open(path, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetDemand(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
}

// host_tags seeds only hosts with no stored tags; stored tags (even empty) win.
func TestHostTagsSeedAndSet(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.SeedHostTags(ctx, map[string][]string{"v1": {"kernel"}, "v2": {"kernel"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHostTags(ctx, "v1", []string{"tiny", "big", "tiny"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHostTags(ctx, "v3", []string{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedHostTags(ctx, map[string][]string{"v1": {"kernel"}, "v3": {"kernel"}, "v4": {"kernel"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExclude(ctx, "v5", true); err != nil {
		t.Fatal(err)
	}
	p, err := s.HostPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"v1": {"big", "tiny"}, "v2": {"kernel"}, "v3": {}, "v4": {"kernel"}}
	for h, tags := range want {
		if got := p[h].Tags; got == nil || !slices.Equal(got, tags) {
			t.Fatalf("%s tags = %#v, want %#v", h, got, tags)
		}
	}
	if p["v5"].Tags != nil {
		t.Fatalf("v5 tags = %#v, want nil (never set)", p["v5"].Tags)
	}
	if _, err := s.SetHostTags(ctx, "v1", []string{"bad tag"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad tag err = %v", err)
	}
	if err := s.SeedHostTags(ctx, map[string][]string{"v6": {"bad tag"}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad seed err = %v", err)
	}
}
