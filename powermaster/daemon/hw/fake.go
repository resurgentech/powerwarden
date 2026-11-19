package hw

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
)

// FakeLab simulates hosts, BMCs and PDUs for tests. Physical rules: a host
// whose PDU outlets are all off has no chassis power and an unreachable BMC;
// SSH and ping answer only while the chassis is on (unless scripted down).
// Every hardware call is appended to Calls.
type FakeLab struct {
	mu    sync.Mutex
	hosts map[string]*FakeHost
	pdus  map[string]*FakePDU
	calls []string
}

// FakeHost is one simulated host. Exported fields are its script; set them
// through FakeLab.Set to stay race-free.
type FakeHost struct {
	Name    string
	Chassis bool // chassis power

	// Scripts.
	PowerOnFailures  int  // SetChassisPower(true) errors this many times first
	PowerOffFailures int  // SetChassisPower(false) errors this many times first
	PowerOnIgnored   bool // power-on accepted, chassis stays off
	PowerOffIgnored  bool // power-off accepted, chassis stays on (hung BMC/PSU)
	ShutdownWorks    bool // SSH shutdown turns the chassis off
	ShutdownFails    bool // SSH shutdown returns an error
	SSHDown          bool // SSH never answers
	PingDown         bool // ping never answers
	ReadErrors       int  // ChassisPower errors this many times first

	outlets []outletKey
}

type outletKey struct {
	pdu    string
	outlet int
}

// FakePDU is one simulated PDU.
type FakePDU struct {
	Name     string
	Outlets  map[int]bool
	SetFails int // SetOutlet errors this many times first
}

// NewFakeLab builds a lab from inventory: chassis off, every outlet on.
func NewFakeLab(inv *inventory.Inventory) *FakeLab {
	l := &FakeLab{hosts: map[string]*FakeHost{}, pdus: map[string]*FakePDU{}}
	for name := range inv.Pdus {
		l.pdus[name] = &FakePDU{Name: name, Outlets: map[int]bool{}}
	}
	for _, h := range inv.Hosts {
		fh := &FakeHost{Name: h.Hostname}
		for _, r := range h.Outlets() {
			fh.outlets = append(fh.outlets, outletKey{r.Name, r.Outlet})
			l.pdus[r.Name].Outlets[r.Outlet] = true
		}
		l.hosts[h.Hostname] = fh
	}
	return l
}

// Set runs fn on a host under the lab lock.
func (l *FakeLab) Set(host string, fn func(h *FakeHost)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(l.hosts[host])
}

// SetPDU runs fn on a PDU under the lab lock.
func (l *FakeLab) SetPDU(name string, fn func(p *FakePDU)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(l.pdus[name])
}

// Chassis reports a host's chassis power.
func (l *FakeLab) Chassis(host string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hosts[host].Chassis
}

// Outlet reports a PDU outlet state.
func (l *FakeLab) Outlet(pdu string, outlet int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pdus[pdu].Outlets[outlet]
}

// Calls returns a copy of the call log.
func (l *FakeLab) Calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// ResetCalls clears the call log.
func (l *FakeLab) ResetCalls() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

func (l *FakeLab) record(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

// powered: a host with outlets has power only if all of them are on.
func (l *FakeLab) poweredLocked(h *FakeHost) bool {
	for _, o := range h.outlets {
		if !l.pdus[o.pdu].Outlets[o.outlet] {
			return false
		}
	}
	return true
}

var errFake = errors.New("fake hardware error")

// IPMI implements Provider.
func (l *FakeLab) IPMI(h inventory.Host) IPMI { return &fakeIPMI{l: l, name: h.Hostname} }

// Net implements Provider.
func (l *FakeLab) Net(h inventory.Host) Net { return &fakeNet{l: l, name: h.Hostname} }

// PDU implements Provider.
func (l *FakeLab) PDU(p inventory.Pdu) (PDU, error) {
	if _, ok := l.pdus[p.Name]; !ok {
		return nil, fmt.Errorf("no fake pdu %s", p.Name)
	}
	return &fakePDU{l: l, name: p.Name}, nil
}

type fakeIPMI struct {
	l    *FakeLab
	name string
}

func (f *fakeIPMI) ChassisPower(ctx context.Context) (bool, error) {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[f.name]
	l.record("%s ipmi.status", f.name)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !l.poweredLocked(h) {
		return false, fmt.Errorf("bmc unreachable: %w", errFake)
	}
	if h.ReadErrors > 0 {
		h.ReadErrors--
		return false, errFake
	}
	return h.Chassis, nil
}

func (f *fakeIPMI) SetChassisPower(ctx context.Context, on bool) error {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[f.name]
	l.record("%s ipmi.%s", f.name, onOff(on))
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.poweredLocked(h) {
		return fmt.Errorf("bmc unreachable: %w", errFake)
	}
	if on {
		if h.PowerOnFailures > 0 {
			h.PowerOnFailures--
			return errFake
		}
		if !h.PowerOnIgnored {
			h.Chassis = true
		}
		return nil
	}
	if h.PowerOffFailures > 0 {
		h.PowerOffFailures--
		return errFake
	}
	if !h.PowerOffIgnored {
		h.Chassis = false
	}
	return nil
}

type fakeNet struct {
	l    *FakeLab
	name string
}

func (f *fakeNet) PingHost(ctx context.Context) bool {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[f.name]
	l.record("%s ping", f.name)
	return ctx.Err() == nil && h.Chassis && !h.PingDown
}

func (f *fakeNet) PingBMC(ctx context.Context) bool {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record("%s ping.bmc", f.name)
	return ctx.Err() == nil && l.poweredLocked(l.hosts[f.name])
}

func (f *fakeNet) SSH(ctx context.Context) bool {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[f.name]
	l.record("%s ssh", f.name)
	return ctx.Err() == nil && h.Chassis && !h.SSHDown
}

func (f *fakeNet) Shutdown(ctx context.Context) error {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[f.name]
	l.record("%s ssh.shutdown", f.name)
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.ShutdownFails || h.SSHDown || !h.Chassis {
		return errFake
	}
	if h.ShutdownWorks {
		h.Chassis = false
	}
	return nil
}

type fakePDU struct {
	l    *FakeLab
	name string
}

func (f *fakePDU) OutletStates(ctx context.Context) (map[int]bool, error) {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record("%s pdu.status", f.name)
	out := map[int]bool{}
	for k, v := range l.pdus[f.name].Outlets {
		out[k] = v
	}
	return out, nil
}

func (f *fakePDU) SetOutlet(ctx context.Context, outlet int, on bool) error {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.pdus[f.name]
	l.record("%s pdu.%d.%s", f.name, outlet, onOff(on))
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.SetFails > 0 {
		p.SetFails--
		return errFake
	}
	p.Outlets[outlet] = on
	if !on {
		// Cutting power drops the chassis of every host fed by this outlet.
		for _, h := range l.hosts {
			for _, o := range h.outlets {
				if o.pdu == f.name && o.outlet == outlet {
					h.Chassis = false
				}
			}
		}
	}
	return nil
}
