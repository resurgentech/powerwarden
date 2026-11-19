package app

import (
	"context"
	"sort"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/reconcile"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// Caller identifies who is making a claim request. Name is the bearer token
// name and becomes the claim's owner. Operator allows releasing a claim
// owned by another name (a full-role token, like the web UI).
type Caller struct {
	Name     string
	Operator bool
}

// PutClaim creates or renews a claim for caller. Binding runs in the same
// store transaction; the outcome is in the claim's state and detail.
func (a *App) PutClaim(ctx context.Context, by Caller, in store.ClaimPut) (api.Claim, error) {
	hosts, err := a.bindHosts(ctx)
	if err != nil {
		return api.Claim{}, err
	}
	in.Owner = by.Name
	c, err := a.Store.PutClaim(ctx, in, hosts, a.Clock.Now())
	if err != nil {
		return api.Claim{}, err
	}
	out := a.claimOut(ctx, c)
	a.publishClaim(api.Put, c.ID, &out)
	a.Rec.Trigger()
	return out, nil
}

// ExtendClaim moves a claim's hard cap.
func (a *App) ExtendClaim(ctx context.Context, by Caller, id string, maxSeconds int) (api.Claim, error) {
	c, err := a.Store.ExtendClaim(ctx, id, by.Name, maxSeconds, a.Clock.Now())
	if err != nil {
		return api.Claim{}, err
	}
	out := a.claimOut(ctx, c)
	a.publishClaim(api.Put, id, &out)
	return out, nil
}

// ReleaseClaim deletes a claim (absent ids are fine) and lets waiting
// claims bind to the host it frees.
func (a *App) ReleaseClaim(ctx context.Context, by Caller, id string) error {
	if err := a.Store.ReleaseClaim(ctx, id, by.Name, by.Operator); err != nil {
		return err
	}
	a.publishClaim(api.Delete, id, nil)
	if err := a.BindClaims(ctx); err != nil {
		return err
	}
	a.Rec.Trigger()
	return nil
}

// BindClaims re-evaluates claims against the current hosts and publishes the
// claims that changed. Release calls it so a waiting claim binds at once;
// each reconcile pass binds on its own.
func (a *App) BindClaims(ctx context.Context) error {
	hosts, err := a.bindHosts(ctx)
	if err != nil {
		return err
	}
	changed, err := a.Store.BindClaims(ctx, hosts, a.Clock.Now())
	if err != nil {
		return err
	}
	for _, c := range changed {
		out := a.claimOut(ctx, c)
		a.publishClaim(api.Put, c.ID, &out)
	}
	return nil
}

// Claims lists claims oldest first.
func (a *App) Claims(ctx context.Context, f store.ClaimFilter) ([]api.Claim, error) {
	cs, err := a.Store.ListClaims(ctx, a.Clock.Now(), f)
	if err != nil {
		return nil, err
	}
	out := make([]api.Claim, 0, len(cs))
	for _, c := range cs {
		out = append(out, a.claimOut(ctx, c))
	}
	return out, nil
}

// Claim returns one claim; a lapsed one only with includeExpired.
func (a *App) Claim(ctx context.Context, id string, includeExpired bool) (api.Claim, error) {
	c, err := a.Store.GetClaim(ctx, id)
	if err != nil {
		return api.Claim{}, err
	}
	if !includeExpired && !c.Live(a.Clock.Now()) {
		return api.Claim{}, store.ErrNotFound
	}
	return a.claimOut(ctx, c), nil
}

// HostsByTags returns the hosts whose explicit plus name tags cover tags: the
// same match claims use.
func (a *App) HostsByTags(ctx context.Context, tags []string) ([]api.HostView, error) {
	hs, err := a.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	idx := a.NameIndex()
	out := make([]api.HostView, 0, len(hs))
	for _, h := range hs {
		if reconcile.Covers(reconcile.MatchTags(idx, h.Hostname, h.Tags), tags) {
			out = append(out, h)
		}
	}
	return out, nil
}

// bindHosts is the host snapshot binding runs against, built as a reconcile
// pass builds it. nil means the powermaster inventory has not been read yet,
// so claims stay pending.
func (a *App) bindHosts(ctx context.Context) ([]store.BindHost, error) {
	if !a.View.Synced() {
		return nil, nil
	}
	pol, err := a.Store.HostPolicies(ctx)
	if err != nil {
		return nil, err
	}
	now := a.Clock.Now()
	ws, err := a.Store.ListWorkers(ctx, now, false)
	if err != nil {
		return nil, err
	}
	return reconcile.BindHosts(now, a.Cfg.Timings, a.View.Hosts(), pol, ws), nil
}

func (a *App) claim(c model.Claim) api.Claim {
	out := api.Claim{Id: c.ID, Owner: c.Owner, Session: c.Session, Purpose: c.Purpose, Mode: api.ClaimMode(c.Mode),
		Detached: c.Detached, Tags: c.Tags, State: api.ClaimState(c.State), Detail: c.Detail, Version: c.Version,
		ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if c.Hostname != "" {
		h := c.Hostname
		out.Hostname = &h
		// A failed claim keeps the hostname as a record; it holds nothing, so
		// it shows no host_state.
		if host, ok := a.View.Host(h); ok && c.State != model.ClaimFailed {
			st := api.HostState(host.State)
			out.HostState = &st
		}
	}
	if !c.Detached {
		n := c.LeaseSeconds
		out.LeaseSeconds = &n
		out.LeaseUntil = c.LeaseUntil
	}
	return out
}

// hostClaims groups the live bound claims (pending or ready) by host, in
// claim order, and reports which hosts have jobs blocked: a bound reserve or
// dedicated claim, draining included.
func hostClaims(live []model.Claim) (map[string][]api.HostClaim, map[string]bool) {
	by, blocked := map[string][]api.HostClaim{}, map[string]bool{}
	for _, c := range live {
		if c.Hostname == "" || c.State == model.ClaimFailed {
			continue
		}
		hc := api.HostClaim{Id: c.ID, Owner: c.Owner, Session: c.Session, Purpose: c.Purpose, Mode: api.ClaimMode(c.Mode),
			Detached: c.Detached, State: api.ClaimState(c.State), ExpiresAt: c.ExpiresAt}
		if !c.Detached {
			hc.LeaseUntil = c.LeaseUntil
		}
		by[c.Hostname] = append(by[c.Hostname], hc)
		if c.Blocks() {
			blocked[c.Hostname] = true
		}
	}
	return by, blocked
}

func (a *App) publishClaim(op api.ChangeOp, id string, c *api.Claim) {
	a.mu.Lock()
	wake := true // a delete always wakes; a put when the version moved or is new to this process
	if c == nil {
		delete(a.prevClaims, id)
		delete(a.hostSeen, id)
		delete(a.claimVer, id)
	} else {
		a.prevClaims[id] = claimKey(*c)
		a.hostSeen[id] = hostKey(*c)
		v, seen := a.claimVer[id]
		wake = !seen || v != c.Version
		a.claimVer[id] = c.Version
	}
	a.mu.Unlock()
	if wake {
		a.wakeClaim(id)
	}
	a.Hub.Publish(events.TypeClaim, func(seq int64) any {
		return api.ClaimEvent{Seq: seq, Type: api.ClaimEventTypeClaim, Op: op, Id: id, Claim: c}
	})
}

// afterPassClaims publishes claims that changed or went away (release,
// lapse, sweep) since the last look. The pass itself sweeps and binds.
func (a *App) afterPassClaims(ctx context.Context, now time.Time) {
	live, err := a.Store.ListClaims(ctx, now, store.ClaimFilter{})
	if err != nil {
		a.Log.Error("claim list", "error", err)
		return
	}
	cur := map[string]api.Claim{}
	for _, c := range live {
		cur[c.ID] = a.claimOut(ctx, c)
	}
	a.mu.Lock()
	var puts, dels []string
	for id, c := range cur {
		if a.prevClaims[id] != claimKey(c) {
			puts = append(puts, id)
		}
	}
	for id := range a.prevClaims {
		if _, ok := cur[id]; !ok {
			dels = append(dels, id)
		}
	}
	a.mu.Unlock()
	sort.Strings(puts)
	sort.Strings(dels)
	for _, id := range puts {
		c := cur[id]
		a.publishClaim(api.Put, id, &c)
	}
	for _, id := range dels {
		a.publishClaim(api.Delete, id, nil)
	}
}

// claimKey leaves out fields that move on every renewal (lease_until,
// updated_at), so a pass publishes only real changes.
func claimKey(c api.Claim) string {
	c.LeaseUntil, c.UpdatedAt = nil, time.Time{}
	return enc(c)
}
