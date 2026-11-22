// Package keeper owns the claims one pwrwrdn process created: it renews each
// leased claim at lease/3, notices when one is lost, and releases them all on
// the way out. `pwrwrdn run` uses it.
package keeper

import (
	"context"
	"fmt"
	"sync"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
)

// defaultLease is used for cadence when the server's reply carries none.
const defaultLease = 15 * time.Minute

// Options configures a Keeper.
type Options struct {
	Prefix  string // claim id prefix: "pwrwrdn"
	Session string // sent as `session` on every PUT
	Clock   Clock  // nil means the wall clock
}

// Spec describes a leased claim to create. It never carries `detached`.
type Spec struct {
	Tags         []string
	Purpose      string
	Mode         pw.ClaimMode // empty means hold
	MaxSeconds   int          // 0 lets the server default apply
	LeaseSeconds int          // 0 lets the server default apply
}

// Lost records a claim the keeper could no longer renew.
type Lost struct {
	ID string
	// Reason is why the claim was lost; for a Failed claim it is the server's
	// detail (for example "host node01 locked").
	Reason string
	// Failed: powerwarden failed the claim (410 claim_failed, or a renew
	// answered with state failed) rather than the claim lapsing or vanishing.
	Failed bool
	At     time.Time
}

type entry struct {
	body   pw.ClaimPut // the renew body: no max_seconds
	cancel context.CancelFunc
	lost   *Lost
	seen   *int64
}

// Keeper renews and releases claims.
type Keeper struct {
	api     client.API
	prefix  string
	session string
	clk     Clock

	base   context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	entries map[string]*entry
	wg      sync.WaitGroup
}

// New builds a Keeper.
func New(api client.API, o Options) *Keeper {
	if o.Clock == nil {
		o.Clock = RealClock{}
	}
	if o.Prefix == "" {
		o.Prefix = "pwrwrdn"
	}
	base, cancel := context.WithCancel(context.Background())
	return &Keeper{api: api, prefix: o.Prefix, session: o.Session, clk: o.Clock,
		base: base, cancel: cancel, entries: map[string]*entry{}}
}

// Session is the session id sent on every PUT.
func (k *Keeper) Session() string { return k.session }

// Create makes a leased claim with a fresh id and starts renewing it. A
// refusal comes back as *client.Refusal and nothing is kept.
func (k *Keeper) Create(ctx context.Context, s Spec) (*pw.Claim, error) {
	id := client.NewClaimID(k.prefix)
	mode := s.Mode
	if mode == "" {
		mode = pw.Hold
	}
	sess := k.session
	body := pw.ClaimPut{Tags: s.Tags, Purpose: s.Purpose, Mode: &mode}
	if sess != "" {
		body.Session = &sess
	}
	create := body
	if s.MaxSeconds > 0 {
		create.MaxSeconds = &s.MaxSeconds
	}
	if s.LeaseSeconds > 0 {
		create.LeaseSeconds = &s.LeaseSeconds
	}
	resp, err := k.api.PutClaimWithResponse(ctx, id, create)
	if err != nil {
		return nil, err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return nil, err
	}
	c := resp.JSON200
	if c == nil {
		return nil, fmt.Errorf("powerwarden answered 200 without a claim")
	}
	lease := defaultLease
	renew := body
	switch {
	case c.LeaseSeconds != nil:
		lease = time.Duration(*c.LeaseSeconds) * time.Second
		renew.LeaseSeconds = c.LeaseSeconds
	case s.LeaseSeconds > 0:
		lease = time.Duration(s.LeaseSeconds) * time.Second
	}
	ectx, cancel := context.WithCancel(k.base)
	v := c.Version
	e := &entry{body: renew, cancel: cancel, seen: &v}
	k.mu.Lock()
	k.entries[id] = e
	k.mu.Unlock()
	k.wg.Add(1)
	go k.renewLoop(ectx, id, e, lease/3)
	return c, nil
}

func (k *Keeper) renewLoop(ctx context.Context, id string, e *entry, every time.Duration) {
	defer k.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.clk.After(every):
		}
		resp, err := k.api.PutClaimWithResponse(ctx, id, e.body)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			continue // transport trouble: try again next tick
		}
		if cerr := client.Check(resp.StatusCode(), resp.Body, 200); cerr != nil {
			ref, isRef := cerr.(*client.Refusal)
			switch {
			case isRef && ref.Transient():
				continue
			case isRef && ref.Code == string(pw.ErrorCodeClaimFailed):
				k.markLostAs(id, ref.Message, true) // the message is the server's detail
			default:
				k.markLostAs(id, cerr.Error(), false)
			}
			return
		}
		// An older server answers a renew of a failed claim with 200.
		if c := resp.JSON200; c != nil && c.State == pw.Failed {
			k.markLostAs(id, c.Detail, true)
			return
		}
	}
}

func (k *Keeper) markLostAs(id, reason string, failed bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.entries[id]; e != nil && e.lost == nil {
		e.lost = &Lost{ID: id, Reason: reason, Failed: failed, At: k.clk.Now()}
		e.cancel()
	}
}

// MarkLost records a claim as lost (for example after a 404/410 on a read).
// It does nothing for ids this process did not create.
func (k *Keeper) MarkLost(id, reason string) { k.markLostAs(id, reason, false) }

// MarkFailed records a claim powerwarden failed (state failed, final) as
// lost, with the server's detail as the reason. It does nothing for ids this
// process did not create.
func (k *Keeper) MarkFailed(id, detail string) { k.markLostAs(id, detail, true) }

// Lost lists the lost claims not yet acknowledged, oldest first.
func (k *Keeper) Lost() []Lost {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []Lost
	for _, e := range k.entries {
		if e.lost != nil {
			out = append(out, *e.lost)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].At.Before(out[j-1].At) || (out[j].At.Equal(out[j-1].At) && out[j].ID < out[j-1].ID)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Ack forgets a claim: stops renewing it and clears any loss warning.
func (k *Keeper) Ack(id string) {
	k.mu.Lock()
	e := k.entries[id]
	delete(k.entries, id)
	k.mu.Unlock()
	if e != nil {
		e.cancel()
	}
}

// Owns reports whether this process created id and still tracks it.
func (k *Keeper) Owns(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.entries[id] != nil
}

// Seen is the last claim version this process saw for id, or nil.
func (k *Keeper) Seen(id string) *int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.entries[id]; e != nil && e.seen != nil {
		v := *e.seen
		return &v
	}
	return nil
}

// SetSeen records the version the model has seen for a tracked claim.
func (k *Keeper) SetSeen(id string, v int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.entries[id]; e != nil {
		e.seen = &v
	}
}

// Release deletes the claim on the server and forgets it, acknowledging any
// loss. A refusal other than success leaves the claim tracked.
func (k *Keeper) Release(ctx context.Context, id string) error {
	resp, err := k.api.DeleteClaimWithResponse(ctx, id)
	if err != nil {
		return err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 204, 200); err != nil {
		return err
	}
	k.Ack(id)
	return nil
}

// ReleaseAll makes a best-effort DELETE of every tracked claim, including
// lost ones, bounded by ctx.
func (k *Keeper) ReleaseAll(ctx context.Context) {
	k.mu.Lock()
	ids := make([]string, 0, len(k.entries))
	for id := range k.entries {
		ids = append(ids, id)
	}
	k.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = k.Release(ctx, id)
		}()
	}
	wg.Wait()
}

// Close stops all renewal goroutines without touching the server.
func (k *Keeper) Close() {
	k.cancel()
	k.wg.Wait()
}

// ReleaseTimeout is the total budget for ReleaseAll on exit.
const ReleaseTimeout = 5 * time.Second
