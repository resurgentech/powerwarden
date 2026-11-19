package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

var powerModes = []string{"hold", "reserve", "dedicated"}

func (e *tenv) power(t *testing.T, id, token, action string) (int, []byte) {
	t.Helper()
	return e.do(t, "POST", "/v1/claims/"+id+"/power", token, fmt.Sprintf(`{"action":%q}`, action))
}

// powerClaim makes a ready claim of mode on v1 for jared.
func (e *tenv) powerClaim(t *testing.T, id, mode string) {
	t.Helper()
	code, c, b := e.putClaim(t, id, tokJared, fmt.Sprintf(`{"tags":["veritech01"],"purpose":"p","mode":%q}`, mode))
	if code != 200 || c.State != api.Ready {
		t.Fatalf("claim %s: %d %s", mode, code, b)
	}
}

func (e *tenv) busyWorker(t *testing.T, id, demand string) {
	t.Helper()
	n := 120
	if _, err := e.Store.PutWorker(context.Background(), store.WorkerPut{WorkerID: id, Hostname: v1, Tags: []string{"kernel"},
		JobStatus: "busy", DemandID: demand, LeaseSeconds: &n}, e.Clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func (e *tenv) wantErr(t *testing.T, what string, code int, b []byte, status int, ec api.ErrorCode) {
	t.Helper()
	if code != status || errCode(t, b) != ec {
		t.Fatalf("%s: want %d %s, got %d %s", what, status, ec, code, b)
	}
}

func (e *tenv) finishAll(t *testing.T, host string) {
	t.Helper()
	c, ok := e.PM.LastCommand(host)
	if !ok {
		t.Fatal("no command")
	}
	e.PM.Finish(c.ID, true)
	if err := e.App.Rec.SyncHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Pass()
}

// Each allowed action reaches powermaster with the claim reason, and the
// reply is powermaster's command object.
func TestClaimPowerAllowedActionsReachPowermaster(t *testing.T) {
	for _, mode := range []string{"hold", "reserve"} {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			// Already on: powerup is 200 and already there.
			code, b := e.power(t, "c1", tokJared, "powerup")
			if code != 200 {
				t.Fatalf("powerup: %d %s", code, b)
			}
			cmd := mustJSON[api.Command](t, b)
			if cmd.Action != "powerup" || cmd.Hostname != v1 || cmd.State != "succeeded" || cmd.Reason != "claim c1 by jared" || cmd.RequestedBy != "powerwarden" {
				t.Fatalf("powerup command: %+v", cmd)
			}
			code, b = e.power(t, "c1", tokJared, "reboot")
			if code != 202 {
				t.Fatalf("reboot: %d %s", code, b)
			}
			cmd = mustJSON[api.Command](t, b)
			if cmd.Action != "reboot" || cmd.State != "queued" || cmd.Reason != "claim c1 by jared" || cmd.Id == 0 {
				t.Fatalf("reboot command: %+v", cmd)
			}
			reqs := e.PM.Requests()
			if len(reqs) != 2 || reqs[0].Action != "powerup" || reqs[1].Action != "reboot" ||
				reqs[0].Reason != "claim c1 by jared" || reqs[1].Reason != "claim c1 by jared" || reqs[1].Hostname != v1 {
				t.Fatalf("requests: %+v", reqs)
			}
		})
	}
	t.Run("dedicated", func(t *testing.T) {
		e := setupClaims(t, nil)
		e.powerClaim(t, "c1", "dedicated")
		for _, step := range []struct {
			action string
			status int
		}{{"reboot", 202}, {"powerdown", 202}, {"powercut", 202}, {"powerup", 202}} {
			code, b := e.power(t, "c1", tokJared, step.action)
			if code != step.status {
				t.Fatalf("%s: %d %s", step.action, code, b)
			}
			if cmd := mustJSON[api.Command](t, b); cmd.Action != step.action || cmd.Reason != "claim c1 by jared" {
				t.Fatalf("%s command: %+v", step.action, cmd)
			}
			e.finishAll(t, v1)
		}
		if n := len(e.PM.Requests()); n != 4 {
			t.Fatalf("requests: %+v", e.PM.Requests())
		}
		if h := e.PM.Host(v1); h.State != pm.StateOn {
			t.Fatalf("host %+v", h)
		}
	})
}

func TestClaimPowerForbiddenAndOrder(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			// Another full-role token is not the owner.
			code, b := e.power(t, "c1", tokAnn, "reboot")
			e.wantErr(t, "other owner", code, b, 403, api.ErrorCodeForbidden)
			code, b = e.power(t, "c1", tokHourchd, "reboot")
			e.wantErr(t, "operator", code, b, 403, api.ErrorCodeForbidden)
			// Forbidden is checked before expired.
			e.Clock.Advance(16 * time.Minute)
			code, b = e.power(t, "c1", tokAnn, "reboot")
			e.wantErr(t, "other owner, lapsed", code, b, 403, api.ErrorCodeForbidden)
			if n := len(e.PM.Requests()); n != 0 {
				t.Fatalf("requests: %+v", e.PM.Requests())
			}
		})
	}
	e := setupClaims(t, nil)
	if code, b := e.power(t, "nope", tokJared, "reboot"); code != 404 {
		t.Fatalf("unknown claim: %d %s", code, b)
	}
	e.powerClaim(t, "c1", "hold")
	if code, b := e.do(t, "POST", "/v1/claims/c1/power", tokJared, `{"action":"explode"}`); code != 400 {
		t.Fatalf("bad action: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/v1/claims/c1/power", tokJared, `{}`); code != 400 {
		t.Fatalf("no action: %d %s", code, b)
	}
}

func TestClaimPowerExpired410(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			e.Clock.Advance(16 * time.Minute)
			// Expired is checked before not-ready and action-not-allowed.
			code, b := e.power(t, "c1", tokJared, "powercut")
			e.wantErr(t, "lapsed", code, b, 410, api.ErrorCodeClaimExpired)
			if n := len(e.PM.Requests()); n != 0 {
				t.Fatalf("requests: %+v", e.PM.Requests())
			}
		})
	}
}

func TestClaimPowerNotReady409(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode+"/not bound", func(t *testing.T) {
			e := setupClaims(t, nil)
			code, c, b := e.putClaim(t, "c1", tokJared, fmt.Sprintf(`{"tags":["nosuchhost"],"purpose":"p","mode":%q}`, mode))
			if code != 200 || c.State != api.Failed {
				t.Fatalf("claim: %d %s", code, b)
			}
			// Checked before action_not_allowed (powercut is not allowed on hold).
			code, b = e.power(t, "c1", tokJared, "powercut")
			e.wantErr(t, "failed claim", code, b, 410, api.ErrorCodeClaimFailed) // PWC11a: failed is final
			if n := len(e.PM.Requests()); n != 0 {
				t.Fatalf("requests: %+v", e.PM.Requests())
			}
		})
	}
	for _, mode := range []string{"reserve", "dedicated"} {
		t.Run(mode+"/draining", func(t *testing.T) {
			e := setupClaims(t, nil)
			e.busyWorker(t, "w1", "hourchd:j1")
			code, c, b := e.putClaim(t, "c1", tokJared, fmt.Sprintf(`{"tags":["veritech01"],"purpose":"p","mode":%q}`, mode))
			if code != 200 || c.State != api.Pending || c.Detail != "draining" {
				t.Fatalf("claim: %d %s", code, b)
			}
			code, b = e.power(t, "c1", tokJared, "reboot")
			e.wantErr(t, "draining", code, b, 409, api.ErrorCodeClaimNotReady)
		})
	}
	t.Run("dedicated/unbound pending", func(t *testing.T) {
		// A dedicated claim holds v1, so a second dedicated claim is pending, unbound.
		e := setupClaims(t, nil)
		e.powerClaim(t, "d1", "dedicated")
		if code, _, b := e.putClaim(t, "d2", tokJared, `{"tags":["veritech02"],"purpose":"p","mode":"dedicated"}`); code != 200 {
			t.Fatalf("d2: %d %s", code, b)
		}
		code, c, b := e.putClaim(t, "c3", tokJared, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
		if code != 200 || c.State != api.Pending || c.Hostname != nil {
			t.Fatalf("claim: %d %s", code, b)
		}
		code, b = e.power(t, "c3", tokJared, "reboot")
		e.wantErr(t, "pending unbound", code, b, 409, api.ErrorCodeClaimNotReady)
	})
}

func TestClaimPowerActionNotAllowed(t *testing.T) {
	for _, mode := range []string{"hold", "reserve"} {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			for _, action := range []string{"powerdown", "powercut"} {
				code, b := e.power(t, "c1", tokJared, action)
				e.wantErr(t, action, code, b, 409, api.ErrorCodeActionNotAllowed)
			}
			if n := len(e.PM.Requests()); n != 0 {
				t.Fatalf("requests: %+v", e.PM.Requests())
			}
			// Not allowed is checked before shared and busy.
			e.putClaim(t, "c2", tokAnn, `{"tags":["veritech01"],"purpose":"p"}`)
			e.busyWorker(t, "w1", "hourchd:j1")
			code, b := e.power(t, "c1", tokJared, "powerdown")
			e.wantErr(t, "powerdown with others", code, b, 409, api.ErrorCodeActionNotAllowed)
		})
	}
	// dedicated allows all four: covered by TestClaimPowerAllowedActionsReachPowermaster.
}

func TestClaimPowerHostShared(t *testing.T) {
	for _, mode := range []string{"hold", "reserve"} {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			if code, c, b := e.putClaim(t, "c2", tokAnn, `{"tags":["veritech01"],"purpose":"p"}`); code != 200 || *c.Hostname != v1 {
				t.Fatalf("second claim: %d %s", code, b)
			}
			code, b := e.power(t, "c1", tokJared, "reboot")
			e.wantErr(t, "shared", code, b, 409, api.ErrorCodeHostShared)
			// Shared is checked before busy.
			e.busyWorker(t, "w1", "hourchd:j1")
			code, b = e.power(t, "c1", tokJared, "reboot")
			e.wantErr(t, "shared and busy", code, b, 409, api.ErrorCodeHostShared)
			if n := len(e.PM.Requests()); n != 0 {
				t.Fatalf("requests: %+v", e.PM.Requests())
			}
			// The same owner's second claim does not make it shared; releasing does.
			e.do(t, "DELETE", "/v1/claims/c2", tokAnn, "")
			e.putClaim(t, "c3", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
			code, b = e.power(t, "c1", tokJared, "reboot")
			e.wantErr(t, "own claims only, busy", code, b, 409, api.ErrorCodeWorkerBusy)
		})
	}
}

func TestClaimPowerWorkerBusy(t *testing.T) {
	for _, mode := range []string{"hold", "reserve"} {
		for _, status := range []string{"busy", "maintenance"} {
			t.Run(mode+"/"+status, func(t *testing.T) {
				e := setupClaims(t, nil)
				e.powerClaim(t, "c1", mode)
				n := 120
				if _, err := e.Store.PutWorker(context.Background(), store.WorkerPut{WorkerID: "w1", Hostname: v1,
					Tags: []string{"kernel"}, JobStatus: status, DemandID: "hourchd:j1", LeaseSeconds: &n}, e.Clock.Now()); err != nil {
					t.Fatal(err)
				}
				code, b := e.power(t, "c1", tokJared, "reboot")
				e.wantErr(t, "busy", code, b, 409, api.ErrorCodeWorkerBusy)
				if len(e.PM.Requests()) != 0 {
					t.Fatalf("requests: %+v", e.PM.Requests())
				}
				// A stale worker row does not count.
				e.Clock.Advance(3 * time.Minute)
				if code, b := e.power(t, "c1", tokJared, "reboot"); code != 202 {
					t.Fatalf("after stale: %d %s", code, b)
				}
			})
		}
	}
}

func TestClaimPowerHostLockedNoForce(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			e.PM.SetLocked(v1, true)
			code, b := e.power(t, "c1", tokJared, "reboot")
			e.wantErr(t, "locked", code, b, 409, api.ErrorCodeHostLocked)
		})
	}
}

func TestClaimPowerCommandInFlight(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			if code, b := e.power(t, "c1", tokJared, "reboot"); code != 202 {
				t.Fatalf("reboot: %d %s", code, b)
			}
			code, b := e.power(t, "c1", tokJared, "powerup")
			e.wantErr(t, "in flight", code, b, 409, api.ErrorCodeCommandInFlight)
		})
	}
}

func TestClaimPowerPowermasterUnreachable502(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			e.PM.SetUnreachable(true)
			code, b := e.power(t, "c1", tokJared, "reboot")
			e.wantErr(t, "unreachable", code, b, 502, api.ErrorCodePowermasterUnreachable)
			// The failure is in the decision feed.
			found := false
			for _, d := range e.App.Decisions() {
				found = found || (d.Action == api.RequestFailed && strings.Contains(d.Detail, "reboot"))
			}
			if !found {
				t.Fatalf("no request_failed decision: %+v", e.App.Decisions())
			}
		})
	}
}

// A dedicated claim that is ready is not blocked by a busy worker: the
// command goes out, is logged at warn and is recorded as a decision.
func TestClaimPowerDedicatedOverBusyWorker(t *testing.T) {
	e := setupClaims(t, nil)
	e.powerClaim(t, "c1", "dedicated")
	e.busyWorker(t, "w1", "hourchd:j1")
	e.busyWorker(t, "w2", "hourchd:j2")
	code, b := e.power(t, "c1", tokJared, "powercut")
	if code != 409 { // powercut on an on host is powermaster's wrong_state, not a worker refusal
		t.Fatalf("powercut on a running host: %d %s", code, b)
	}
	code, b = e.power(t, "c1", tokJared, "reboot")
	if code != 202 {
		t.Fatalf("reboot over busy worker: %d %s", code, b)
	}
	cmd := mustJSON[api.Command](t, b)
	var req, over *api.Decision
	for _, d := range e.App.Decisions() {
		d := d
		switch d.Action {
		case api.ClaimPowerRequested:
			req = &d
		case api.ClaimPowerOverBusyWorker:
			over = &d
		}
	}
	if req == nil || over == nil {
		t.Fatalf("decisions: %+v", e.App.Decisions())
	}
	if req.CommandId == nil || *req.CommandId != cmd.Id || !strings.Contains(req.Detail, "claim c1 by jared") || !strings.Contains(req.Detail, "reboot") {
		t.Fatalf("requested: %+v", req)
	}
	for _, want := range []string{"claim c1", "jared", "reboot", "w1", "w2", "hourchd:j1", "hourchd:j2"} {
		if !strings.Contains(over.Detail, want) {
			t.Fatalf("over-busy detail lacks %q: %q", want, over.Detail)
		}
	}
	logs := e.Logs.String()
	warn := ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "level=WARN") && strings.Contains(l, "busy worker") {
			warn = l
		}
	}
	for _, want := range []string{"claim=c1", "owner=jared", "action=reboot", "w1", "w2", "hourchd:j1", "hourchd:j2"} {
		if !strings.Contains(warn, want) {
			t.Fatalf("warn log lacks %q: %q\nall: %s", want, warn, logs)
		}
	}
	// No busy worker: no over-busy decision.
	e2 := setupClaims(t, nil)
	e2.powerClaim(t, "c1", "dedicated")
	if code, b := e2.power(t, "c1", tokJared, "reboot"); code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	for _, d := range e2.App.Decisions() {
		if d.Action == api.ClaimPowerOverBusyWorker {
			t.Fatalf("unexpected: %+v", d)
		}
	}
}

// Every request that reaches powermaster is in the decision feed.
func TestClaimPowerRecordsDecision(t *testing.T) {
	for _, mode := range powerModes {
		t.Run(mode, func(t *testing.T) {
			e := setupClaims(t, nil)
			e.powerClaim(t, "c1", mode)
			code, b := e.power(t, "c1", tokJared, "reboot")
			if code != 202 {
				t.Fatalf("%d %s", code, b)
			}
			cmd := mustJSON[api.Command](t, b)
			n := 0
			for _, d := range e.App.Decisions() {
				if d.Action == api.ClaimPowerRequested {
					n++
					if d.CommandId == nil || *d.CommandId != cmd.Id || d.Hostname == nil || *d.Hostname != v1 {
						t.Fatalf("decision: %+v", d)
					}
				}
			}
			if n != 1 {
				t.Fatalf("want 1 claim_power_requested, got %d: %+v", n, e.App.Decisions())
			}
		})
	}
}

// A reboot (or a powerup after the host went off) on a hold claim makes it
// pending until the host is on again, then ready.
func TestClaimPowerRebootHoldPendingThenReady(t *testing.T) {
	e := setupClaims(t, nil)
	e.powerClaim(t, "c1", "hold")
	state := func() api.Claim {
		_, b := e.do(t, "GET", "/v1/claims/c1", tokJared, "")
		return mustJSON[api.Claim](t, b)
	}
	code, b := e.power(t, "c1", tokJared, "reboot")
	if code != 202 {
		t.Fatalf("reboot: %d %s", code, b)
	}
	if c := state(); c.State != api.Pending || c.Detail != "booting" {
		t.Fatalf("after reboot: %+v", c)
	}
	cmd := mustJSON[api.Command](t, b)
	e.PM.Run(cmd.Id)
	e.Pass()
	if c := state(); c.State != api.Pending {
		t.Fatalf("while running: %+v", c)
	}
	e.finishAll(t, v1)
	if c := state(); c.State != api.Ready || c.Detail != "" {
		t.Fatalf("after finish: %+v", c)
	}
	// The host went off: the claim waits for powerup; a powerup request is
	// allowed (the claim is bound) and keeps it pending until the host is on.
	e.PM.SetState(v1, pm.StateSoftOff)
	if err := e.App.Rec.SyncHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.App.BindClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := state(); c.State != api.Pending || c.Detail != "waiting for powerup" {
		t.Fatalf("off: %+v", c)
	}
	if code, b := e.power(t, "c1", tokJared, "powerup"); code != 202 {
		t.Fatalf("powerup: %d %s", code, b)
	}
	if c := state(); c.State != api.Pending || c.Detail != "booting" {
		t.Fatalf("after powerup: %+v", c)
	}
	e.finishAll(t, v1)
	if c := state(); c.State != api.Ready {
		t.Fatalf("after powerup finish: %+v", c)
	}
}
