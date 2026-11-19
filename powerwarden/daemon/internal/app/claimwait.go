package app

import (
	"context"
	"errors"
	"strings"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// notifier wakes the blocking reads of claims. A channel per claim id is
// closed (and dropped) when that claim's version changes or it is deleted;
// a waiter takes the channel before it reads the row, so a change between
// the read and the wait is never missed.

// claimChan returns the channel the next change of claim id closes.
func (a *App) claimChan(id string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.claimWake[id]
	if !ok {
		ch = make(chan struct{})
		a.claimWake[id] = ch
	}
	return ch
}

// wakeClaim wakes every blocking read of claim id.
func (a *App) wakeClaim(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ch, ok := a.claimWake[id]; ok {
		close(ch)
		delete(a.claimWake, id)
	}
}

// ClampWait is the wait a request gets: min(want, claim_wait_max).
func (a *App) ClampWait(want time.Duration) time.Duration {
	if max := a.Cfg.Timings.ClaimWaitMax; want > max {
		return max
	}
	return want
}

// ClaimWaiters reports how many blocking reads wait on claim id right now.
func (a *App) ClaimWaiters(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claimWaiters[id]
}

// WaitWakes counts notifier wake-ups of blocking reads since start (a
// diagnostic: renewals and extends must not add to it).
func (a *App) WaitWakes() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waitWakes
}

func (a *App) addWaiter(id string, d int) {
	a.mu.Lock()
	a.claimWaiters[id] += d
	if a.claimWaiters[id] == 0 {
		delete(a.claimWaiters, id)
	}
	a.mu.Unlock()
}

// WaitClaim is the blocking claim read. It returns at once when since is nil,
// differs from the claim's version, or wait is 0. Otherwise it blocks until
// the version changes, the claim is deleted (ErrNotFound), the claim lapses
// (store.ErrExpired), ctx ends (ctx's error), or the clamped wait ends (the
// unchanged claim). include_expired reads a lapsed claim at once.
func (a *App) WaitClaim(ctx context.Context, id string, includeExpired bool, since *int64, wait time.Duration) (api.Claim, error) {
	wait = a.ClampWait(wait)
	c, err := a.Claim(ctx, id, includeExpired)
	if err != nil || since == nil || *since != c.Version || wait <= 0 {
		return c, err
	}
	timeout := a.Clock.After(wait)
	last := false // the wait ended: one more read, then return
	for {
		ch := a.claimChan(id) // before the read
		raw, err := a.Store.GetClaim(ctx, id)
		if err != nil {
			return api.Claim{}, err
		}
		raw = a.settle(ctx, raw)
		now := a.Clock.Now()
		live := raw.Live(now)
		switch {
		case !live && !includeExpired:
			return api.Claim{}, store.ErrExpired
		case raw.Version != *since || last || !live:
			return a.claim(raw), nil
		}
		lapse := a.Clock.After(lapseIn(raw, now))
		a.addWaiter(id, 1) // timers exist now: a test that sees the waiter may advance the clock
		select {
		case <-ch:
			a.mu.Lock()
			a.waitWakes++
			a.mu.Unlock()
		case <-timeout:
			last = true
		case <-lapse:
		case <-ctx.Done():
			a.addWaiter(id, -1)
			return api.Claim{}, ctx.Err()
		}
		a.addWaiter(id, -1)
	}
}

// lapseIn is how long until the claim lapses if nothing renews it.
func lapseIn(c model.Claim, now time.Time) time.Duration {
	at := c.ExpiresAt
	if !c.Detached && c.LeaseUntil != nil && c.LeaseUntil.Before(at) {
		at = *c.LeaseUntil
	}
	return at.Sub(now)
}

// hostKey is what a claim shows of its host: the bound hostname and state.
func hostKey(c api.Claim) string {
	if c.Hostname == nil {
		return ""
	}
	st := ""
	if c.HostState != nil {
		st = string(*c.HostState)
	}
	return *c.Hostname + "\x00" + st
}

// claimOut renders a claim after settling its host_state against the row.
func (a *App) claimOut(ctx context.Context, c model.Claim) api.Claim {
	return a.claim(a.settle(ctx, c))
}

// settle bumps the stored version of c when its bound host changed state
// since the app last saw the claim, and returns the row as it now is.
// host_state is read from the host view and is not a column, so this bump is
// the only way it reaches `version`. It runs on every read and publish path,
// so a state the view shows is never paired with an older version. The
// compare-and-set of hostSeen is under the lock, so concurrent callers bump
// once. A claim with no baseline (first sighting since start) is not
// bumped, and a claim whose hostname changed was already bumped by binding.
func (a *App) settle(ctx context.Context, c model.Claim) model.Claim {
	if c.Hostname == "" || c.State == model.ClaimFailed { // a failed claim shows no host_state
		return c
	}
	cur := hostKey(a.claim(c))
	a.mu.Lock()
	prev, seen := a.hostSeen[c.ID]
	bump := seen && prev != cur && strings.HasPrefix(prev, c.Hostname+"\x00")
	if bump {
		a.hostSeen[c.ID] = cur
	}
	a.mu.Unlock()
	if !bump {
		return c
	}
	b, err := a.Store.BumpClaimVersion(ctx, c.ID, a.Clock.Now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.Log.Error("claim version bump", "id", c.ID, "error", err)
		}
		return c
	}
	a.wakeClaim(c.ID)
	return b
}
