package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

// ---- PWC11a: a bound claim never moves ----

func off(h BindHost) BindHost { h.State = pm.StateSoftOff; return h }

func TestBoundClaimFailsWhenHostLeavesInventory(t *testing.T) {
	for _, mode := range []string{"hold", "reserve", "dedicated"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := open(t), context.Background()
			c := mustClaim(t, s, cput("c1", "o", mode, "a.lab"), twoHosts, t0)
			if c.State != model.ClaimReady || c.Hostname != "a.lab" {
				t.Fatalf("bind: %+v", c)
			}
			// b.lab is a match too, but a bound claim never moves.
			ch, err := s.BindClaims(ctx, []BindHost{bh("b.lab", "a.lab")}, t0.Add(time.Minute))
			if err != nil || len(ch) != 1 {
				t.Fatalf("bind: %+v %v", ch, err)
			}
			got := ch[0]
			if got.State != model.ClaimFailed || got.Hostname != "a.lab" || got.Detail != "host a.lab removed from inventory" || got.Version != 2 {
				t.Fatalf("removed: %+v", got)
			}
			// An unsynced inventory keeps a claim as is.
			c2 := mustClaim(t, s, cput("c2", "o", mode, "b.lab"), twoHosts, t0)
			if ch, _ := s.BindClaims(ctx, nil, t0.Add(time.Second)); len(ch) != 0 {
				t.Fatalf("nil inventory changed %+v (c2 %+v)", ch, c2)
			}
		})
	}
}

func TestBoundClaimFailsOnLockExcludeCooldownWhileOff(t *testing.T) {
	cases := map[string]struct {
		mut    func(*BindHost)
		detail string
	}{
		"locked":   {func(h *BindHost) { h.Locked = true }, "host a.lab locked"},
		"excluded": {func(h *BindHost) { h.Excluded = true }, "host a.lab excluded"},
		"cooldown": {func(h *BindHost) { h.Cooldown = true }, "boot failed on a.lab"},
	}
	for name, tc := range cases {
		for _, mode := range []string{"hold", "reserve"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				s, ctx := open(t), context.Background()
				mustClaim(t, s, cput("c1", "o", mode, "a.lab"), []BindHost{off(bh("a.lab"))}, t0)
				h := off(bh("a.lab"))
				tc.mut(&h)
				ch, err := s.BindClaims(ctx, []BindHost{h, bh("b.lab", "a.lab")}, t0.Add(time.Minute))
				if err != nil || len(ch) != 1 || ch[0].State != model.ClaimFailed || ch[0].Hostname != "a.lab" || ch[0].Detail != tc.detail {
					t.Fatalf("got %+v %v, want failed %q", ch, err, tc.detail)
				}
				// Final: the host coming back does not revive the claim.
				if ch, _ := s.BindClaims(ctx, twoHosts, t0.Add(2*time.Minute)); len(ch) != 0 {
					t.Fatalf("failed must be final: %+v", ch)
				}
			})
		}
	}
}

func TestBoundClaimOnOnHostSurvivesLockExcludeCooldown(t *testing.T) {
	for _, mode := range []string{"hold", "reserve"} {
		s, ctx := open(t), context.Background()
		mustClaim(t, s, cput("c1", "o", mode, "a.lab"), twoHosts, t0)
		for i, mut := range []func(*BindHost){
			func(h *BindHost) { h.Locked = true }, func(h *BindHost) { h.Excluded = true }, func(h *BindHost) { h.Cooldown = true }} {
			h := bh("a.lab", "kernel")
			mut(&h)
			ch, err := s.BindClaims(ctx, []BindHost{h, bh("b.lab", "kernel")}, t0.Add(time.Duration(i+1)*time.Minute))
			if err != nil || len(ch) != 0 {
				t.Fatalf("%s %d: on host must keep the claim: %+v %v", mode, i, ch, err)
			}
		}
	}
}

func TestDedicatedClaimNotFailedByLockExcludeCooldown(t *testing.T) {
	s, ctx := open(t), context.Background()
	mustClaim(t, s, cput("d", "o", "dedicated", "a.lab"), []BindHost{off(bh("a.lab"))}, t0)
	h := off(bh("a.lab"))
	h.Locked, h.Excluded, h.Cooldown = true, true, true
	if ch, err := s.BindClaims(ctx, []BindHost{h}, t0.Add(time.Minute)); err != nil || len(ch) != 0 {
		t.Fatalf("dedicated must hold: %+v %v", ch, err)
	}
}

func TestFailedClaimHoldsNoOccupancy(t *testing.T) {
	s, ctx := open(t), context.Background()
	mustClaim(t, s, cput("h", "o", "hold", "a.lab"), []BindHost{off(bh("a.lab"))}, t0)
	lk := off(bh("a.lab"))
	lk.Locked = true
	if ch, _ := s.BindClaims(ctx, []BindHost{lk}, t0.Add(time.Minute)); len(ch) != 1 || ch[0].State != model.ClaimFailed {
		t.Fatalf("fail: %+v", ch)
	}
	// The host is unlocked: a dedicated claim binds, the failed row does not block it.
	d := mustClaim(t, s, cput("d", "o2", "dedicated", "a.lab"), []BindHost{bh("a.lab")}, t0.Add(2*time.Minute))
	if d.State != model.ClaimReady || d.Hostname != "a.lab" {
		t.Fatalf("dedicated behind failed: %+v", d)
	}
}

func TestFailedClaimDoesNotCountTowardMaxClaims(t *testing.T) {
	s, ctx := open(t), context.Background()
	for i := 0; i < 5; i++ {
		mustClaim(t, s, cput(fmt.Sprint("c", i), "o", "hold", "a"), twoHosts, t0)
	}
	nh := mustClaim(t, s, cput("none", "o", "hold", "gpu"), twoHosts, t0) // failed no_host, the sixth row
	if nh.State != model.ClaimFailed {
		t.Fatalf("setup: %+v", nh)
	}
	if _, err := s.PutClaim(ctx, cput("ok", "o", "hold", "a"), twoHosts, t0); err != nil {
		t.Fatalf("failed claim counted toward max_claims: %v", err)
	}
	if _, err := s.PutClaim(ctx, cput("over", "o", "hold", "a"), twoHosts, t0); !errors.Is(err, ErrCapacity) {
		t.Fatalf("want capacity, got %v", err)
	}
}

func TestRenewAndExtendFailedClaimIs410(t *testing.T) {
	s, ctx := open(t), context.Background()
	mustClaim(t, s, cput("c1", "o", "hold", "a.lab"), []BindHost{off(bh("a.lab"))}, t0)
	lk := off(bh("a.lab"))
	lk.Locked = true
	s.BindClaims(ctx, []BindHost{lk}, t0.Add(time.Minute))
	_, err := s.PutClaim(ctx, cput("c1", "o", "hold", "a.lab"), twoHosts, t0.Add(2*time.Minute))
	var cf *ClaimFailedError
	if !errors.Is(err, ErrClaimFailed) || !errors.As(err, &cf) || cf.Detail != "host a.lab locked" {
		t.Fatalf("renew: %v", err)
	}
	if _, err := s.ExtendClaim(ctx, "c1", "o", 3600, t0.Add(2*time.Minute)); !errors.Is(err, ErrClaimFailed) {
		t.Fatalf("extend: %v", err)
	}
	// Another owner is still forbidden, not told the detail.
	if _, err := s.ExtendClaim(ctx, "c1", "x", 3600, t0.Add(2*time.Minute)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("extend other owner: %v", err)
	}
	// Renew changed nothing: updated_at stays fixed.
	g, _ := s.GetClaim(ctx, "c1")
	if !g.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("updated_at moved: %v", g.UpdatedAt)
	}
}

func TestFailedClaimSweptAfterFailure(t *testing.T) {
	s, ctx := open(t), context.Background()
	d := cput("c1", "o", "hold", "a.lab")
	d.Detached = true
	d.MaxSeconds = ip(72 * 3600)
	mustClaim(t, s, d, []BindHost{off(bh("a.lab"))}, t0)
	lk := off(bh("a.lab"))
	lk.Locked = true
	s.BindClaims(ctx, []BindHost{lk}, t0.Add(time.Hour))
	if n, _ := s.SweepClaims(ctx, t0.Add(24*time.Hour), 24*time.Hour); n != 0 {
		t.Fatalf("swept early: %d", n)
	}
	if n, _ := s.SweepClaims(ctx, t0.Add(25*time.Hour+time.Second), 24*time.Hour); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
}
