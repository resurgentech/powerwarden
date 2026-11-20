// Package powermaster is the core of the powermaster daemon: configuration,
// the per-host state machine, power sequences, pollers and recovery.
package powermaster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
)

// Actions.
const (
	ActionPowerup   = "powerup"
	ActionPowerdown = "powerdown"
	ActionPowercut  = "powercut"
	ActionReboot    = "reboot"
)

// Duration is a time.Duration written in Go syntax ("30s", "5m") in YAML.
type Duration time.Duration

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a Go duration string. Bare numbers are rejected.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("line %d: duration %q must be a string with a unit, like 30s", n.Line, n.Value)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %v", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the Go duration string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// VaultConfig names where inventory comes from. Values are environment
// variable names, never secrets.
type VaultConfig struct {
	AddrEnv         string `yaml:"addr_env"`
	TokenEnv        string `yaml:"token_env"`
	SkipVerifyEnv   string `yaml:"skip_verify_env"`
	HostsSecretPath string `yaml:"hosts_secret_path"`
}

// PollingConfig sets poll intervals.
type PollingConfig struct {
	IPMIInterval    Duration `yaml:"ipmi_interval"`
	NetworkInterval Duration `yaml:"network_interval"`
	PDUInterval     Duration `yaml:"pdu_interval"`
}

// EventsConfig sizes the event ring and heartbeat.
type EventsConfig struct {
	RingSize  int      `yaml:"ring_size"`
	Heartbeat Duration `yaml:"heartbeat"`
}

// Timings are the stage times. The top-level `timings` block holds every
// key; a class overrides any subset (ClassTimings).
type Timings struct {
	PduOutletWait        Duration `yaml:"pdu_outlet_wait"`
	IpmiPingWait         Duration `yaml:"ipmi_ping_wait"`
	ChassisOnWait        Duration `yaml:"chassis_on_wait"`
	PingSSHWait          Duration `yaml:"ping_ssh_wait"`
	SSHShutdownWait      Duration `yaml:"ssh_shutdown_wait"`
	PostSSHOffWait       Duration `yaml:"post_ssh_off_wait"`
	IpmiOffCommandWait   Duration `yaml:"ipmi_off_command_wait"`
	FinalOffWait         Duration `yaml:"final_off_wait"`
	RebootChassisOffWait Duration `yaml:"reboot_chassis_off_wait"`
	RebootPduSettle      Duration `yaml:"reboot_pdu_settle"`
	DeadlineSlack        Duration `yaml:"deadline_slack"`
}

// ClassTimings is a partial Timings: nil means "not overridden".
type ClassTimings struct {
	PduOutletWait        *Duration `yaml:"pdu_outlet_wait"`
	IpmiPingWait         *Duration `yaml:"ipmi_ping_wait"`
	ChassisOnWait        *Duration `yaml:"chassis_on_wait"`
	PingSSHWait          *Duration `yaml:"ping_ssh_wait"`
	SSHShutdownWait      *Duration `yaml:"ssh_shutdown_wait"`
	PostSSHOffWait       *Duration `yaml:"post_ssh_off_wait"`
	IpmiOffCommandWait   *Duration `yaml:"ipmi_off_command_wait"`
	FinalOffWait         *Duration `yaml:"final_off_wait"`
	RebootChassisOffWait *Duration `yaml:"reboot_chassis_off_wait"`
	RebootPduSettle      *Duration `yaml:"reboot_pdu_settle"`
	DeadlineSlack        *Duration `yaml:"deadline_slack"`
}

// Class groups hosts and/or PDUs that share timing overrides.
type Class struct {
	Hosts   []string     `yaml:"hosts"`
	Pdus    []string     `yaml:"pdus"`
	Timings ClassTimings `yaml:"timings"`
}

// TUIConfig configures `powermaster monitor`.
type TUIConfig struct {
	APIURL           string   `yaml:"api_url"`
	TokenFile        string   `yaml:"token_file"`
	LockedTaps       int      `yaml:"locked_taps"`
	LockedTapGap     Duration `yaml:"locked_tap_gap"`
	LockedForceDelay Duration `yaml:"locked_force_delay"`
}

// Config is config.yaml. Every key is listed in config.yaml.example.
type Config struct {
	Listen           string           `yaml:"listen"`
	StatePath        string           `yaml:"state_path"`
	TokensFile       string           `yaml:"tokens_file"`
	LogLevel         string           `yaml:"log_level"`
	Vault            VaultConfig      `yaml:"vault"`
	Polling          PollingConfig    `yaml:"polling"`
	RecoveryInterval Duration         `yaml:"recovery_interval"`
	Events           EventsConfig     `yaml:"events"`
	Timings          Timings          `yaml:"timings"`
	Classes          map[string]Class `yaml:"classes"`
	TUI              TUIConfig        `yaml:"tui"`
	// PersonalKeys is optional and off by default.
	PersonalKeys PersonalKeys `yaml:"personal_keys"`
}

// PersonalKeys configures key request discovery and the pulled key list
// (keyclient's docs/CONTRACT.md, "Key list").
type PersonalKeys struct {
	// RequestURL is advertised unauthenticated by GET /v1/key-request: where a
	// person requests a key for this service. Empty turns the route off (404).
	RequestURL string `yaml:"request_url"`
	// URL is the key-list URL. Empty turns the key list off: nothing pulls and
	// nothing changes.
	URL string `yaml:"url"`
	// TokenFile holds the pull token (optional).
	TokenFile string `yaml:"token_file"`
	// CacheFile is required when URL is set; it has no default.
	CacheFile string `yaml:"cache_file"`
	// Refresh is the time between pulls; must be greater than zero.
	Refresh Duration `yaml:"refresh"`
	// RefreshOnMiss is the least time between pulls started by an unknown key;
	// zero turns refresh on miss off.
	RefreshOnMiss Duration `yaml:"refresh_on_miss"`
	// AllowInsecureHTTP allows a plain-http URL to a host that is not loopback.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`

	// present records which sub-keys appeared in the YAML file, whatever their
	// value: "set" is presence-based (a sub-key without a url is an error even
	// when it holds its default).
	present presentKeys
}

type presentKeys struct{ tokenFile, cacheFile, refresh, refreshOnMiss, allowInsecureHTTP bool }

// Defaults of the key-list settings (SR-1).
const (
	DefaultRefresh       = time.Minute
	DefaultRefreshOnMiss = 5 * time.Second
)

// UnmarshalYAML decodes the block onto the values already in p (the defaults),
// records which sub-keys were present, and refuses unknown keys as the
// top-level decoder does (Node.Decode does not inherit KnownFields).
func (p *PersonalKeys) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: personal_keys must be a mapping", n.Line)
	}
	var pr presentKeys
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "request_url", "url":
		case "token_file":
			pr.tokenFile = true
		case "cache_file":
			pr.cacheFile = true
		case "refresh":
			pr.refresh = true
		case "refresh_on_miss":
			pr.refreshOnMiss = true
		case "allow_insecure_http":
			pr.allowInsecureHTTP = true
		default:
			return fmt.Errorf("line %d: field %s not found in type powermaster.PersonalKeys", n.Content[i].Line, k)
		}
	}
	type plain PersonalKeys // no UnmarshalYAML: avoids recursion
	q := plain(*p)
	if err := n.Decode(&q); err != nil {
		return err
	}
	*p = PersonalKeys(q)
	p.present = pr
	return nil
}

func (p PersonalKeys) setTokenFile() bool { return p.present.tokenFile || p.TokenFile != "" }
func (p PersonalKeys) setCacheFile() bool { return p.present.cacheFile || p.CacheFile != "" }

// For a Config built in code, a zero duration also counts as unset, so a
// PersonalKeys literal made without DefaultConfig() is "off" rather than an error.
func (p PersonalKeys) setRefresh() bool {
	return p.present.refresh || (p.Refresh != 0 && p.Refresh.D() != DefaultRefresh)
}
func (p PersonalKeys) setRefreshOnMiss() bool {
	return p.present.refreshOnMiss || (p.RefreshOnMiss != 0 && p.RefreshOnMiss.D() != DefaultRefreshOnMiss)
}
func (p PersonalKeys) setAllowInsecureHTTP() bool {
	return p.present.allowInsecureHTTP || p.AllowInsecureHTTP
}

// CheckSuffix is appended to the `powermaster check` ok line: " key_request=on"
// when personal_keys.request_url is set, else empty.
func (c Config) CheckSuffix() string {
	if c.PersonalKeys.RequestURL != "" {
		return " key_request=on"
	}
	return ""
}

// validRequestURL accepts "" (off) or an absolute http or https URL with a
// host, no userinfo (it is advertised unauthenticated) and no fragment.
func validRequestURL(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("not a valid URL")
	}
	if sc := strings.ToLower(u.Scheme); sc != "http" && sc != "https" {
		return errors.New("must be an absolute http or https URL")
	}
	if u.Hostname() == "" {
		return errors.New("must have a host")
	}
	if u.User != nil {
		return errors.New("must not contain user information")
	}
	if u.Fragment != "" || strings.Contains(s, "#") {
		return errors.New("must not contain a fragment")
	}
	return nil
}

func sec(n int) Duration { return Duration(time.Duration(n) * time.Second) }

// DefaultConfig returns the defaults (the values in config.yaml.example,
// without its illustrative classes).
func DefaultConfig() Config {
	return Config{
		Listen:     ":8092",
		StatePath:  "/var/lib/powermaster/state.db",
		TokensFile: "/etc/powermaster/tokens.yaml",
		LogLevel:   "info",
		Vault: VaultConfig{
			AddrEnv:       "VAULT_ADDR",
			TokenEnv:      "VAULT_TOKEN",
			SkipVerifyEnv: "VAULT_SKIP_VERIFY",
			// HostsSecretPath has no default: see ValidateInventory.
		},
		Polling:          PollingConfig{IPMIInterval: sec(2), NetworkInterval: sec(2), PDUInterval: sec(2)},
		RecoveryInterval: sec(15),
		Events:           EventsConfig{RingSize: 1000, Heartbeat: sec(15)},
		Timings: Timings{
			PduOutletWait:        sec(30),
			IpmiPingWait:         sec(300),
			ChassisOnWait:        sec(300),
			PingSSHWait:          sec(300),
			SSHShutdownWait:      sec(10),
			PostSSHOffWait:       sec(300),
			IpmiOffCommandWait:   sec(120),
			FinalOffWait:         sec(300),
			RebootChassisOffWait: sec(60),
			RebootPduSettle:      sec(30),
			DeadlineSlack:        sec(60),
		},
		PersonalKeys: PersonalKeys{Refresh: Duration(DefaultRefresh), RefreshOnMiss: Duration(DefaultRefreshOnMiss)},
		TUI: TUIConfig{
			APIURL:           "",
			TokenFile:        "~/.config/powermaster/token",
			LockedTaps:       4,
			LockedTapGap:     sec(3),
			LockedForceDelay: sec(2),
		},
	}
}

// ResolveMonitorURL picks the API base URL for `powermaster monitor`: the
// --api-url flag, then tui.api_url from --config, then POWERMASTER_URL, then the
// one-line file ~/.config/powermaster/url (whitespace trimmed). No URL is
// compiled in; the error names every source when none is set.
func ResolveMonitorURL(flagURL, cfgURL string, getenv func(string) string, home string) (string, error) {
	for _, v := range []string{flagURL, cfgURL, getenv("POWERMASTER_URL")} {
		if v = strings.TrimSpace(v); v != "" {
			return strings.TrimRight(v, "/"), nil
		}
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "powermaster", "url"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("url file: %w", err)
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		return strings.TrimRight(v, "/"), nil
	}
	return "", errors.New("powermaster URL not set: use --api-url, tui.api_url in --config, POWERMASTER_URL, or write it to ~/.config/powermaster/url")
}

// LoadConfig reads and parses a config file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig decodes YAML over the defaults. Unknown keys, bad durations and
// class conflicts are errors.
func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ValidateInventory checks what `serve` and `check` need to read the inventory:
// vault.hosts_secret_path, which has no default. `monitor` never reads the
// inventory, so ParseConfig does not require it.
func (c Config) ValidateInventory() error {
	if c.Vault.HostsSecretPath == "" {
		return fmt.Errorf("vault.hosts_secret_path is required")
	}
	return nil
}

// Validate checks values that do not depend on inventory.
func (c Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q: want debug, info, warn or error", c.LogLevel)
	}
	if c.StatePath == "" {
		return fmt.Errorf("state_path required")
	}
	if c.Events.RingSize < 1 {
		return fmt.Errorf("events.ring_size must be >= 1")
	}
	if c.TUI.LockedTaps < 1 {
		return fmt.Errorf("tui.locked_taps must be >= 1")
	}
	pos := map[string]Duration{
		"polling.ipmi_interval":    c.Polling.IPMIInterval,
		"polling.network_interval": c.Polling.NetworkInterval,
		"polling.pdu_interval":     c.Polling.PDUInterval,
		"recovery_interval":        c.RecoveryInterval,
		"events.heartbeat":         c.Events.Heartbeat,
		"tui.locked_tap_gap":       c.TUI.LockedTapGap,
	}
	for k, v := range pos {
		if v <= 0 {
			return fmt.Errorf("%s must be > 0", k)
		}
	}
	if c.TUI.LockedForceDelay < 0 {
		return fmt.Errorf("tui.locked_force_delay must be >= 0")
	}
	if err := validRequestURL(c.PersonalKeys.RequestURL); err != nil {
		return fmt.Errorf("personal_keys.request_url %q: %v", c.PersonalKeys.RequestURL, err)
	}
	if err := validatePersonalKeys(c.PersonalKeys); err != nil {
		return err
	}
	if err := checkTimings("timings", c.Timings.overrides()); err != nil {
		return err
	}
	hostClass := map[string]string{}
	pduClass := map[string]string{}
	for _, name := range sortedClassNames(c.Classes) {
		cl := c.Classes[name]
		if err := checkTimings("classes."+name+".timings", cl.Timings); err != nil {
			return err
		}
		for _, h := range cl.Hosts {
			if prev, ok := hostClass[h]; ok {
				return fmt.Errorf("host %s is in two classes: %s and %s", h, prev, name)
			}
			hostClass[h] = name
		}
		for _, p := range cl.Pdus {
			if prev, ok := pduClass[p]; ok {
				return fmt.Errorf("pdu %s is in two classes: %s and %s", p, prev, name)
			}
			pduClass[p] = name
		}
	}
	return nil
}

// validatePersonalKeys checks the key-list settings (SR-2) and returns the first
// error, in this order: a malformed url; with no url, each sub-key that is set;
// with a url (malformed or not), cache_file, refresh, refresh_on_miss.
func validatePersonalKeys(p PersonalKeys) error {
	if p.URL != "" && !validListURL(p.URL) {
		shown := "xxxxx"
		if u, err := url.Parse(p.URL); err == nil {
			shown = u.Redacted()
		}
		return fmt.Errorf("personal_keys.url %q: want an absolute http(s)://host/... URL (or \"\" to turn the key list off)", shown)
	}
	if p.URL == "" {
		for _, k := range []struct {
			key string
			set bool
		}{
			{"token_file", p.setTokenFile()}, {"cache_file", p.setCacheFile()},
			{"refresh", p.setRefresh()}, {"refresh_on_miss", p.setRefreshOnMiss()},
			{"allow_insecure_http", p.setAllowInsecureHTTP()},
		} {
			if k.set {
				return fmt.Errorf("personal_keys.%s is set but personal_keys.url is empty", k.key)
			}
		}
		return nil
	}
	if p.CacheFile == "" {
		return errors.New("personal_keys.cache_file: required when personal_keys.url is set")
	}
	if p.Refresh <= 0 {
		return errors.New("personal_keys.refresh: must be greater than zero")
	}
	if p.RefreshOnMiss < 0 {
		return errors.New("personal_keys.refresh_on_miss: must not be negative")
	}
	return nil
}

// validListURL: an absolute http(s) URL with a host, no user information, no fragment.
func validListURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if sc := u.Scheme; sc != "http" && sc != "https" {
		return false
	}
	return u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.Contains(s, "#")
}

func sortedClassNames(m map[string]Class) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (t Timings) overrides() ClassTimings {
	p := func(d Duration) *Duration { return &d }
	return ClassTimings{
		PduOutletWait: p(t.PduOutletWait), IpmiPingWait: p(t.IpmiPingWait), ChassisOnWait: p(t.ChassisOnWait),
		PingSSHWait: p(t.PingSSHWait), SSHShutdownWait: p(t.SSHShutdownWait), PostSSHOffWait: p(t.PostSSHOffWait),
		IpmiOffCommandWait: p(t.IpmiOffCommandWait), FinalOffWait: p(t.FinalOffWait),
		RebootChassisOffWait: p(t.RebootChassisOffWait), RebootPduSettle: p(t.RebootPduSettle),
		DeadlineSlack: p(t.DeadlineSlack),
	}
}

func checkTimings(prefix string, t ClassTimings) error {
	fields := []struct {
		name   string
		v      *Duration
		zeroOK bool
	}{
		{"pdu_outlet_wait", t.PduOutletWait, false},
		{"ipmi_ping_wait", t.IpmiPingWait, false},
		{"chassis_on_wait", t.ChassisOnWait, false},
		{"ping_ssh_wait", t.PingSSHWait, false},
		{"ssh_shutdown_wait", t.SSHShutdownWait, false},
		{"post_ssh_off_wait", t.PostSSHOffWait, false},
		{"ipmi_off_command_wait", t.IpmiOffCommandWait, false},
		{"final_off_wait", t.FinalOffWait, false},
		{"reboot_chassis_off_wait", t.RebootChassisOffWait, false},
		{"reboot_pdu_settle", t.RebootPduSettle, true},
		{"deadline_slack", t.DeadlineSlack, true},
	}
	for _, f := range fields {
		if f.v == nil {
			continue
		}
		if *f.v < 0 || (*f.v == 0 && !f.zeroOK) {
			return fmt.Errorf("%s.%s must be > 0", prefix, f.name)
		}
	}
	return nil
}

// ResolvedTimings are one host's effective stage times.
type ResolvedTimings struct {
	PduOutletWait        time.Duration // host-level value; per outlet see HostPlan.Outlets
	IpmiPingWait         time.Duration
	ChassisOnWait        time.Duration
	PingSSHWait          time.Duration
	SSHShutdownWait      time.Duration
	PostSSHOffWait       time.Duration
	IpmiOffCommandWait   time.Duration
	FinalOffWait         time.Duration
	RebootChassisOffWait time.Duration
	RebootPduSettle      time.Duration
	DeadlineSlack        time.Duration
}

func (r *ResolvedTimings) apply(t ClassTimings) {
	set := func(dst *time.Duration, v *Duration) {
		if v != nil {
			*dst = v.D()
		}
	}
	set(&r.PduOutletWait, t.PduOutletWait)
	set(&r.IpmiPingWait, t.IpmiPingWait)
	set(&r.ChassisOnWait, t.ChassisOnWait)
	set(&r.PingSSHWait, t.PingSSHWait)
	set(&r.SSHShutdownWait, t.SSHShutdownWait)
	set(&r.PostSSHOffWait, t.PostSSHOffWait)
	set(&r.IpmiOffCommandWait, t.IpmiOffCommandWait)
	set(&r.FinalOffWait, t.FinalOffWait)
	set(&r.RebootChassisOffWait, t.RebootChassisOffWait)
	set(&r.RebootPduSettle, t.RebootPduSettle)
	set(&r.DeadlineSlack, t.DeadlineSlack)
}

// OutletPlan is one outlet with its resolved wait.
type OutletPlan struct {
	Pdu    string
	Outlet int
	Wait   time.Duration
}

// Budgets are the per-action budgets (sums of stage times).
type Budgets struct {
	Powerup   time.Duration
	Powerdown time.Duration
	Powercut  time.Duration // powerdown plus one more cut of every outlet
	Reboot    time.Duration
}

// Strings renders budgets as {"powerup": "930s", ...} for the API.
func (b Budgets) Strings() map[string]string {
	f := func(d time.Duration) string { return fmt.Sprintf("%ds", int64(d/time.Second)) }
	return map[string]string{ActionPowerup: f(b.Powerup), ActionPowerdown: f(b.Powerdown), ActionPowercut: f(b.Powercut), ActionReboot: f(b.Reboot)}
}

// For returns the budget for an action.
func (b Budgets) For(action string) time.Duration {
	switch action {
	case ActionPowerup:
		return b.Powerup
	case ActionPowerdown:
		return b.Powerdown
	case ActionPowercut:
		return b.Powercut
	case ActionReboot:
		return b.Reboot
	}
	return 0
}

// HostPlan is everything resolved for one host.
type HostPlan struct {
	Class   string
	Timings ResolvedTimings
	Outlets []OutletPlan
	Budgets Budgets
	Slack   time.Duration
}

// Deadline is the recorded command deadline offset: budget plus slack.
func (p HostPlan) Deadline(action string) time.Duration { return p.Budgets.For(action) + p.Slack }

// Resolve applies classes to every inventory host and computes budgets. A
// class naming a host or PDU that is not in inventory is an error.
func Resolve(cfg Config, inv *inventory.Inventory) (map[string]HostPlan, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	hostClass := map[string]string{}
	pduClass := map[string]string{}
	for _, name := range sortedClassNames(cfg.Classes) {
		cl := cfg.Classes[name]
		for _, h := range cl.Hosts {
			if _, ok := inv.Host(h); !ok {
				return nil, fmt.Errorf("class %s names host %s, which is not in inventory", name, h)
			}
			hostClass[h] = name
		}
		for _, p := range cl.Pdus {
			if _, ok := inv.Pdus[p]; !ok {
				return nil, fmt.Errorf("class %s names pdu %s, which is not in inventory", name, p)
			}
			pduClass[p] = name
		}
	}
	// Every host needs PDU outlets: cold_off, powercut and the seed rule
	// depend on them.
	var noOutlets []error
	for _, h := range inv.Hosts {
		if len(h.Outlets()) == 0 {
			noOutlets = append(noOutlets, fmt.Errorf("inventory host %s has no pdu outlets", h.Hostname))
		}
	}
	if len(noOutlets) > 0 {
		return nil, errors.Join(noOutlets...)
	}
	plans := map[string]HostPlan{}
	for _, h := range inv.Hosts {
		var rt ResolvedTimings
		rt.apply(cfg.Timings.overrides())
		cls := hostClass[h.Hostname]
		if cls != "" {
			rt.apply(cfg.Classes[cls].Timings)
		}
		plan := HostPlan{Class: cls, Timings: rt, Slack: rt.DeadlineSlack}
		var outletSum time.Duration
		for _, ref := range h.Outlets() {
			wait := rt.PduOutletWait
			if pc, ok := pduClass[ref.Name]; ok {
				if v := cfg.Classes[pc].Timings.PduOutletWait; v != nil {
					wait = v.D()
				}
			}
			plan.Outlets = append(plan.Outlets, OutletPlan{Pdu: ref.Name, Outlet: ref.Outlet, Wait: wait})
			outletSum += wait
		}
		plan.Budgets.Powerup = outletSum + rt.IpmiPingWait + rt.ChassisOnWait + rt.PingSSHWait
		plan.Budgets.Powerdown = rt.SSHShutdownWait + rt.PostSSHOffWait + rt.IpmiOffCommandWait + outletSum + rt.FinalOffWait
		plan.Budgets.Powercut = plan.Budgets.Powerdown + outletSum
		plan.Budgets.Reboot = rt.IpmiOffCommandWait + rt.RebootChassisOffWait + rt.ChassisOnWait + rt.PingSSHWait
		// Room for the PDU escalation (outlets off, settle, outlets on),
		// so the budget never cuts a legitimate escalated reboot short.
		plan.Budgets.Reboot += rt.RebootPduSettle + 2*outletSum
		plans[h.Hostname] = plan
	}
	return plans, nil
}

// InventoryLoader loads the inventory for a config (Vault in production).
type InventoryLoader func(ctx context.Context, cfg Config) (*inventory.Inventory, error)

// Check loads the config at path and the inventory, runs Resolve and returns
// the number of inventory hosts. It is the `powermaster check` command.
func Check(ctx context.Context, path string, load InventoryLoader) (int, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return 0, err
	}
	if err := cfg.ValidateInventory(); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	inv, err := load(ctx, cfg)
	if err != nil {
		return 0, fmt.Errorf("inventory: %w", err)
	}
	if _, err := Resolve(cfg, inv); err != nil {
		return 0, err
	}
	return len(inv.Hosts), nil
}
