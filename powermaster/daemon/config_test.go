package powermaster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
)

func TestExampleConfigMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile("config.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	def := DefaultConfig()
	// The example carries illustrative classes; everything else is the default.
	cfg.Classes = nil
	// vault.hosts_secret_path is required, so it has no default: take the example's.
	if cfg.Vault.HostsSecretPath == "" {
		t.Fatal("example must set vault.hosts_secret_path")
	}
	def.Vault.HostsSecretPath = cfg.Vault.HostsSecretPath
	if !reflect.DeepEqual(cfg, def) {
		t.Fatalf("example != defaults\nexample: %+v\ndefault: %+v", cfg, def)
	}
}

func TestDefaultTUIAPIURLIsEmpty(t *testing.T) {
	if u := DefaultConfig().TUI.APIURL; u != "" {
		t.Fatalf("tui.api_url default = %q, want empty (no lab URL compiled in)", u)
	}
}

func TestResolveMonitorURLPrecedence(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "powermaster")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(" https://file.example.com/ \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == "POWERMASTER_URL" {
			return "https://env.example.com/"
		}
		return ""
	}
	none := func(string) string { return "" }
	cases := []struct {
		name, flag, cfg string
		getenv          func(string) string
		home, want      string
	}{
		{"flag wins", "https://flag.example.com/", "https://cfg.example.com", env, home, "https://flag.example.com"},
		{"config beats env", "", "https://cfg.example.com", env, home, "https://cfg.example.com"},
		{"env beats file", "", "", env, home, "https://env.example.com"},
		{"file last", "", "", none, home, "https://file.example.com"},
	}
	for _, c := range cases {
		got, err := ResolveMonitorURL(c.flag, c.cfg, c.getenv, c.home)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	_, err := ResolveMonitorURL("", "", none, t.TempDir())
	want := "powermaster URL not set: use --api-url, tui.api_url in --config, POWERMASTER_URL, or write it to ~/.config/powermaster/url"
	if err == nil || err.Error() != want {
		t.Fatalf("unset: %v", err)
	}
}

func TestDefaultPollIntervalsAreTwoSeconds(t *testing.T) {
	p := DefaultConfig().Polling
	for name, d := range map[string]Duration{"ipmi": p.IPMIInterval, "network": p.NetworkInterval, "pdu": p.PDUInterval} {
		if d.D() != 2*time.Second {
			t.Errorf("%s interval = %v, want 2s", name, d.D())
		}
	}
}

func TestParseConfigDurationsAndDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("polling:\n  ipmi_interval: 2m\ntimings:\n  chassis_on_wait: 90s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Polling.IPMIInterval.D() != 2*time.Minute {
		t.Fatalf("ipmi_interval = %v", cfg.Polling.IPMIInterval.D())
	}
	if cfg.Polling.NetworkInterval.D() != 2*time.Second {
		t.Fatalf("network_interval default lost: %v", cfg.Polling.NetworkInterval.D())
	}
	if cfg.Timings.ChassisOnWait.D() != 90*time.Second || cfg.Timings.PingSSHWait.D() != 300*time.Second {
		t.Fatalf("timings = %+v", cfg.Timings)
	}
	if cfg.Listen != ":8092" || cfg.Events.RingSize != 1000 || cfg.TUI.LockedTaps != 4 {
		t.Fatalf("defaults lost: %+v", cfg)
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key": "listne: \":1\"\n",
		"unknown nested key":    "polling:\n  ipmi_intreval: 1s\n",
		"unknown timing key":    "timings:\n  chasis_on_wait: 1s\n",
		"unknown class key":     "classes:\n  a:\n    hostz: [x]\n",
		"unknown class timing":  "classes:\n  a:\n    hosts: [x]\n    timings:\n      bogus: 1s\n",
		"duration without unit": "timings:\n  chassis_on_wait: 30\n",
		"bad duration":          "recovery_interval: soon\n",
		"zero duration":         "polling:\n  pdu_interval: 0s\n",
		"negative class timing": "classes:\n  a:\n    hosts: [x]\n    timings:\n      ping_ssh_wait: -1s\n",
		"host in two classes":   "classes:\n  a:\n    hosts: [x]\n  b:\n    hosts: [x]\n",
		"pdu in two classes":    "classes:\n  a:\n    pdus: [p]\n  b:\n    pdus: [p]\n",
		"bad log level":         "log_level: loud\n",
		"zero ring size":        "events:\n  ring_size: 0\n",
		"secret-looking key":    "vault:\n  token: abc\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(doc)); err == nil {
				t.Fatalf("want error for %q", doc)
			}
		})
	}
}

func testInventory() *inventory.Inventory {
	inv := &inventory.Inventory{
		Hosts: []inventory.Host{
			{Hostname: "h1", Ipmi: inventory.Ipmi{Username: "u", Password: "p"},
				Pdu: []inventory.PduRef{{Name: "eaton", Outlet: 1}, {Name: "kasa", Outlet: 2}}},
			{Hostname: "h2", Ipmi: inventory.Ipmi{Username: "u", Password: "p"},
				Pdu: []inventory.PduRef{{Name: "plain", Outlet: 4}}},
			{Hostname: "h3", Ipmi: inventory.Ipmi{Username: "u", Password: "p"},
				Pdu: []inventory.PduRef{{Name: "plain", Outlet: 5}}},
		},
		Pdus: map[string]inventory.Pdu{
			"eaton": {IP: "x", Username: "u", Password: "p", PduStyle: "s"},
			"kasa":  {IP: "x", Username: "u", Password: "p", PduStyle: "s"},
			"plain": {IP: "x", Username: "u", Password: "p", PduStyle: "s"},
		},
	}
	if err := inv.Validate(); err != nil {
		panic(err)
	}
	return inv
}

const classDoc = `
classes:
  fast:
    hosts: [h1]
    timings:
      chassis_on_wait: 100s
      pdu_outlet_wait: 50s
      deadline_slack: 10s
  eaton:
    pdus: [eaton]
    timings:
      pdu_outlet_wait: 20s
`

func TestResolveClasses(t *testing.T) {
	cfg, err := ParseConfig([]byte(classDoc))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := Resolve(cfg, testInventory())
	if err != nil {
		t.Fatal(err)
	}

	// h1: host class overrides defaults; PDU class beats host class for its
	// outlet; the other outlet takes the host class value.
	h1 := plans["h1"]
	if h1.Class != "fast" {
		t.Fatalf("h1 class = %q", h1.Class)
	}
	if h1.Timings.ChassisOnWait != 100*time.Second || h1.Timings.PingSSHWait != 300*time.Second {
		t.Fatalf("h1 timings = %+v", h1.Timings)
	}
	if len(h1.Outlets) != 2 || h1.Outlets[0].Wait != 20*time.Second || h1.Outlets[1].Wait != 50*time.Second {
		t.Fatalf("h1 outlets = %+v", h1.Outlets)
	}
	// powerup = outlets(20+50) + ipmi_ping 300 + chassis_on 100 + ping_ssh 300
	if got := h1.Budgets.Powerup; got != 770*time.Second {
		t.Fatalf("h1 powerup budget = %v", got)
	}
	// powercut = powerdown + pdu_outlet_wait per outlet (20+50)
	if got := h1.Budgets.Powercut - h1.Budgets.Powerdown; got != 70*time.Second {
		t.Fatalf("h1 powercut-powerdown = %v", got)
	}
	if h1.Slack != 10*time.Second {
		t.Fatalf("h1 slack = %v", h1.Slack)
	}

	// h2: no class, default everything; one outlet at 30s.
	h2 := plans["h2"]
	if h2.Class != "" || h2.Outlets[0].Wait != 30*time.Second {
		t.Fatalf("h2 = %+v", h2)
	}
	want := Budgets{Powerup: 930 * time.Second, Powerdown: 760 * time.Second, Powercut: 790 * time.Second, Reboot: 870 * time.Second}
	if h2.Budgets != want {
		t.Fatalf("h2 budgets = %+v, want %+v", h2.Budgets, want)
	}
	if h2.Slack != 60*time.Second || h2.Deadline(ActionPowerup) != 990*time.Second {
		t.Fatalf("h2 slack/deadline = %v/%v", h2.Slack, h2.Deadline(ActionPowerup))
	}

	// h3: one outlet, classless: same budgets as h2.
	if h3 := plans["h3"]; h3.Budgets != want {
		t.Fatalf("h3 budgets = %+v, want %+v", h3.Budgets, want)
	}
}

func TestResolveRefusesHostWithoutOutlets(t *testing.T) {
	inv := testInventory()
	inv.Hosts = append(inv.Hosts,
		inventory.Host{Hostname: "vocal-dove", Ipmi: inventory.Ipmi{Username: "u", Password: "p"}},
		inventory.Host{Hostname: "x10sdv02", Ipmi: inventory.Ipmi{Username: "u", Password: "p"}})
	_, err := Resolve(DefaultConfig(), inv)
	if err == nil || !strings.Contains(err.Error(), "vocal-dove") || !strings.Contains(err.Error(), "x10sdv02") {
		t.Fatalf("want error naming both hosts, got %v", err)
	}
	if strings.Contains(err.Error(), "h1") {
		t.Fatalf("error names a host that has outlets: %v", err)
	}
}

func writeConfig(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func staticLoader(inv *inventory.Inventory, err error) InventoryLoader {
	return func(context.Context, Config) (*inventory.Inventory, error) { return inv, err }
}

const hostsDoc = "vault:\n  hosts_secret_path: test/hosts\n"

func TestHostsSecretPathIsRequiredToServeAndCheck(t *testing.T) {
	// Parsing alone accepts it (monitor never reads the inventory).
	cfg, err := ParseConfig([]byte(""))
	if err != nil {
		t.Fatalf("empty config must parse: %v", err)
	}
	if cfg.Vault.HostsSecretPath != "" {
		t.Fatalf("hosts_secret_path has a default: %q", cfg.Vault.HostsSecretPath)
	}
	if err := cfg.ValidateInventory(); err == nil || !strings.Contains(err.Error(), "vault.hosts_secret_path is required") {
		t.Fatalf("ValidateInventory = %v", err)
	}
	called := false
	load := func(context.Context, Config) (*inventory.Inventory, error) {
		called = true
		return testInventory(), nil
	}
	_, err = Check(context.Background(), writeConfig(t, "log_level: info\n"), load)
	if err == nil || !strings.Contains(err.Error(), "vault.hosts_secret_path is required") {
		t.Fatalf("Check = %v", err)
	}
	if called {
		t.Fatal("inventory loaded despite missing hosts_secret_path")
	}
	cfg, err = ParseConfig([]byte(hostsDoc))
	if err != nil || cfg.ValidateInventory() != nil {
		t.Fatalf("with path: %v / %v", err, cfg.ValidateInventory())
	}
}

func TestCheckConfig(t *testing.T) {
	ctx := context.Background()
	ok := writeConfig(t, hostsDoc+classDoc)
	n, err := Check(ctx, ok, staticLoader(testInventory(), nil))
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}

	noOutlets := testInventory()
	noOutlets.Hosts = append(noOutlets.Hosts, inventory.Host{Hostname: "vocal-dove", Ipmi: inventory.Ipmi{Username: "u", Password: "p"}})
	if _, err := Check(ctx, ok, staticLoader(noOutlets, nil)); err == nil || !strings.Contains(err.Error(), "vocal-dove") {
		t.Fatalf("want no-outlets error naming vocal-dove, got %v", err)
	}
	if _, err := Check(ctx, writeConfig(t, hostsDoc+"classes:\n  a:\n    hosts: [nope]\n"), staticLoader(testInventory(), nil)); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want class error, got %v", err)
	}
	if _, err := Check(ctx, writeConfig(t, hostsDoc+"log_level: loud\n"), staticLoader(testInventory(), nil)); err == nil {
		t.Fatal("want config error")
	}
	if _, err := Check(ctx, filepath.Join(t.TempDir(), "missing.yaml"), staticLoader(testInventory(), nil)); err == nil {
		t.Fatal("want missing-file error")
	}
	if _, err := Check(ctx, ok, staticLoader(nil, errors.New("vault down"))); err == nil || !strings.Contains(err.Error(), "vault down") {
		t.Fatalf("want inventory error, got %v", err)
	}
}

func TestResolveRejectsUnknownMembers(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown host": "classes:\n  a:\n    hosts: [nope]\n",
		"unknown pdu":  "classes:\n  a:\n    pdus: [nope]\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Resolve(cfg, testInventory())
			if err == nil || !strings.Contains(err.Error(), "nope") {
				t.Fatalf("want error naming nope, got %v", err)
			}
		})
	}
}

func TestBudgetStrings(t *testing.T) {
	b := Budgets{Powerup: 930 * time.Second, Powerdown: 760 * time.Second, Reboot: 780 * time.Second}
	got := b.Strings()
	if got["powerup"] != "930s" || got["powerdown"] != "760s" || got["reboot"] != "780s" {
		t.Fatalf("strings = %v", got)
	}
}

func TestPersonalKeysRequestURL(t *testing.T) {
	if DefaultConfig().PersonalKeys.RequestURL != "" {
		t.Fatal("request_url has a default")
	}
	cfg, err := ParseConfig(nil)
	if err != nil || cfg.PersonalKeys.RequestURL != "" {
		t.Fatalf("absent: %+v %v", cfg, err)
	}
	for _, u := range []string{
		"https://keys.example.com/v1/services/powermaster/requests",
		"http://provider.example:8080/req?x=1",
		"HTTPS://Keys.Example.com/r",
	} {
		cfg, err := ParseConfig([]byte("personal_keys:\n  request_url: \"" + u + "\"\n"))
		if err != nil || cfg.PersonalKeys.RequestURL != u {
			t.Errorf("%q: %q %v", u, cfg.PersonalKeys.RequestURL, err)
		}
	}
	for name, y := range map[string]string{
		"relative":    "personal_keys:\n  request_url: /v1/requests\n",
		"no scheme":   "personal_keys:\n  request_url: keys.example.com/r\n",
		"ftp":         "personal_keys:\n  request_url: ftp://keys.example.com/r\n",
		"no host":     "personal_keys:\n  request_url: \"https:///r\"\n",
		"userinfo":    "personal_keys:\n  request_url: https://u:p@keys.example.com/r\n",
		"fragment":    "personal_keys:\n  request_url: https://keys.example.com/r#f\n",
		"bare #":      "personal_keys:\n  request_url: \"https://keys.example.com/r#\"\n",
		"unknown key": "personal_keys:\n  request_url: https://keys.example.com/r\n  url: https://keys.example.com\n",
	} {
		_, err := ParseConfig([]byte(y))
		want := "personal_keys.request_url"
		if name == "unknown key" {
			want = "url"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckSuffix(t *testing.T) {
	if s := DefaultConfig().CheckSuffix(); s != "" {
		t.Fatalf("unset: %q", s)
	}
	cfg, err := ParseConfig([]byte("personal_keys:\n  request_url: https://keys.example.com/r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.CheckSuffix(); s != " key_request=on" {
		t.Fatalf("set: %q", s)
	}
}

func TestResolveKeyRequestURL(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvKeyRequestURL {
				return v
			}
			return ""
		}
	}
	cases := []struct{ flag, env, want string }{
		{"", "", ""},
		{"  https://keys.example/r ", "https://other.example/r", "https://keys.example/r"},
		{"", " https://other.example/r\n", "https://other.example/r"},
		{"   ", "", ""},
	}
	for _, c := range cases {
		if got := ResolveKeyRequestURL(c.flag, env(c.env)); got != c.want {
			t.Errorf("ResolveKeyRequestURL(%q, env %q) = %q, want %q", c.flag, c.env, got, c.want)
		}
	}
	if EnvKeyRequestURL != "POWERMASTER_KEY_REQUEST_URL" {
		t.Fatalf("EnvKeyRequestURL = %q", EnvKeyRequestURL)
	}
}
