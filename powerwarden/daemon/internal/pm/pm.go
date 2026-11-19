// Package pm is powerwarden's view of powermaster: the host list, power
// requests, and the event stream. The reconciler only sees the Client
// interface; tests use Fake, production uses HTTP (over powermasterapi).
package pm

import (
	"context"
	"fmt"
	"time"
)

// Host states and command values as defined by powermaster.
const (
	StateSoftOff     = "soft_off"
	StateColdOff     = "cold_off"
	StatePoweringOn  = "powering_on"
	StateOn          = "on"
	StatePoweringOff = "powering_off"
	StateRebooting   = "rebooting"
	StateUnknown     = "unknown"

	ActionPowerup   = "powerup"
	ActionPowerdown = "powerdown"
	ActionPowercut  = "powercut"
	ActionReboot    = "reboot"

	CmdQueued    = "queued"
	CmdRunning   = "running"
	CmdSucceeded = "succeeded"
	CmdFailed    = "failed"

	EventHost    = "host"
	EventCommand = "command"
	EventResync  = "resync"
)

// IsOff reports whether state is any off state: soft_off or cold_off.
func IsOff(state string) bool {
	return state == StateSoftOff || state == StateColdOff
}

// Host is the subset of powermaster's host object powerwarden uses.
type Host struct {
	Hostname string
	Locked   bool
	State    string
	// StateSince is when State last changed. Zero when powermaster does not
	// report it (old powermaster): such a host is never cold-off eligible.
	StateSince    time.Time
	PowerFailedAt *time.Time
	ActiveCommand *ActiveCommand
}

// ActiveCommand is the in-flight command summary on a host.
type ActiveCommand struct {
	ID       int64
	Action   string // may be empty if only the id is known
	State    string
	Deadline time.Time
}

// InFlightBoot reports a queued or running powerup or reboot.
func (a *ActiveCommand) InFlightBoot() bool {
	return a != nil && (a.Action == ActionPowerup || a.Action == ActionReboot) &&
		(a.State == CmdQueued || a.State == CmdRunning)
}

// Command is powermaster's command object.
type Command struct {
	ID               int64
	Hostname         string
	Action           string
	State            string
	Force, Escalated bool
	Attempt          string
	StartedAt        *time.Time
	RequestedBy      string
	Reason           string
	Error            string
	CreatedAt        time.Time
	FinishedAt       *time.Time
	Deadline         time.Time
}

// Active reports queued or running.
func (c Command) Active() bool { return c.State == CmdQueued || c.State == CmdRunning }

// Event is one powermaster SSE event.
type Event struct {
	Seq     int64
	Type    string // host, command, resync
	Host    *HostEvent
	Command *Command
}

// HostEvent is the payload of a "host" event.
type HostEvent struct {
	Hostname        string
	State           string
	StateSince      time.Time // zero when absent
	ActiveCommandID *int64
	// ActiveCommand is set when the event carries the full summary
	// (powermaster does); ActiveCommandID is then its id.
	ActiveCommand *ActiveCommand
}

// Client is everything powerwarden asks of powermaster.
type Client interface {
	// ListHosts is GET /v1/hosts, in inventory order.
	ListHosts(ctx context.Context) ([]Host, error)
	// Power is POST /v1/hosts/{hostname}/power. Errors from the server are *APIError.
	Power(ctx context.Context, hostname, action, reason string) (Command, error)
	// Events is GET /v1/events. The channel closes when the stream ends.
	Events(ctx context.Context, lastEventID int64) (<-chan Event, error)
}

// APIError is a non-2xx response from powermaster.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("powermaster %d %s: %s", e.Status, e.Code, e.Message)
}
