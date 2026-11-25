package keeper_test

import (
	"context"
	"testing"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/cli/internal/keeper"
	"github.com/resurgentech/powerwarden/powerwarden/cli/internal/pwfake"
)

// PWC11a: a renew that learns the claim failed marks it lost with the
// server's detail, and renewal stops.
func TestRenewalClaimFailedMarksLostWithDetail(t *testing.T) {
	for name, respond := range map[string]func(c pw.Claim) (int, any){
		"410 claim_failed": func(pw.Claim) (int, any) {
			return 410, pwfake.Err(pw.ErrorCodeClaimFailed, "host box1.labtwo locked")
		},
		"200 failed (older server)": func(c pw.Claim) (int, any) {
			c.State, c.Detail = pw.Failed, "host box1.labtwo locked"
			return 200, c
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, k, clk := setup(t)
			c, err := k.Create(context.Background(), keeper.Spec{Tags: []string{"k"}, Purpose: "p", LeaseSeconds: 60, MaxSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, "timer", func() bool { return clk.Waiters() == 1 })
			f.Respond = func(r pwfake.Req) (int, any, bool) {
				if r.Method == "PUT" {
					st, body := respond(*c)
					return st, body, true
				}
				return 0, nil, false
			}
			clk.Advance(20 * time.Second)
			waitFor(t, "lost", func() bool { return len(k.Lost()) == 1 })
			l := k.Lost()[0]
			if l.ID != c.Id || !l.Failed || l.Reason != "host box1.labtwo locked" {
				t.Fatalf("lost = %+v", l)
			}
			clk.Advance(time.Hour)
			time.Sleep(20 * time.Millisecond)
			if n := f.Count("PUT", "/v1/claims/"+c.Id); n != 2 {
				t.Fatalf("%d PUTs, want 2 (renewal must stop)", n)
			}
		})
	}
}
