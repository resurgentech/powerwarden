package inventory

import (
	"context"
	"strings"
	"testing"
)

func secretData() map[string]any {
	return map[string]any{
		"hosts": []any{
			map[string]any{
				"hostname": "veritech01.labtwo",
				"ip":       "10.0.0.11",
				"ipmi":     map[string]any{"ip": "10.0.1.11", "username": "admin", "password": "pw"},
				"pdu":      []any{map[string]any{"name": "eatonpdu01.labtwo", "outlet": 3}},
				"user":     "root",
				"sshkey":   map[string]any{"private": "a2V5", "public": "pub"},
				"locked":   true,
			},
			map[string]any{
				"hostname": "spare01.labtwo",
				"ip":       "10.0.0.12",
				"ipmi":     map[string]any{"ip": "10.0.1.12", "username": "admin", "password": "pw"},
			},
		},
		"pdus": map[string]any{
			"eatonpdu01.labtwo": map[string]any{"ip": "10.0.2.1", "username": "u", "password": "p", "pdustyle": "eaton_ePDU_G3_Managed"},
		},
	}
}

func TestParseSecret(t *testing.T) {
	inv, err := ParseSecret(secretData())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 2 {
		t.Fatalf("hosts = %d", len(inv.Hosts))
	}
	h, ok := inv.Host("veritech01.labtwo")
	if !ok {
		t.Fatal("veritech01 missing")
	}
	if !h.Locked {
		t.Fatalf("locked=%v", h.Locked)
	}
	if h.Ipmi.IP != "10.0.1.11" || h.Pdu[0].Outlet != 3 || h.SSHKey.PrivateKey != "a2V5" {
		t.Fatalf("host = %+v", h)
	}
	p, ok := inv.Pdus["eatonpdu01.labtwo"]
	if !ok || p.Name != "eatonpdu01.labtwo" || p.PduStyle != "eaton_ePDU_G3_Managed" {
		t.Fatalf("pdu = %+v", p)
	}
}

func TestParseSecretErrors(t *testing.T) {
	cases := map[string]func(map[string]any){
		"no hosts key": func(d map[string]any) { delete(d, "hosts") },
		"missing hostname": func(d map[string]any) {
			d["hosts"].([]any)[1].(map[string]any)["hostname"] = ""
		},
		"missing ipmi password": func(d map[string]any) {
			delete(d["hosts"].([]any)[1].(map[string]any)["ipmi"].(map[string]any), "password")
		},
		"duplicate hostname": func(d map[string]any) {
			d["hosts"].([]any)[1].(map[string]any)["hostname"] = "veritech01.labtwo"
		},
		"unknown pdu ref": func(d map[string]any) {
			d["hosts"].([]any)[0].(map[string]any)["pdu"] = []any{map[string]any{"name": "nope", "outlet": 1}}
		},
		"pdu missing style": func(d map[string]any) {
			delete(d["pdus"].(map[string]any)["eatonpdu01.labtwo"].(map[string]any), "pdustyle")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := secretData()
			mutate(d)
			if _, err := ParseSecret(d); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestStaticSource(t *testing.T) {
	src := Static{Inventory: Inventory{Hosts: []Host{{Hostname: "a", Ipmi: Ipmi{Username: "u", Password: "p"}}}}}
	inv, err := src.Load(context.Background())
	if err != nil || len(inv.Hosts) != 1 {
		t.Fatalf("inv=%+v err=%v", inv, err)
	}
	bad := Static{Inventory: Inventory{Hosts: []Host{{Hostname: "a"}}}}
	if _, err := bad.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "ipmi") {
		t.Fatalf("want ipmi validation error, got %v", err)
	}
}

func TestVaultSourceRequiresEnv(t *testing.T) {
	v := Vault{AddrEnv: "PM_TEST_ADDR_UNSET", TokenEnv: "PM_TEST_TOKEN_UNSET", SecretPath: "infra/hosts"}
	if _, err := v.Load(context.Background()); err == nil {
		t.Fatal("want error when env unset")
	}
}
