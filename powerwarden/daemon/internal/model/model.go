// Package model holds powerwarden's demand, worker, and host-policy records.
package model

import "time"

const (
	SourceHourchd = "hourchd"
	SourceManual  = "manual"

	JobIdle        = "idle"
	JobBusy        = "busy"
	JobError       = "error"
	JobMaintenance = "maintenance"
)

// Demand is work waiting for a machine with certain tags.
type Demand struct {
	ID           string    `json:"id"`
	Tags         []string  `json:"tags"`
	LeaseSeconds int       `json:"lease_seconds"`
	LeaseUntil   time.Time `json:"lease_until"`
	Source       string    `json:"source"`
	ExternalRef  string    `json:"external_ref"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Open reports whether the demand's lease is still running.
func (d Demand) Open(now time.Time) bool { return d.LeaseUntil.After(now) }

// Worker is a liveness record.
type Worker struct {
	WorkerID     string     `json:"worker_id"`
	Hostname     string     `json:"hostname"`
	Tags         []string   `json:"tags"`
	JobStatus    string     `json:"job_status"`
	DemandID     string     `json:"demand_id"`
	LeaseSeconds int        `json:"lease_seconds"`
	LeaseUntil   time.Time  `json:"lease_until"`
	LastBusyAt   *time.Time `json:"last_busy_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Fresh reports whether the worker's lease is still running.
func (w Worker) Fresh(now time.Time) bool { return w.LeaseUntil.After(now) }

// HostPolicy is powerwarden's own per-host state.
type HostPolicy struct {
	Hostname string `json:"hostname"`
	Exclude  bool   `json:"exclude"`
	// Tags is the host's capability. nil means never set (no stored row value);
	// empty means set to nothing.
	Tags            []string   `json:"tags"`
	IdleSince       *time.Time `json:"idle_since"`
	LastPowerupOKAt *time.Time `json:"last_powerup_ok_at"`
	LastRequestID   *int64     `json:"last_request_id"`
	LastRequestAt   *time.Time `json:"last_request_at"`
}

const (
	ClaimHold      = "hold"
	ClaimReserve   = "reserve"
	ClaimDedicated = "dedicated"

	ClaimPending = "pending"
	ClaimReady   = "ready"
	ClaimFailed  = "failed"
)

// Claim is a lease an agent holds on one host, chosen by tags.
type Claim struct {
	ID       string
	Owner    string // bearer token name at creation; never from a body
	Session  string
	Purpose  string
	Mode     string
	Detached bool
	Tags     []string
	// Hostname is the bound host; "" until bound (state ready).
	Hostname string
	State    string
	Detail   string
	// Version starts at 1 and increases when State, Detail or Hostname change.
	Version int64
	// LeaseSeconds and LeaseUntil are zero/nil when Detached.
	LeaseSeconds int
	LeaseUntil   *time.Time
	ExpiresAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Live reports whether the claim has not lapsed: expires_at is in the future
// and, for a non-detached claim, so is lease_until.
func (c Claim) Live(now time.Time) bool {
	if !c.ExpiresAt.After(now) {
		return false
	}
	return c.Detached || (c.LeaseUntil != nil && c.LeaseUntil.After(now))
}

// Blocks reports whether the claim blocks hourchd jobs while bound and live.
func (c Claim) Blocks() bool { return c.Mode == ClaimReserve || c.Mode == ClaimDedicated }
