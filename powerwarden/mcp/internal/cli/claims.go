package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
)

// WaitSeconds is the length of one blocking read.
const WaitSeconds = 60

// Pacing for WaitClaim; variables so tests can shorten them.
var (
	// RetryDelay is the pause after a transport error or 5xx.
	RetryDelay = time.Second
	// MinPoll is the pause after an unchanged answer that came back at once
	// (a server that ignored `wait`), so the loop never spins.
	MinPoll = 100 * time.Millisecond
)

// WaitClaim reads the claim, blocking on its version, until done reports true.
// A 404/410 or other refusal ends the wait with the *client.Refusal.
func WaitClaim(ctx context.Context, api client.API, id string, done func(pw.Claim) bool) (pw.Claim, error) {
	var since *int64
	for {
		if err := ctx.Err(); err != nil {
			return pw.Claim{}, err
		}
		p := pw.GetClaimParams{}
		if since != nil {
			w := WaitSeconds
			p.Wait, p.Since = &w, since
		}
		start := time.Now()
		resp, err := api.GetClaimWithResponse(ctx, id, &p)
		if err == nil {
			err = client.Check(resp.StatusCode(), resp.Body, 200)
		}
		if err != nil {
			var ref *client.Refusal
			if errors.As(err, &ref) && !ref.Transient() {
				return pw.Claim{}, err
			}
			if cerr := ctx.Err(); cerr != nil {
				return pw.Claim{}, cerr
			}
			if serr := sleep(ctx, RetryDelay); serr != nil {
				return pw.Claim{}, serr
			}
			continue
		}
		c := resp.JSON200
		if c == nil {
			return pw.Claim{}, fmt.Errorf("powerwarden answered 200 without a claim")
		}
		if done(*c) {
			return *c, nil
		}
		unchanged := since != nil && *since == c.Version
		v := c.Version
		since = &v
		if unchanged && time.Since(start) < MinPoll {
			if serr := sleep(ctx, MinPoll); serr != nil {
				return pw.Claim{}, serr
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ReadyOrFailed is the done func for "wait until the claim is bound".
func ReadyOrFailed(c pw.Claim) bool { return c.State == pw.Ready || c.State == pw.Failed }

// HostOn is the done func for "wait until the host is on" (a failed claim also
// ends the wait).
func HostOn(c pw.Claim) bool {
	return c.State == pw.Failed || (c.HostState != nil && *c.HostState == pw.On)
}

// CreateDetached makes a detached claim: no liveness lease, so it lives until
// its hard cap or a release. It is a one-shot PUT that never registers with a
// keeper (nothing renews or releases it); keeper.Spec has no way to ask for
// detached, which keeps `pwrwrdn run` unable to send it.
// Only spec.Tags, Purpose, Mode and MaxSeconds are used.
func CreateDetached(ctx context.Context, api client.API, prefix, session string, s keeper.Spec) (*pw.Claim, error) {
	mode := s.Mode
	if mode == "" {
		mode = pw.Hold
	}
	detached := true
	body := pw.ClaimPut{Tags: s.Tags, Purpose: s.Purpose, Mode: &mode, Detached: &detached}
	if session != "" {
		body.Session = &session
	}
	if s.MaxSeconds > 0 {
		body.MaxSeconds = &s.MaxSeconds
	}
	resp, err := api.PutClaimWithResponse(ctx, client.NewClaimID(prefix), body)
	if err != nil {
		return nil, err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("powerwarden answered 200 without a claim")
	}
	return resp.JSON200, nil
}

// Power sends a power command for the claim's host. already reports a 200
// (the host was already there) as opposed to a 202 (command queued).
func Power(ctx context.Context, api client.API, id, action string) (cmd *pw.Command, already bool, err error) {
	resp, err := api.ClaimPowerWithResponse(ctx, id, pw.ClaimPower{Action: pw.ClaimPowerAction(action)})
	if err != nil {
		return nil, false, err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200, 202); err != nil {
		return nil, false, err
	}
	if resp.StatusCode() == 200 {
		return resp.JSON200, true, nil
	}
	return resp.JSON202, false, nil
}

// Release deletes a claim.
func Release(ctx context.Context, api client.API, id string) error {
	resp, err := api.DeleteClaimWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return client.Check(resp.StatusCode(), resp.Body, 204, 200)
}
