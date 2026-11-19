package httpapi

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

type waitRes struct {
	code int
	body []byte
}

// getAsync starts a GET that may block and returns its result channel.
func (e *tenv) getAsync(t *testing.T, path, token string) <-chan waitRes {
	t.Helper()
	ch := make(chan waitRes, 1)
	go func() {
		req, b := e.doNoFatal(path, token)
		ch <- waitRes{req, b}
	}()
	return ch
}

// doNoFatal is a GET that is safe off the test goroutine.
func (e *tenv) doNoFatal(path, token string) (int, []byte) {
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// blocked waits until n blocking reads are parked on id.
func (e *tenv) blocked(t *testing.T, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.App.ClaimWaiters(id) != n {
		if time.Now().After(deadline) {
			t.Fatalf("waiters on %s: %d, want %d", id, e.App.ClaimWaiters(id), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func recv(t *testing.T, ch <-chan waitRes) waitRes {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("blocking read did not return")
		return waitRes{}
	}
}

func (e *tenv) pending(t *testing.T, ch <-chan waitRes) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("returned early: %d %s", r.code, r.body)
	default:
	}
}

func (e *tenv) resync(t *testing.T) {
	t.Helper()
	if err := e.App.Rec.SyncHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Pass()
}

func TestClaimWaitImmediateWhenSinceDiffers(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	for _, q := range []string{"?since=0&wait=30", "?wait=30", "?since=1&wait=0", "?since=7"} {
		ch := e.getAsync(t, "/v1/claims/c1"+q, tokJared)
		r := recv(t, ch)
		if r.code != 200 {
			t.Fatalf("%s: %d %s", q, r.code, r.body)
		}
		if got := mustJSON[api.Claim](t, r.body); got.Version != 1 {
			t.Fatalf("%s: %+v", q, got)
		}
	}
	if n := e.App.ClaimWaiters("c1"); n != 0 {
		t.Fatalf("waiters %d", n)
	}
	for _, q := range []string{"?since=x", "?since=-1", "?wait=x", "?wait=-1"} {
		if code, b := e.do(t, "GET", "/v1/claims/c1"+q, tokJared, ""); code != 400 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
	}
}

func TestClaimWaitWakesOnStateChange(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "ann", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
	_, c, _ := e.putClaim(t, "j", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	if c.State == api.Ready || c.Version != 1 {
		t.Fatalf("setup: %+v", c)
	}
	ch := e.getAsync(t, "/v1/claims/j?since=1&wait=30", tokJared)
	e.blocked(t, "j", 1)
	e.pending(t, ch)
	if code, b := e.do(t, "DELETE", "/v1/claims/ann", tokAnn, ""); code != 204 {
		t.Fatalf("%d %s", code, b)
	}
	r := recv(t, ch)
	got := mustJSON[api.Claim](t, r.body)
	if r.code != 200 || got.State != api.Ready || got.Version != 2 {
		t.Fatalf("%d %s", r.code, r.body)
	}
}

func TestClaimWaitWakesOnHostState(t *testing.T) {
	e := setupClaims(t, nil)
	_, c, _ := e.putClaim(t, "d", tokJared, `{"tags":["veritech02"],"purpose":"p","mode":"dedicated"}`)
	if c.State != api.Ready || c.Version != 1 || *c.HostState != api.On {
		t.Fatalf("setup: %+v", c)
	}
	e.resync(t) // first look at the claim: no change
	ch := e.getAsync(t, "/v1/claims/d?since=1&wait=30", tokJared)
	e.blocked(t, "d", 1)
	e.PM.SetState(v2, pm.StateSoftOff)
	e.resync(t)
	r := recv(t, ch)
	got := mustJSON[api.Claim](t, r.body)
	if r.code != 200 || got.Version != 2 || got.State != api.Ready || *got.HostState != api.SoftOff {
		t.Fatalf("%d %s", r.code, r.body)
	}
	// The stored version moved, so a caller with since=1 never misses it.
	_, b := e.do(t, "GET", "/v1/claims/d?since=1&wait=30", tokJared, "")
	if mustJSON[api.Claim](t, b).Version != 2 {
		t.Fatalf("%s", b)
	}
	// An idle pass bumps nothing.
	e.resync(t)
	_, b = e.do(t, "GET", "/v1/claims/d", tokJared, "")
	if mustJSON[api.Claim](t, b).Version != 2 {
		t.Fatalf("%s", b)
	}
}

func TestClaimWaitDeleteAndLapse(t *testing.T) {
	e := setupClaims(t, func(c *config.Config) { c.Timings.ClaimWaitMax = 10 * time.Minute })
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	ch := e.getAsync(t, "/v1/claims/c1?since=1&wait=300", tokJared)
	e.blocked(t, "c1", 1)
	if code, b := e.do(t, "DELETE", "/v1/claims/c1", tokJared, ""); code != 204 {
		t.Fatalf("%d %s", code, b)
	}
	if r := recv(t, ch); r.code != 404 || errCode(t, r.body) != api.ErrorCodeNotFound {
		t.Fatalf("deleted: %d %s", r.code, r.body)
	}

	e.putClaim(t, "c2", tokJared, `{"tags":["veritech02"],"purpose":"p","lease_seconds":60}`)
	ch = e.getAsync(t, "/v1/claims/c2?since=1&wait=300", tokJared)
	e.blocked(t, "c2", 1)
	e.Clock.Advance(61 * time.Second)
	if r := recv(t, ch); r.code != 410 || errCode(t, r.body) != api.ErrorCodeClaimExpired {
		t.Fatalf("lapsed: %d %s", r.code, r.body)
	}
	e.blocked(t, "c2", 0)
}

func TestClaimWaitTimeoutReturnsUnchanged(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	ch := e.getAsync(t, "/v1/claims/c1?since=1&wait=30", tokJared)
	e.blocked(t, "c1", 1)
	e.Clock.Advance(29 * time.Second)
	e.pending(t, ch)
	e.Clock.Advance(time.Second)
	r := recv(t, ch)
	if got := mustJSON[api.Claim](t, r.body); r.code != 200 || got.Version != 1 || got.State != api.Ready {
		t.Fatalf("%d %s", r.code, r.body)
	}
	e.blocked(t, "c1", 0)
}

func TestClaimWaitRenewalDoesNotWake(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	ch := e.getAsync(t, "/v1/claims/c1?since=1&wait=60", tokJared)
	e.blocked(t, "c1", 1)
	e.Clock.Advance(10 * time.Second)
	code, again, _ := e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	if code != 200 || again.Version != 1 {
		t.Fatalf("renew: %d %+v", code, again)
	}
	if code, b := e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":7200}`); code != 200 {
		t.Fatalf("extend: %d %s", code, b)
	}
	e.resync(t)
	e.pending(t, ch)
	e.Clock.Advance(50 * time.Second)
	r := recv(t, ch)
	if got := mustJSON[api.Claim](t, r.body); r.code != 200 || got.Version != 1 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if n := e.App.WaitWakes(); n != 0 {
		t.Fatalf("renewal or extend woke the waiter %d times", n)
	}
}

func TestClaimWaitClampedToConfig(t *testing.T) {
	e := setupClaims(t, nil)
	if got := e.App.ClampWait(time.Hour); got != time.Minute {
		t.Fatalf("clamp: %v", got)
	}
	if got := e.App.ClampWait(5 * time.Second); got != 5*time.Second {
		t.Fatalf("clamp: %v", got)
	}
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	ch := e.getAsync(t, "/v1/claims/c1?since=1&wait=3600", tokJared)
	e.blocked(t, "c1", 1)
	e.Clock.Advance(59 * time.Second)
	e.pending(t, ch)
	e.Clock.Advance(time.Second)
	if r := recv(t, ch); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	// wait omitted with since given is the configured maximum.
	ch = e.getAsync(t, "/v1/claims/c1?since=1", tokJared)
	e.blocked(t, "c1", 1)
	e.Clock.Advance(time.Minute)
	if r := recv(t, ch); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
}
