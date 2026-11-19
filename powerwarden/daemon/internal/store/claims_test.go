package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

// bh builds an on host whose match tags are its name tags plus extra.
func bh(name string, extra ...string) BindHost {
	return BindHost{Name: name, Tags: append([]string{name, strings.SplitN(name, ".", 2)[0]}, extra...), State: pm.StateOn}
}

func cput(id, owner, mode string, tags ...string) ClaimPut {
	return ClaimPut{ID: id, Owner: owner, Session: "s1", Purpose: "test", Mode: mode, Tags: tags}
}

var twoHosts = []BindHost{bh("a.lab", "kernel"), bh("b.lab", "kernel")}

func mustClaim(t *testing.T, s *Store, in ClaimPut, hosts []BindHost, now time.Time) model.Claim {
	t.Helper()
	c, err := s.PutClaim(context.Background(), in, hosts, now)
	if err != nil {
		t.Fatalf("PutClaim %s: %v", in.ID, err)
	}
	return c
}

func TestClaimPutCreateRenewKeepsBinding(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := mustClaim(t, s, cput("c1", "jared", "hold", "a"), twoHosts, t0)
	if c.State != model.ClaimReady || c.Hostname != "a.lab" || c.Version != 1 || c.Session != "s1" ||
		c.LeaseSeconds != 900 || c.LeaseUntil == nil || !c.LeaseUntil.Equal(t0.Add(15*time.Minute)) ||
		!c.ExpiresAt.Equal(t0.Add(8*time.Hour)) {
		t.Fatalf("create: %+v", c)
	}
	in := cput("c1", "jared", "hold", "a")
	in.Session = "other"
	in.Purpose = "ignored"
	in.MaxSeconds = ip(3600)
	r := mustClaim(t, s, in, twoHosts, t0.Add(time.Minute))
	if r.Session != "s1" || r.Purpose != "test" || r.Hostname != "a.lab" || r.Version != 1 ||
		!r.CreatedAt.Equal(t0) || !r.ExpiresAt.Equal(t0.Add(8*time.Hour)) ||
		r.LeaseSeconds != 900 || !r.LeaseUntil.Equal(t0.Add(16*time.Minute)) {
		t.Fatalf("renew: %+v", r)
	}
	g, err := s.GetClaim(ctx, "c1")
	if err != nil || g.Hostname != "a.lab" {
		t.Fatalf("get: %+v %v", g, err)
	}
	// An explicit lease replaces the stored one; omitted keeps it.
	in.LeaseSeconds = ip(120)
	r = mustClaim(t, s, in, twoHosts, t0.Add(2*time.Minute))
	in.LeaseSeconds = nil
	r2 := mustClaim(t, s, in, twoHosts, t0.Add(3*time.Minute))
	if r.LeaseSeconds != 120 || r2.LeaseSeconds != 120 || !r2.LeaseUntil.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("lease kept: %+v %+v", r, r2)
	}
}

func TestClaimClamping(t *testing.T) {
	s, ctx := open(t), context.Background()
	lo := cput("lo", "o", "hold", "a")
	lo.LeaseSeconds, lo.MaxSeconds = ip(1), ip(1)
	c := mustClaim(t, s, lo, twoHosts, t0)
	if c.LeaseSeconds != 60 || !c.ExpiresAt.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("min clamp: %+v", c)
	}
	hi := cput("hi", "o", "hold", "a")
	hi.LeaseSeconds, hi.MaxSeconds = ip(999999), ip(999999999)
	c = mustClaim(t, s, hi, twoHosts, t0)
	if c.LeaseSeconds != 3600 || !c.ExpiresAt.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("max clamp: %+v", c)
	}
	e, err := s.ExtendClaim(ctx, "hi", "o", 1, t0.Add(time.Minute))
	if err != nil || !e.ExpiresAt.Equal(t0.Add(time.Minute+15*time.Minute)) {
		t.Fatalf("extend min: %+v %v", e, err)
	}
	e, err = s.ExtendClaim(ctx, "hi", "o", 1<<30, t0.Add(time.Minute))
	if err != nil || !e.ExpiresAt.Equal(t0.Add(time.Minute+72*time.Hour)) {
		t.Fatalf("extend max: %+v %v", e, err)
	}
}

func TestClaimValidation(t *testing.T) {
	s, ctx := open(t), context.Background()
	bad := map[string]ClaimPut{
		"no tags":        {ID: "a", Owner: "o", Purpose: "p"},
		"bad tag":        {ID: "a", Owner: "o", Purpose: "p", Tags: []string{"no way"}},
		"too many tags":  {ID: "a", Owner: "o", Purpose: "p", Tags: []string{"a", "b", "c", "d"}},
		"empty purpose":  {ID: "a", Owner: "o", Tags: []string{"a"}},
		"long purpose":   {ID: "a", Owner: "o", Purpose: strings.Repeat("x", 257), Tags: []string{"a"}},
		"bad session":    {ID: "a", Owner: "o", Purpose: "p", Tags: []string{"a"}, Session: "a b"},
		"bad mode":       {ID: "a", Owner: "o", Purpose: "p", Tags: []string{"a"}, Mode: "exclusive"},
		"bad id":         {ID: "a b", Owner: "o", Purpose: "p", Tags: []string{"a"}},
		"detached+lease": {ID: "a", Owner: "o", Purpose: "p", Tags: []string{"a"}, Detached: true, LeaseSeconds: ip(60)},
		"zero cap":       {ID: "a", Owner: "o", Purpose: "p", Tags: []string{"a"}, MaxSeconds: ip(0)},
	}
	for name, in := range bad {
		if _, err := s.PutClaim(ctx, in, twoHosts, t0); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: want bad request, got %v", name, err)
		}
	}
	if _, err := s.ExtendClaim(ctx, "nope", "o", 3600, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("extend unknown: %v", err)
	}
}

func TestClaimOwnershipAndLapse(t *testing.T) {
	s, ctx := open(t), context.Background()
	mustClaim(t, s, cput("c1", "jared", "hold", "a"), twoHosts, t0)
	now := t0.Add(time.Minute)
	if _, err := s.PutClaim(ctx, cput("c1", "mallory", "hold", "a"), twoHosts, now); !errors.Is(err, ErrForbidden) {
		t.Fatalf("put by other: %v", err)
	}
	if _, err := s.ExtendClaim(ctx, "c1", "mallory", 3600, now); !errors.Is(err, ErrForbidden) {
		t.Fatalf("extend by other: %v", err)
	}
	if err := s.ReleaseClaim(ctx, "c1", "mallory", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("release by other: %v", err)
	}
	if _, err := s.GetClaim(ctx, "c1"); err != nil {
		t.Fatal("forbidden release must not delete")
	}
	// Differing changes are conflicts.
	for name, in := range map[string]ClaimPut{
		"tags":     cput("c1", "jared", "hold", "b"),
		"mode":     cput("c1", "jared", "reserve", "a"),
		"detached": func() ClaimPut { p := cput("c1", "jared", "hold", "a"); p.Detached = true; return p }(),
	} {
		if _, err := s.PutClaim(ctx, in, twoHosts, now); !errors.Is(err, ErrConflict) {
			t.Errorf("%s change: want conflict, got %v", name, err)
		}
	}
	// Lease lapse (15m): renew and extend are 410; the row stays until swept.
	late := t0.Add(16 * time.Minute)
	if _, err := s.PutClaim(ctx, cput("c1", "jared", "hold", "a"), twoHosts, late); !errors.Is(err, ErrExpired) {
		t.Fatalf("renew after lapse: %v", err)
	}
	if _, err := s.ExtendClaim(ctx, "c1", "jared", 3600, late); !errors.Is(err, ErrExpired) {
		t.Fatalf("extend after lapse: %v", err)
	}
	if l, _ := s.ListClaims(ctx, late, ClaimFilter{}); len(l) != 0 {
		t.Fatalf("lapsed must be hidden: %+v", l)
	}
	if l, _ := s.ListClaims(ctx, late, ClaimFilter{IncludeExpired: true}); len(l) != 1 {
		t.Fatalf("include_expired: %+v", l)
	}
	if err := s.ReleaseClaim(ctx, "c1", "jared", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseClaim(ctx, "c1", "jared", false); err != nil {
		t.Fatalf("release of absent must be fine: %v", err)
	}
	if err := s.ReleaseClaim(ctx, "never", "jared", false); err != nil {
		t.Fatal(err)
	}
	// Operator release works across owners.
	mustClaim(t, s, cput("c2", "jared", "hold", "a"), twoHosts, t0)
	if err := s.ReleaseClaim(ctx, "c2", "op", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetClaim(ctx, "c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c2 should be gone: %v", err)
	}
}

func TestClaimExtendMovesOnlyExpiresAt(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := mustClaim(t, s, cput("c1", "jared", "hold", "a"), twoHosts, t0)
	e, err := s.ExtendClaim(ctx, "c1", "jared", 7200, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !e.ExpiresAt.Equal(t0.Add(time.Minute+2*time.Hour)) || !e.LeaseUntil.Equal(*c.LeaseUntil) ||
		e.Version != c.Version || e.Hostname != c.Hostname || !e.CreatedAt.Equal(c.CreatedAt) || e.LeaseSeconds != c.LeaseSeconds {
		t.Fatalf("extend: before %+v after %+v", c, e)
	}
}

func TestClaimCapacityCountsDetached(t *testing.T) {
	s, ctx := open(t), context.Background()
	for i := 0; i < 5; i++ {
		mustClaim(t, s, cput(fmt.Sprint("c", i), "o", "hold", "a"), twoHosts, t0)
	}
	d := cput("d", "o", "hold", "a")
	d.Detached = true
	mustClaim(t, s, d, twoHosts, t0)
	if _, err := s.PutClaim(ctx, cput("over", "o", "hold", "a"), twoHosts, t0); !errors.Is(err, ErrCapacity) {
		t.Fatalf("want capacity, got %v", err)
	}
	// A renew of an existing id is not a new claim.
	mustClaim(t, s, cput("c0", "o", "hold", "a"), twoHosts, t0)
	// Lapsed claims do not count.
	if _, err := s.PutClaim(ctx, cput("over", "o", "hold", "a"), twoHosts, t0.Add(20*time.Minute)); err != nil {
		t.Fatalf("lapsed claims must not count: %v", err)
	}
}

func TestClaimDetached(t *testing.T) {
	s, ctx := open(t), context.Background()
	in := cput("d", "o", "hold", "a")
	in.Detached = true
	in.MaxSeconds = ip(3600)
	c := mustClaim(t, s, in, twoHosts, t0)
	if c.LeaseUntil != nil || c.LeaseSeconds != 0 || !c.Detached {
		t.Fatalf("detached: %+v", c)
	}
	// Alive long past any lease, renew is a no-op.
	mid := t0.Add(50 * time.Minute)
	if l, _ := s.ListClaims(ctx, mid, ClaimFilter{}); len(l) != 1 {
		t.Fatalf("detached lapsed early: %+v", l)
	}
	r := mustClaim(t, s, in, twoHosts, mid)
	if r.LeaseUntil != nil || !r.ExpiresAt.Equal(c.ExpiresAt) {
		t.Fatalf("renew: %+v", r)
	}
	// Extend works.
	e, err := s.ExtendClaim(ctx, "d", "o", 7200, mid)
	if err != nil || !e.ExpiresAt.Equal(mid.Add(2*time.Hour)) || e.LeaseUntil != nil {
		t.Fatalf("extend: %+v %v", e, err)
	}
	// Lapses at expires_at.
	if l, _ := s.ListClaims(ctx, e.ExpiresAt, ClaimFilter{}); len(l) != 0 {
		t.Fatalf("must lapse at expires_at: %+v", l)
	}
	if _, err := s.PutClaim(ctx, in, twoHosts, e.ExpiresAt); !errors.Is(err, ErrExpired) {
		t.Fatalf("re-PUT after cap: %v", err)
	}
}

func TestClaimBindingDedicatedWaitsThenBinds(t *testing.T) {
	s, ctx := open(t), context.Background()
	hosts := []BindHost{bh("a.lab")}
	h := mustClaim(t, s, cput("h", "o", "hold", "a.lab"), hosts, t0)
	d := mustClaim(t, s, cput("d", "o2", "dedicated", "a.lab"), hosts, t0)
	if h.State != model.ClaimReady || d.State != model.ClaimPending || d.Hostname != "" ||
		d.Detail != "waiting for a.lab" || d.Version != 1 {
		t.Fatalf("hold %+v dedicated %+v", h, d)
	}
	// Nothing changes while the hold lives.
	if ch, err := s.BindClaims(ctx, hosts, t0.Add(time.Minute)); err != nil || len(ch) != 0 {
		t.Fatalf("bind: %+v %v", ch, err)
	}
	if err := s.ReleaseClaim(ctx, "h", "o", false); err != nil {
		t.Fatal(err)
	}
	ch, err := s.BindClaims(ctx, hosts, t0.Add(2*time.Minute))
	if err != nil || len(ch) != 1 || ch[0].ID != "d" || ch[0].State != model.ClaimReady ||
		ch[0].Hostname != "a.lab" || ch[0].Detail != "" || ch[0].Version != 2 {
		t.Fatalf("bind after release: %+v %v", ch, err)
	}
	// A hold now waits behind the dedicated claim.
	h2 := mustClaim(t, s, cput("h2", "o", "hold", "a.lab"), hosts, t0.Add(2*time.Minute))
	if h2.State != model.ClaimPending {
		t.Fatalf("hold behind dedicated: %+v", h2)
	}
	// Binding also frees when a claim lapses, with no release.
	late := t0.Add(2*time.Minute + 20*time.Minute) // d's lease (15m) lapsed
	ch, _ = s.BindClaims(ctx, hosts, late)
	if len(ch) != 0 { // h2 lapsed too: nothing live to bind
		t.Fatalf("lapsed pending must not bind: %+v", ch)
	}
}

func TestClaimBindingFailures(t *testing.T) {
	s := open(t)
	hosts := []BindHost{bh("a.lab", "kernel"), {Name: "b.lab", Tags: []string{"b.lab", "b", "kernel"}, Locked: true},
		{Name: "c.lab", Tags: []string{"c.lab", "c", "kernel"}, Excluded: true}}
	none := mustClaim(t, s, cput("none", "o", "hold", "gpu"), hosts, t0)
	if none.State != model.ClaimFailed || none.Detail != "no_host" || none.Hostname != "" {
		t.Fatalf("no match: %+v", none)
	}
	lk := mustClaim(t, s, cput("lk", "o", "hold", "b.lab"), hosts, t0)
	if lk.State != model.ClaimFailed || lk.Detail != "b.lab: locked" {
		t.Fatalf("locked: %+v", lk)
	}
	both := mustClaim(t, s, cput("both", "o", "hold", "kernel", "b", "c"), hosts, t0)
	if both.State != model.ClaimFailed || both.Detail != "no_host" { // b and c never share tags
		t.Fatalf("both: %+v", both)
	}
	// Two hosts, both unusable: each is named with its reason.
	lx := mustClaim(t, s, cput("lx", "o", "hold", "kernel"), hosts[1:], t0)
	if lx.State != model.ClaimFailed || lx.Detail != "b.lab: locked; c.lab: excluded" {
		t.Fatalf("locked+excluded: %+v", lx)
	}
	// Failed is final: freeing the host later does not revive it.
	hosts[1].Locked = false
	if ch, _ := s.BindClaims(context.Background(), hosts, t0.Add(time.Minute)); len(ch) != 0 {
		t.Fatalf("failed is final: %+v", ch)
	}
	// Locked hosts are dropped when another match is usable.
	ok := mustClaim(t, s, cput("ok", "o", "hold", "kernel"), []BindHost{
		{Name: "b.lab", Tags: []string{"b.lab", "kernel"}, Locked: true}, bh("a.lab", "kernel")}, t0)
	if ok.State != model.ClaimReady || ok.Hostname != "a.lab" {
		t.Fatalf("usable: %+v", ok)
	}
}

func TestClaimBindingPrefersRankThenName(t *testing.T) {
	s := open(t)
	hosts := []BindHost{bh("a.lab", "kernel"), bh("b.lab", "kernel")}
	hosts[1].Rank = -1
	c := mustClaim(t, s, cput("c", "o", "hold", "kernel"), hosts, t0)
	if c.Hostname != "b.lab" {
		t.Fatalf("rank: %+v", c)
	}
}

func TestClaimInventoryUnknownStaysPending(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := mustClaim(t, s, cput("c", "o", "hold", "a"), nil, t0)
	if c.State != model.ClaimPending || c.Hostname != "" {
		t.Fatalf("nil inventory: %+v", c)
	}
	ch, err := s.BindClaims(ctx, twoHosts, t0.Add(time.Second))
	if err != nil || len(ch) != 1 || ch[0].State != model.ClaimReady || ch[0].Hostname != "a.lab" {
		t.Fatalf("bind once inventory arrives: %+v %v", ch, err)
	}
}

func TestClaimConcurrentDedicatedBindsOne(t *testing.T) {
	s, ctx := open(t), context.Background()
	hosts := []BindHost{bh("a.lab")}
	var wg sync.WaitGroup
	res := make([]model.Claim, 8)
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.PutClaim(ctx, cput(fmt.Sprint("d", i), "o", "dedicated", "a.lab"), hosts, t0)
			if err != nil && !errors.Is(err, ErrCapacity) {
				t.Error(err)
			}
			res[i] = c
		}()
	}
	wg.Wait()
	ready, pending := 0, 0
	l, _ := s.ListClaims(ctx, t0, ClaimFilter{})
	for _, c := range l {
		switch c.State {
		case model.ClaimReady:
			ready++
		case model.ClaimPending:
			pending++
		}
	}
	if ready != 1 || pending != len(l)-1 || len(l) < 2 {
		t.Fatalf("ready=%d pending=%d of %d", ready, pending, len(l))
	}
}

func TestClaimListFiltersAndSweep(t *testing.T) {
	s, ctx := open(t), context.Background()
	mustClaim(t, s, cput("c1", "jared", "hold", "a"), twoHosts, t0)
	mustClaim(t, s, cput("c2", "ann", "hold", "b"), twoHosts, t0.Add(time.Second))
	if l, _ := s.ListClaims(ctx, t0, ClaimFilter{Owner: "ann"}); len(l) != 1 || l[0].ID != "c2" {
		t.Fatalf("owner: %+v", l)
	}
	if l, _ := s.ListClaims(ctx, t0, ClaimFilter{Hostname: "a.lab"}); len(l) != 1 || l[0].ID != "c1" {
		t.Fatalf("hostname: %+v", l)
	}
	if n, err := s.SweepClaims(ctx, t0.Add(20*time.Hour), 24*time.Hour); err != nil || n != 0 {
		t.Fatalf("early sweep: %d %v", n, err)
	}
	// Lease ended at t0+15m; swept once that is 24h past.
	if n, err := s.SweepClaims(ctx, t0.Add(25*time.Hour), 24*time.Hour); err != nil || n != 2 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if _, err := s.GetClaim(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("swept: %v", err)
	}
}

func TestMigrationTwoAddsClaims(t *testing.T) {
	s := open(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("user_version %d %v", v, err)
	}
}
