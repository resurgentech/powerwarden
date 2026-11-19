// Package decisions keeps the in-memory ring of reconcile actions and skips
// served by GET /v1/decisions.
package decisions

import (
	"slices"
	"sync"
	"time"
)

const (
	PowerupRequested   = "powerup_requested"
	PowerdownRequested = "powerdown_requested"
	PowercutRequested  = "powercut_requested"
	RequestFailed      = "request_failed"
	NoCandidate        = "no_candidate"
	BootGraceExpired   = "boot_grace_expired"
	CooldownStarted    = "cooldown_started"
	// ClaimPowerRequested: a claim holder asked for a power command (PWC8a).
	ClaimPowerRequested = "claim_power_requested"
	// ClaimPowerOverBusyWorker: that command went out while a worker on the
	// host was busy (dedicated claims only).
	ClaimPowerOverBusyWorker = "claim_power_over_busy_worker"
)

type Decision struct {
	Ts        time.Time `json:"ts"`
	Action    string    `json:"action"`
	Hostname  string    `json:"hostname,omitempty"`
	DemandID  string    `json:"demand_id,omitempty"`
	CommandID *int64    `json:"command_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// Ring holds the last N decisions.
type Ring struct {
	mu    sync.Mutex
	buf   []Decision
	next  int
	full  bool
	onAdd []func(Decision)
}

func NewRing(size int) *Ring { return &Ring{buf: make([]Decision, size)} }

// OnAdd registers a callback run after each Add (used by the event hub).
func (r *Ring) OnAdd(fn func(Decision)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onAdd = append(r.onAdd, fn)
}

func (r *Ring) Add(d Decision) {
	r.mu.Lock()
	r.buf[r.next] = d
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	fns := slices.Clone(r.onAdd)
	r.mu.Unlock()
	for _, fn := range fns {
		fn(d)
	}
}

// List returns decisions newest first.
func (r *Ring) List() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]Decision, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, r.buf[(r.next-i+len(r.buf))%len(r.buf)])
	}
	return out
}
