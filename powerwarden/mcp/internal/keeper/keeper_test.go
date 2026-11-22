package keeper_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

func setup(t *testing.T) (*pwfake.Fake, *keeper.Keeper, *keeper.FakeClock) {
	t.Helper()
	f := pwfake.New()
	t.Cleanup(f.Srv.Close)
	api, err := client.New(client.Config{URL: f.Srv.URL}, "tok", f.Srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	clk := keeper.NewFakeClock()
	k := keeper.New(api, keeper.Options{Prefix: "pwrwrdn", Session: "s-abc123abc123", Clock: clk})
	t.Cleanup(k.Close)
	return f, k, clk
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCreateSendsSessionAndNoDetached(t *testing.T) {
	f, k, _ := setup(t)
	c, err := k.Create(context.Background(), keeper.Spec{Tags: []string{"kernel"}, Purpose: "p", Mode: pw.Hold, MaxSeconds: 3600})
	if err != nil || c == nil {
		t.Fatalf("create: %v %v", c, err)
	}
	if !strings.HasPrefix(c.Id, "pwrwrdn-") {
		t.Fatalf("id %q lacks prefix", c.Id)
	}
	r, ok := f.Last("PUT", "/v1/claims/"+c.Id)
	if !ok {
		t.Fatal("no PUT")
	}
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	if m["session"] != "s-abc123abc123" {
		t.Fatalf("session = %v", m["session"])
	}
	if _, has := m["detached"]; has {
		t.Fatalf("detached sent: %v", m)
	}
	if r.Auth != "Bearer tok" {
		t.Fatalf("auth %q", r.Auth)
	}
	if !k.Owns(c.Id) {
		t.Fatal("not owned")
	}
}

// Lease 90s renews every 30s (lease/3), carrying tags, mode and session, and
// never max_seconds.
func TestRenewalCadence(t *testing.T) {
	f, k, clk := setup(t)
	ls := 90
	c, err := k.Create(context.Background(), keeper.Spec{Tags: []string{"kernel"}, Purpose: "p", Mode: pw.Reserve, MaxSeconds: 3600, LeaseSeconds: ls})
	if err != nil || c == nil {
		t.Fatalf("create: %v", err)
	}
	path := "/v1/claims/" + c.Id
	waitFor(t, "timer", func() bool { return clk.Waiters() == 1 })

	clk.Advance(29 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if n := f.Count("PUT", path); n != 1 {
		t.Fatalf("renewed early: %d PUTs", n)
	}
	for i := 2; i <= 4; i++ {
		clk.Advance(time.Second) // reaches 30s, then each next 30s
		want := i
		waitFor(t, "renewal", func() bool { return f.Count("PUT", path) == want && clk.Waiters() == 1 })
		clk.Advance(29 * time.Second)
	}
	r, _ := f.Last("PUT", path)
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	if _, has := m["max_seconds"]; has {
		t.Fatalf("renew sent max_seconds: %v", m)
	}
	if m["mode"] != "reserve" || m["session"] != "s-abc123abc123" {
		t.Fatalf("renew body %v", m)
	}
	if len(k.Lost()) != 0 {
		t.Fatalf("lost: %v", k.Lost())
	}
}

func TestRenewalRefusalMarksLost(t *testing.T) {
	for _, st := range []int{404, 410, 403} {
		f, k, clk := setup(t)
		c, err := k.Create(context.Background(), keeper.Spec{Tags: []string{"k"}, Purpose: "p", LeaseSeconds: 60, MaxSeconds: 3600})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "timer", func() bool { return clk.Waiters() == 1 })
		status := st
		f.Respond = func(r pwfake.Req) (int, any, bool) {
			if r.Method == "PUT" {
				return status, pwfake.Err(pw.ErrorCodeClaimExpired, "gone"), true
			}
			return 0, nil, false
		}
		clk.Advance(20 * time.Second)
		waitFor(t, "lost", func() bool { return len(k.Lost()) == 1 })
		l := k.Lost()[0]
		if l.ID != c.Id || !strings.Contains(l.Reason, "claim_expired") {
			t.Fatalf("status %d lost = %+v", st, l)
		}
		// no more renewals once lost
		clk.Advance(time.Hour)
		time.Sleep(20 * time.Millisecond)
		if n := f.Count("PUT", "/v1/claims/"+c.Id); n != 2 {
			t.Fatalf("status %d: %d PUTs, want 2", st, n)
		}
		k.Ack(c.Id)
		if len(k.Lost()) != 0 {
			t.Fatal("ack did not clear")
		}
	}
}

func TestRenewalServerErrorIsTransient(t *testing.T) {
	f, k, clk := setup(t)
	c, _ := k.Create(context.Background(), keeper.Spec{Tags: []string{"k"}, Purpose: "p", LeaseSeconds: 60, MaxSeconds: 3600})
	waitFor(t, "timer", func() bool { return clk.Waiters() == 1 })
	f.Respond = func(r pwfake.Req) (int, any, bool) {
		if r.Method == "PUT" {
			return http.StatusBadGateway, pwfake.Err(pw.ErrorCodePowermasterUnreachable, "x"), true
		}
		return 0, nil, false
	}
	clk.Advance(20 * time.Second)
	waitFor(t, "second PUT", func() bool { return f.Count("PUT", "/v1/claims/"+c.Id) == 2 && clk.Waiters() == 1 })
	if len(k.Lost()) != 0 {
		t.Fatalf("5xx marked lost: %v", k.Lost())
	}
}

func TestReleaseAndReleaseAll(t *testing.T) {
	f, k, _ := setup(t)
	a, _ := k.Create(context.Background(), keeper.Spec{Tags: []string{"k"}, Purpose: "p"})
	b, _ := k.Create(context.Background(), keeper.Spec{Tags: []string{"k"}, Purpose: "p"})
	if err := k.Release(context.Background(), a.Id); err != nil {
		t.Fatal(err)
	}
	if f.Count("DELETE", "/v1/claims/"+a.Id) != 1 || k.Owns(a.Id) {
		t.Fatal("release did not delete/forget")
	}
	k.ReleaseAll(context.Background())
	if f.Count("DELETE", "/v1/claims/"+b.Id) != 1 {
		t.Fatal("ReleaseAll did not delete b")
	}
	if f.Count("DELETE", "/v1/claims/"+a.Id) != 1 {
		t.Fatal("ReleaseAll deleted a again")
	}
}

func TestCreateRefusalIsRefusal(t *testing.T) {
	f, k, _ := setup(t)
	f.Respond = func(r pwfake.Req) (int, any, bool) {
		return 429, pwfake.Err(pw.ErrorCodeCapacity, "full"), true
	}
	_, err := k.Create(context.Background(), keeper.Spec{Tags: []string{"k"}, Purpose: "p"})
	var ref *client.Refusal
	if !asRefusal(err, &ref) || ref.Code != "capacity" || ref.Status != 429 {
		t.Fatalf("err = %v", err)
	}
}

func asRefusal(err error, out **client.Refusal) bool {
	r, ok := err.(*client.Refusal)
	if ok {
		*out = r
	}
	return ok
}
