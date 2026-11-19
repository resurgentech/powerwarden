package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/decisions"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// PowerError is a refusal of a claim power request with its HTTP status and
// API error code.
type PowerError struct {
	Status int
	Code   api.ErrorCode
	Msg    string
}

func (e *PowerError) Error() string { return e.Msg }

func refuse(status int, code api.ErrorCode, format string, args ...any) error {
	return &PowerError{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// claimActions lists what a claim of each mode may ask for.
var claimActions = map[string][]string{
	model.ClaimHold:      {pm.ActionPowerup, pm.ActionReboot},
	model.ClaimReserve:   {pm.ActionPowerup, pm.ActionReboot},
	model.ClaimDedicated: {pm.ActionPowerup, pm.ActionPowerdown, pm.ActionPowercut, pm.ActionReboot},
}

// ClaimPower sends a power command for the host of claim id on behalf of its
// owner, using powerwarden's own powermaster token. Only the owner may ask
// (an operator token that is not the owner is refused too). Refusals come in
// the order of the spec: forbidden, expired, failed (410 claim_failed), not ready, action not allowed,
// then for hold and reserve host shared and worker busy; powermaster's own
// answers (host_locked, command_in_flight) pass through. The status is 202
// when the command was queued and 200 when the host is already there.
//
// A dedicated claim that is ready is never refused for a busy worker; the
// command is logged and recorded as claim_power_over_busy_worker instead.
func (a *App) ClaimPower(ctx context.Context, by Caller, id, action string) (api.Command, int, error) {
	now := a.Clock.Now()
	c, err := a.Store.GetClaim(ctx, id)
	if err != nil {
		return api.Command{}, 0, err
	}
	switch {
	case c.Owner != by.Name:
		return api.Command{}, 0, fmt.Errorf("%w: claim %s belongs to another owner", store.ErrForbidden, id)
	case !c.Live(now):
		return api.Command{}, 0, fmt.Errorf("%w: claim %s lapsed", store.ErrExpired, id)
	case c.State == model.ClaimFailed:
		return api.Command{}, 0, store.FailedErr(c)
	case c.Hostname == "" || c.Detail == store.DetailDraining:
		return api.Command{}, 0, refuse(http.StatusConflict, api.ErrorCodeClaimNotReady, "claim %s is not ready (%s)", id, notReadyWhy(c))
	}
	allowed := claimActions[c.Mode]
	if !contains(allowed, action) {
		return api.Command{}, 0, refuse(http.StatusConflict, api.ErrorCodeActionNotAllowed,
			"a %s claim may ask for %s, not %s", c.Mode, strings.Join(allowed, ", "), action)
	}
	ws, err := a.Store.ListWorkers(ctx, now, false)
	if err != nil {
		return api.Command{}, 0, err
	}
	var busy []model.Worker
	for _, w := range ws {
		if w.Hostname == c.Hostname && (w.JobStatus == model.JobBusy || w.JobStatus == model.JobMaintenance) {
			busy = append(busy, w)
		}
	}
	if c.Mode != model.ClaimDedicated {
		others, err := a.Store.ListClaims(ctx, now, store.ClaimFilter{Hostname: c.Hostname})
		if err != nil {
			return api.Command{}, 0, err
		}
		for _, o := range others {
			if o.Owner != c.Owner && o.State != model.ClaimFailed {
				return api.Command{}, 0, refuse(http.StatusConflict, api.ErrorCodeHostShared,
					"%s is shared with another owner's claim %s", c.Hostname, o.ID)
			}
		}
		if len(busy) > 0 {
			return api.Command{}, 0, refuse(http.StatusConflict, api.ErrorCodeWorkerBusy,
				"worker %s on %s is %s", busy[0].WorkerID, c.Hostname, busy[0].JobStatus)
		}
	}

	reason := fmt.Sprintf("claim %s by %s", c.ID, c.Owner)
	cmd, err := a.Rec.PM.Power(ctx, c.Hostname, action, reason)
	if err != nil {
		return api.Command{}, 0, a.powerFailed(c, action, err)
	}
	a.Rec.NoteCommand(ctx, cmd)
	cid := cmd.ID
	a.Log.Info("claim power request", "claim", c.ID, "owner", c.Owner, "action", action, "host", c.Hostname,
		"command_id", cid, "command_state", cmd.State)
	a.Ring.Add(decisions.Decision{Ts: now, Action: decisions.ClaimPowerRequested, Hostname: c.Hostname, CommandID: &cid,
		Detail: fmt.Sprintf("%s: %s, command %s", reason, action, cmd.State)})
	if c.Mode == model.ClaimDedicated && len(busy) > 0 && cmd.Active() {
		a.overBusy(c, action, cid, busy)
	}
	// The command is in flight now: hold and reserve claims go pending.
	if err := a.BindClaims(ctx); err != nil {
		a.Log.Error("claim bind after power", "claim", c.ID, "error", err)
	}
	a.Rec.Trigger()
	status := http.StatusAccepted
	if !cmd.Active() {
		status = http.StatusOK
	}
	return apiCommand(cmd), status, nil
}

// overBusy records that a dedicated claim's power command went out while
// workers on the host were busy, so a killed job can be explained.
func (a *App) overBusy(c model.Claim, action string, cid int64, busy []model.Worker) {
	var wids, dids []string
	for _, w := range busy {
		wids = append(wids, w.WorkerID)
		if w.DemandID != "" {
			dids = append(dids, w.DemandID)
		}
	}
	sort.Strings(wids)
	sort.Strings(dids)
	a.Log.Warn("claim power command over busy worker", "claim", c.ID, "owner", c.Owner, "action", action,
		"host", c.Hostname, "command_id", cid, "workers", strings.Join(wids, ","), "demands", strings.Join(dids, ","))
	a.Ring.Add(decisions.Decision{Ts: a.Clock.Now(), Action: decisions.ClaimPowerOverBusyWorker, Hostname: c.Hostname, CommandID: &cid,
		Detail: fmt.Sprintf("claim %s by %s: %s with busy workers [%s], demands [%s]", c.ID, c.Owner, action,
			strings.Join(wids, ", "), strings.Join(dids, ", "))})
}

// powerFailed maps a powermaster error to the API answer and records it.
func (a *App) powerFailed(c model.Claim, action string, err error) error {
	var apiErr *pm.APIError
	out := error(nil)
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusConflict && apiErr.Code == string(api.ErrorCodeHostLocked):
			out = refuse(http.StatusConflict, api.ErrorCodeHostLocked, "%s is locked", c.Hostname)
		case apiErr.Status == http.StatusConflict && apiErr.Code == string(api.ErrorCodeCommandInFlight):
			out = refuse(http.StatusConflict, api.ErrorCodeCommandInFlight, "%s has a command in flight", c.Hostname)
		case apiErr.Status == http.StatusConflict:
			out = refuse(http.StatusConflict, api.ErrorCodeConflict, "powermaster refused %s: %s", action, apiErr.Code)
		default:
			out = refuse(http.StatusBadGateway, api.ErrorCodePowermasterUnreachable, "powermaster answered %d %s", apiErr.Status, apiErr.Code)
		}
		a.Log.Warn("claim power request refused by powermaster", "claim", c.ID, "action", action, "host", c.Hostname, "error", err)
	} else {
		a.Rec.MarkPowermasterUnreachable()
		out = refuse(http.StatusBadGateway, api.ErrorCodePowermasterUnreachable, "powermaster unreachable")
		a.Log.Error("claim power request failed", "claim", c.ID, "action", action, "host", c.Hostname, "error", err)
	}
	a.Ring.Add(decisions.Decision{Ts: a.Clock.Now(), Action: decisions.RequestFailed, Hostname: c.Hostname,
		Detail: fmt.Sprintf("claim %s by %s: %s: %s", c.ID, c.Owner, action, err)})
	return out
}

func notReadyWhy(c model.Claim) string {
	if c.Detail == store.DetailDraining {
		return "draining"
	}
	return "not bound: " + c.Detail
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func apiCommand(c pm.Command) api.Command {
	return api.Command{Id: c.ID, Hostname: c.Hostname, Action: c.Action, State: c.State, Force: c.Force,
		Escalated: c.Escalated, RequestedBy: c.RequestedBy, Reason: c.Reason, Attempt: c.Attempt,
		CreatedAt: c.CreatedAt, StartedAt: c.StartedAt, Deadline: c.Deadline, FinishedAt: c.FinishedAt, Error: c.Error}
}
