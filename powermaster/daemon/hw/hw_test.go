package hw

import (
	"context"
	"testing"

	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
)

var (
	_ Provider = Real{}
	_ Provider = (*FakeLab)(nil)
)

func labInventory() *inventory.Inventory {
	inv := &inventory.Inventory{
		Hosts: []inventory.Host{{Hostname: "h1", Ipmi: inventory.Ipmi{Username: "u", Password: "p"},
			Pdu: []inventory.PduRef{{Name: "p1", Outlet: 2}}}},
		Pdus: map[string]inventory.Pdu{"p1": {IP: "x", Username: "u", Password: "p", PduStyle: "kasa_hs300"}},
	}
	if err := inv.Validate(); err != nil {
		panic(err)
	}
	return inv
}

func TestRealPDUStyles(t *testing.T) {
	r := Real{}
	for _, style := range []string{"eaton_ePDU_G3_Managed", "eaton_ePDU_G3_Managed_proxied", "kasa_hs300"} {
		if _, err := r.PDU(inventory.Pdu{Name: "p", PduStyle: style}); err != nil {
			t.Fatalf("%s: %v", style, err)
		}
	}
	if _, err := r.PDU(inventory.Pdu{Name: "p", PduStyle: "nope"}); err == nil {
		t.Fatal("want unsupported pdustyle error")
	}
}

func TestFakeLabPhysics(t *testing.T) {
	ctx := context.Background()
	inv := labInventory()
	lab := NewFakeLab(inv)
	h := inv.Hosts[0]
	ipmi, net := lab.IPMI(h), lab.Net(h)
	pdu, _ := lab.PDU(inv.Pdus["p1"])

	if on, err := ipmi.ChassisPower(ctx); err != nil || on {
		t.Fatalf("initial chassis = %v %v", on, err)
	}
	lab.Set("h1", func(h *FakeHost) { h.PowerOnFailures = 1 })
	if err := ipmi.SetChassisPower(ctx, true); err == nil {
		t.Fatal("want scripted failure")
	}
	if err := ipmi.SetChassisPower(ctx, true); err != nil || !lab.Chassis("h1") {
		t.Fatalf("power on: %v chassis=%v", err, lab.Chassis("h1"))
	}
	if !net.PingHost(ctx) || !net.SSH(ctx) {
		t.Fatal("on host should answer ping and ssh")
	}
	if err := pdu.SetOutlet(ctx, 2, false); err != nil {
		t.Fatal(err)
	}
	if lab.Chassis("h1") || net.PingBMC(ctx) {
		t.Fatal("cutting the outlet must drop chassis and BMC")
	}
	if _, err := ipmi.ChassisPower(ctx); err == nil {
		t.Fatal("BMC without power must not answer")
	}
	states, _ := pdu.OutletStates(ctx)
	if states[2] {
		t.Fatal("outlet should read off")
	}
	if len(lab.Calls()) == 0 {
		t.Fatal("calls not recorded")
	}
}
