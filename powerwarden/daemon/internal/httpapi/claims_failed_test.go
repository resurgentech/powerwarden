package httpapi

import (
	"fmt"
	"strings"
	"testing"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

// ---- PWC11a: a failed claim is final and holds nothing ----

// failClaim makes jared's ready claim id of mode on v1, then powers v1 off
// and locks it: the claim fails with "host <v1> locked".
func (e *tenv) failClaim(t *testing.T, id, mode string) {
	t.Helper()
	e.powerClaim(t, id, mode)
	e.PM.SetState(v1, pm.StateSoftOff)
	e.PM.SetLocked(v1, true)
	e.resync(t)
	_, b := e.do(t, "GET", "/v1/claims/"+id, tokJared, "")
	if c := mustJSON[api.Claim](t, b); c.State != api.Failed || c.Detail != "host "+v1+" locked" {
		t.Fatalf("setup: %s", b)
	}
}

func TestFailedClaimRenewExtendPower410(t *testing.T) {
	for _, mode := range []string{"hold", "reserve"} {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.failClaim(t, "c1", mode)
			body := fmt.Sprintf(`{"tags":["veritech01"],"purpose":"p","mode":%q}`, mode)
			code, _, b := e.putClaim(t, "c1", tokJared, body)
			e.wantErr(t, "renew", code, b, 410, api.ErrorCodeClaimFailed)
			if m := mustJSON[api.Error](t, b).Message; !strings.Contains(m, "host "+v1+" locked") {
				t.Fatalf("message lacks the detail: %q", m)
			}
			code, b = e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":3600}`)
			e.wantErr(t, "extend", code, b, 410, api.ErrorCodeClaimFailed)
			code, b = e.power(t, "c1", tokJared, "powerup")
			e.wantErr(t, "power", code, b, 410, api.ErrorCodeClaimFailed)
			if n := len(e.PM.Requests()); n != 0 {
				t.Fatalf("requests: %+v", e.PM.Requests())
			}
			// Another token is still forbidden.
			code, b = e.power(t, "c1", tokAnn, "powerup")
			e.wantErr(t, "other owner", code, b, 403, api.ErrorCodeForbidden)
			// GET shows it: hostname kept, no host_state. It is listed. DELETE is 204.
			code, b = e.do(t, "GET", "/v1/claims/c1", tokJared, "")
			c := mustJSON[api.Claim](t, b)
			if code != 200 || c.State != api.Failed || c.Hostname == nil || *c.Hostname != v1 || c.HostState != nil {
				t.Fatalf("get: %d %s", code, b)
			}
			if _, b := e.do(t, "GET", "/v1/claims", tokJared, ""); len(mustJSON[[]api.Claim](t, b)) != 1 {
				t.Fatalf("list: %s", b)
			}
			if code, _ := e.do(t, "DELETE", "/v1/claims/c1", tokJared, ""); code != 204 {
				t.Fatalf("delete: %d", code)
			}
		})
	}
	// An unbound failed claim (no_host) is final too.
	e := setupClaims(t, nil)
	e.putClaim(t, "n", tokJared, `{"tags":["nosuchhost"],"purpose":"p"}`)
	code, _, b := e.putClaim(t, "n", tokJared, `{"tags":["nosuchhost"],"purpose":"p"}`)
	e.wantErr(t, "renew no_host", code, b, 410, api.ErrorCodeClaimFailed)
}

func TestFailedClaimHoldsNothingOnHostView(t *testing.T) {
	e := setupClaims(t, nil)
	e.failClaim(t, "r", "reserve")
	e.PM.SetLocked(v1, false)
	e.PM.SetState(v1, pm.StateOn)
	e.resync(t)
	_, b := e.do(t, "GET", "/v1/hosts/"+v1, tokJared, "")
	h := mustJSON[api.HostView](t, b)
	if h.JobsBlocked || len(h.Claims) != 0 {
		t.Fatalf("failed claim shown on host: %+v", h)
	}
	// host_state is not tracked: the version stays put as the host changes.
	_, b = e.do(t, "GET", "/v1/claims/r", tokJared, "")
	before := mustJSON[api.Claim](t, b)
	e.PM.SetState(v1, pm.StateSoftOff)
	e.resync(t)
	_, b = e.do(t, "GET", "/v1/claims/r", tokJared, "")
	if after := mustJSON[api.Claim](t, b); after.Version != before.Version || after.HostState != nil {
		t.Fatalf("version moved on a host change: %+v -> %+v", before, after)
	}
	// A dedicated claim binds the host.
	code, c, b := e.putClaim(t, "d", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
	if code != 200 || c.State != api.Ready {
		t.Fatalf("dedicated: %d %s", code, b)
	}
}

func TestClaimWaitWakesOnFail(t *testing.T) {
	e := setupClaims(t, nil)
	e.powerClaim(t, "c1", "hold")
	e.resync(t)
	ch := e.getAsync(t, "/v1/claims/c1?since=1&wait=30", tokJared)
	e.blocked(t, "c1", 1)
	e.pending(t, ch)
	e.PM.SetState(v1, pm.StateSoftOff)
	e.PM.SetLocked(v1, true)
	e.resync(t)
	r := recv(t, ch)
	got := mustJSON[api.Claim](t, r.body)
	if r.code != 200 || got.State != api.Failed || got.Detail != "host "+v1+" locked" {
		t.Fatalf("%d %s", r.code, r.body)
	}
}
