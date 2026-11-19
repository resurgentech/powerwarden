// Package hw holds the hardware interfaces powermaster drives (IPMI, PDU,
// network), their real implementations adapted from an earlier powerwarden, and
// scriptable fakes for tests.
package hw

import (
	"context"

	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
)

// IPMI controls one host's BMC.
type IPMI interface {
	// ChassisPower reports whether the chassis is powered on.
	ChassisPower(ctx context.Context) (bool, error)
	// SetChassisPower sends a chassis power on/off command. It does not wait
	// for the chassis to change; callers poll ChassisPower.
	SetChassisPower(ctx context.Context, on bool) error
}

// PDU controls one PDU. Outlet numbers are as written in inventory.
type PDU interface {
	OutletStates(ctx context.Context) (map[int]bool, error)
	SetOutlet(ctx context.Context, outlet int, on bool) error
}

// Net checks and controls one host over the network.
type Net interface {
	// PingHost pings the host's own address.
	PingHost(ctx context.Context) bool
	// PingBMC pings the host's BMC address.
	PingBMC(ctx context.Context) bool
	// SSH reports whether `ssh host exit 0` succeeds.
	SSH(ctx context.Context) bool
	// Shutdown runs the OS shutdown over SSH.
	Shutdown(ctx context.Context) error
}

// Provider builds per-host and per-PDU clients. Each host owns its clients;
// nothing is shared between hosts.
type Provider interface {
	IPMI(h inventory.Host) IPMI
	Net(h inventory.Host) Net
	PDU(p inventory.Pdu) (PDU, error)
}
