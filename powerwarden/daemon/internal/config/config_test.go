package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExampleHoldsEveryDefault(t *testing.T) {
	cfg, err := Load("../../config.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	// powermaster.url is required, so it has no default: take the example's.
	def := Default()
	def.Powermaster.URL = cfg.Powermaster.URL
	if cfg.Powermaster.URL == "" || !reflect.DeepEqual(cfg, def) {
		t.Fatalf("example differs from Default():\n%+v\n%+v", cfg, def)
	}
}

func TestMinimalFileIsDefaults(t *testing.T) {
	cfg, err := Parse([]byte("powermaster:\n  url: http://pm.example:8092\n"))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	def.Powermaster.URL = "http://pm.example:8092"
	if !reflect.DeepEqual(cfg, def) {
		t.Fatal("config with only the required keys should equal defaults")
	}
}

func TestPowermasterURLIsRequired(t *testing.T) {
	if Default().Powermaster.URL != "" {
		t.Fatalf("powermaster.url has a default: %q", Default().Powermaster.URL)
	}
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "powermaster.url is required") {
		t.Fatalf("Parse(nil) = %v", err)
	}
}

// parseReq parses y after the one key that is required and has no default,
// so tests of other keys are not decided by powermaster.url.
func parseReq(y string) (Config, error) {
	return Parse([]byte("powermaster:\n  url: http://pm.example:8092\n" + y))
}

func TestOverride(t *testing.T) {
	cfg, err := parseReq("listen: \":9000\"\ntimings:\n  boot_grace: 2m\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9000" || cfg.Timings.BootGrace != 2*time.Minute {
		t.Fatalf("override not applied: %+v", cfg)
	}
	if cfg.Timings.BootCooldown != 10*time.Minute {
		t.Fatal("unset key lost its default")
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"unknown top key":    "bogus: 1\n",
		"unknown nested key": "timings:\n  boot_graec: 5m\n",
		"zero duration":      "timings:\n  power_on_spacing: 0s\n",
		"lease bounds":       "timings:\n  worker_lease_min: 120s\n",
		"bad duration":       "timings:\n  boot_grace: soon\n",
		"bad limit":          "limits:\n  max_demands: 0\n",
		"bad log level":      "log_level: chatty\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseReq(y); err == nil {
				t.Fatalf("expected error for %q", strings.TrimSpace(y))
			}
		})
	}
}

func TestColdOffAfter(t *testing.T) {
	if Default().Timings.ColdOffAfter != time.Hour {
		t.Fatal("default cold_off_after is not 1h")
	}
	cfg, err := parseReq("timings:\n  cold_off_after: 0s\n")
	if err != nil || cfg.Timings.ColdOffAfter != 0 {
		t.Fatalf("0 must be valid and disable powercut: %v %+v", err, cfg.Timings)
	}
	if _, err := parseReq("timings:\n  cold_off_after: -1m\n"); err == nil {
		t.Fatal("negative cold_off_after accepted")
	}
}

// W6: configs from the Redis-source era fail at startup with a clear message.
func TestRemovedKeys(t *testing.T) {
	for _, y := range []string{
		"demand_source: http\n",
		"demand_source: redis\n",
		"redis:\n  addr_env: HOURCH_REDIS_ADDR\n",
	} {
		_, err := parseReq(y)
		if err == nil || !strings.Contains(err.Error(), "was removed") {
			t.Fatalf("%q: want a removed-key error, got %v", strings.TrimSpace(y), err)
		}
	}
}

func TestHostTagsRejectHostNames(t *testing.T) {
	base := "host_tags:\n  veritech01.labtwo: [kernel]\n  veritech02.labtwo: [%s]\n"
	for _, bad := range []string{"veritech01.labtwo", "veritech01", "veritech02", "kernel, veritech02.labtwo"} {
		if _, err := parseReq(strings.Replace(base, "%s", bad, 1)); err == nil {
			t.Errorf("host_tags value %q accepted", bad)
		}
	}
	if _, err := parseReq(strings.Replace(base, "%s", "kernel, tiny", 1)); err != nil {
		t.Fatalf("ordinary tags rejected: %v", err)
	}
}

func TestClaimDefaultsAndBounds(t *testing.T) {
	c := Default()
	tm := c.Timings
	if tm.ClaimLeaseDefault != 15*time.Minute || tm.ClaimLeaseMin != time.Minute || tm.ClaimLeaseMax != time.Hour ||
		tm.ClaimCapDefault != 8*time.Hour || tm.ClaimCapMin != 15*time.Minute || tm.ClaimCapMax != 72*time.Hour ||
		tm.ClaimRestartGrace != 5*time.Minute || tm.ClaimWaitMax != time.Minute || tm.ClaimSweepAfter != 24*time.Hour ||
		c.Limits.MaxClaims != 64 {
		t.Fatalf("claim defaults wrong: %+v %+v", tm, c.Limits)
	}
	for name, y := range map[string]string{
		"lease order":  "timings:\n  claim_lease_min: 2h\n",
		"cap order":    "timings:\n  claim_cap_default: 100h\n",
		"zero grace":   "timings:\n  claim_restart_grace: 0s\n",
		"zero sweep":   "timings:\n  claim_sweep_after: 0s\n",
		"fractional":   "timings:\n  claim_lease_default: 90500ms\n",
		"max claims 0": "limits:\n  max_claims: 0\n",
	} {
		if _, err := parseReq(y); err == nil {
			t.Errorf("%s: accepted %q", name, y)
		}
	}
}

func TestUIPathPrefix(t *testing.T) {
	if got := Default().UI.PathPrefix; got != "" {
		t.Fatalf("default ui.path_prefix = %q, want empty", got)
	}
	for _, ok := range []string{"", "/p", "/a/b", "/power-warden_1.x~"} {
		cfg, err := parseReq("ui:\n  path_prefix: \"" + ok + "\"\n")
		if err != nil || cfg.UI.PathPrefix != ok {
			t.Errorf("prefix %q: %v %q", ok, err, cfg.UI.PathPrefix)
		}
	}
	for _, bad := range []string{"p", "/p/", "/", "/a/../b", "/..", "/a//b", "/a b", "/a{b}", "/a?b", "/a#b", "/a%2fb", "/./a", "//p"} {
		_, err := parseReq("ui:\n  path_prefix: \"" + bad + "\"\n")
		if err == nil || !strings.Contains(err.Error(), "ui.path_prefix") {
			t.Errorf("prefix %q accepted or unnamed error: %v", bad, err)
		}
	}
}

const proxyYAML = "ui:\n  auth: proxy\n  proxy:\n    secret_file: /etc/powerwarden/ui-proxy.secret\n    secret_header: X-Example-Proxy-Secret\n    user_header: X-Example-User\n"

func TestUIAuthDefaultsToHtpasswd(t *testing.T) {
	if got := Default().UI.Auth; got != "htpasswd" {
		t.Fatalf("default ui.auth = %q", got)
	}
	cfg, err := parseReq("ui:\n  auth: htpasswd\n")
	if err != nil || cfg.UI.Auth != "htpasswd" {
		t.Fatalf("explicit htpasswd: %v %q", err, cfg.UI.Auth)
	}
	if p := Default().UI.Proxy; p != (UIProxy{}) {
		t.Fatalf("ui.proxy has defaults: %+v", p)
	}
}

func TestUIProxyAuth(t *testing.T) {
	cfg, err := parseReq(proxyYAML)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.Auth != "proxy" || cfg.UI.Proxy.SecretFile != "/etc/powerwarden/ui-proxy.secret" ||
		cfg.UI.Proxy.SecretHeader != "X-Example-Proxy-Secret" || cfg.UI.Proxy.UserHeader != "X-Example-User" {
		t.Fatalf("%+v", cfg.UI)
	}
	// ui.enabled false: the proxy keys are not required.
	if _, err := parseReq("ui:\n  enabled: false\n  auth: proxy\n"); err != nil {
		t.Fatalf("disabled UI: %v", err)
	}
}

func TestUIProxyRequiredKeys(t *testing.T) {
	keys := []string{"secret_file", "secret_header", "user_header"}
	for _, key := range keys {
		y := "ui:\n  auth: proxy\n  proxy:\n"
		for _, k := range keys {
			if k != key {
				y += "    " + k + ": X-Example-" + strings.ReplaceAll(k, "_", "-") + "\n"
			}
		}
		_, err := parseReq(y)
		if err == nil || !strings.Contains(err.Error(), "ui.proxy."+key) {
			t.Errorf("missing %s: %v", key, err)
		}
	}
	_, err := parseReq("ui:\n  auth: proxy\n")
	for _, key := range keys {
		if err == nil || !strings.Contains(err.Error(), "ui.proxy."+key) {
			t.Errorf("all missing, %s not named: %v", key, err)
		}
	}
}

func TestUIAuthRejects(t *testing.T) {
	cases := map[string]struct{ y, want string }{
		"unknown auth":             {"ui:\n  auth: oauth\n", "ui.auth"},
		"bad secret header":        {strings.Replace(proxyYAML, "X-Example-Proxy-Secret", "X Bad", 1), "ui.proxy.secret_header"},
		"bad user header":          {strings.Replace(proxyYAML, "X-Example-User", "X:Bad", 1), "ui.proxy.user_header"},
		"same headers":             {strings.Replace(proxyYAML, "X-Example-User", "x-example-proxy-secret", 1), "differ"},
		"proxy keys with htpasswd": {"ui:\n  proxy:\n    user_header: X-Example-User\n", "ui.proxy"},
	}
	for name, c := range cases {
		_, err := parseReq(c.y)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
}
