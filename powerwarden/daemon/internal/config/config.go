// Package config loads powerwarden's config.yaml. Every key is documented,
// with its default, in config.yaml.example. Unknown keys are an error.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen      string      `yaml:"listen"`
	StatePath   string      `yaml:"state_path"`
	TokensFile  string      `yaml:"tokens_file"`
	LogLevel    string      `yaml:"log_level"`
	Powermaster Powermaster `yaml:"powermaster"`
	Timings     Timings     `yaml:"timings"`
	Limits      Limits      `yaml:"limits"`
	Events      Events      `yaml:"events"`
	UI          UI          `yaml:"ui"`
	// HostTags seeds host capability tags into the database for hosts that
	// have none stored. The database value wins once set.
	HostTags map[string][]string `yaml:"host_tags"`
}

type Powermaster struct {
	URL            string        `yaml:"url"`
	TokenFile      string        `yaml:"token_file"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

type Timings struct {
	ReconcileInterval  time.Duration `yaml:"reconcile_interval"`
	PowerOnSpacing     time.Duration `yaml:"power_on_spacing"`
	HostViewRefresh    time.Duration `yaml:"host_view_refresh"`
	BootGrace          time.Duration `yaml:"boot_grace"`
	BootCooldown       time.Duration `yaml:"boot_cooldown"`
	MinIdleTime        time.Duration `yaml:"min_idle_time"`
	BusyGrace          time.Duration `yaml:"busy_grace"`
	DemandLeaseDefault time.Duration `yaml:"demand_lease_default"`
	DemandLeaseMin     time.Duration `yaml:"demand_lease_min"`
	DemandLeaseMax     time.Duration `yaml:"demand_lease_max"`
	WorkerLeaseDefault time.Duration `yaml:"worker_lease_default"`
	WorkerLeaseMin     time.Duration `yaml:"worker_lease_min"`
	WorkerLeaseMax     time.Duration `yaml:"worker_lease_max"`
	DemandSweepAfter   time.Duration `yaml:"demand_sweep_after"`
	WorkerSweepExtra   time.Duration `yaml:"worker_sweep_extra"`
	// ColdOffAfter is how long a host sits in soft_off before powerwarden
	// asks for a powercut. 0 disables powercut.
	ColdOffAfter time.Duration `yaml:"cold_off_after"`
	// Claims. The lease is the liveness window renewed by re-PUT; the cap is
	// the hard limit moved only by extend. Requests outside min..max are clamped.
	ClaimLeaseDefault time.Duration `yaml:"claim_lease_default"`
	ClaimLeaseMin     time.Duration `yaml:"claim_lease_min"`
	ClaimLeaseMax     time.Duration `yaml:"claim_lease_max"`
	ClaimCapDefault   time.Duration `yaml:"claim_cap_default"`
	ClaimCapMin       time.Duration `yaml:"claim_cap_min"`
	ClaimCapMax       time.Duration `yaml:"claim_cap_max"`
	ClaimRestartGrace time.Duration `yaml:"claim_restart_grace"` // used from PWC8
	ClaimWaitMax      time.Duration `yaml:"claim_wait_max"`      // used from PWC8b
	ClaimSweepAfter   time.Duration `yaml:"claim_sweep_after"`
}

type Limits struct {
	MaxDemands    int `yaml:"max_demands"`
	MaxClaims     int `yaml:"max_claims"`
	MaxTags       int `yaml:"max_tags"`
	DecisionsKept int `yaml:"decisions_kept"`
}

type Events struct {
	RingSize  int           `yaml:"ring_size"`
	Heartbeat time.Duration `yaml:"heartbeat"`
}

type UI struct {
	Enabled      bool   `yaml:"enabled"`
	HtpasswdFile string `yaml:"htpasswd_file"`
	// PathPrefix serves the UI under <prefix>/ui for a reverse proxy that
	// neither strips nor rewrites the prefix. Empty serves at /ui.
	PathPrefix string `yaml:"path_prefix"`
}

// Default returns the documented defaults.
func Default() Config {
	return Config{
		Listen:     ":8091",
		StatePath:  "/var/lib/powerwarden/state.db",
		TokensFile: "/etc/powerwarden/tokens.yaml",
		LogLevel:   "info",
		Powermaster: Powermaster{
			URL:            "", // required: Validate rejects it
			TokenFile:      "/etc/powerwarden/powermaster.token",
			RequestTimeout: 10 * time.Second,
		},
		Timings: Timings{
			ReconcileInterval:  15 * time.Second,
			PowerOnSpacing:     15 * time.Second,
			HostViewRefresh:    60 * time.Second,
			BootGrace:          5 * time.Minute,
			BootCooldown:       10 * time.Minute,
			MinIdleTime:        10 * time.Minute,
			BusyGrace:          7200 * time.Second,
			DemandLeaseDefault: 3600 * time.Second,
			DemandLeaseMin:     30 * time.Second,
			DemandLeaseMax:     3600 * time.Second,
			WorkerLeaseDefault: 90 * time.Second,
			WorkerLeaseMin:     15 * time.Second,
			WorkerLeaseMax:     300 * time.Second,
			DemandSweepAfter:   24 * time.Hour,
			WorkerSweepExtra:   24 * time.Hour,
			ColdOffAfter:       time.Hour,
			ClaimLeaseDefault:  15 * time.Minute,
			ClaimLeaseMin:      time.Minute,
			ClaimLeaseMax:      time.Hour,
			ClaimCapDefault:    8 * time.Hour,
			ClaimCapMin:        15 * time.Minute,
			ClaimCapMax:        72 * time.Hour,
			ClaimRestartGrace:  5 * time.Minute,
			ClaimWaitMax:       time.Minute,
			ClaimSweepAfter:    24 * time.Hour,
		},
		Limits: Limits{MaxDemands: 500, MaxClaims: 64, MaxTags: 32, DecisionsKept: 500},
		Events: Events{RingSize: 1000, Heartbeat: 15 * time.Second},
		UI:     UI{Enabled: true, HtpasswdFile: "/etc/powerwarden/ui.htpasswd"},
	}
}

// Load reads path; keys absent from the file keep their defaults.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return Parse(b)
}

// removedKeys are top-level keys from the Redis demand source era (before
// hourchd). They are rejected with an explanation rather than the generic
// unknown-key error, so an old config fails clearly at startup.
var removedKeys = map[string]string{
	"demand_source": "powerwarden is HTTP-only since the hourchd cutover; demand and liveness come from hourchd over the API",
	"redis":         "the Redis demand source was removed at the hourchd cutover",
}

// Parse decodes YAML onto the defaults, rejecting unknown keys, then validates.
func Parse(b []byte) (Config, error) {
	var top map[string]any
	if yaml.Unmarshal(b, &top) == nil {
		var errs []error
		for _, k := range []string{"demand_source", "redis"} {
			if _, ok := top[k]; ok {
				errs = append(errs, fmt.Errorf("key %q was removed: %s; delete it", k, removedKeys[k]))
			}
		}
		if len(errs) > 0 {
			return Config{}, fmt.Errorf("config: %w", errors.Join(errs...))
		}
	}
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks values that would make the daemon misbehave.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Listen == "" {
		bad("listen is required")
	}
	if c.StatePath == "" {
		bad("state_path is required")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("log_level %q: want debug, info, warn or error", c.LogLevel)
	}
	if c.Powermaster.URL == "" {
		bad("powermaster.url is required")
	}
	if c.Powermaster.RequestTimeout <= 0 {
		bad("powermaster.request_timeout must be > 0")
	}

	t := c.Timings
	for name, d := range map[string]time.Duration{
		"reconcile_interval": t.ReconcileInterval, "power_on_spacing": t.PowerOnSpacing,
		"host_view_refresh": t.HostViewRefresh, "boot_grace": t.BootGrace,
		"boot_cooldown": t.BootCooldown, "min_idle_time": t.MinIdleTime, "busy_grace": t.BusyGrace,
		"demand_lease_default": t.DemandLeaseDefault, "demand_lease_min": t.DemandLeaseMin,
		"demand_lease_max": t.DemandLeaseMax, "worker_lease_default": t.WorkerLeaseDefault,
		"worker_lease_min": t.WorkerLeaseMin, "worker_lease_max": t.WorkerLeaseMax,
		"demand_sweep_after": t.DemandSweepAfter, "worker_sweep_extra": t.WorkerSweepExtra,
		"claim_lease_default": t.ClaimLeaseDefault, "claim_lease_min": t.ClaimLeaseMin,
		"claim_lease_max": t.ClaimLeaseMax, "claim_cap_default": t.ClaimCapDefault,
		"claim_cap_min": t.ClaimCapMin, "claim_cap_max": t.ClaimCapMax,
		"claim_restart_grace": t.ClaimRestartGrace, "claim_wait_max": t.ClaimWaitMax,
		"claim_sweep_after": t.ClaimSweepAfter,
	} {
		if d <= 0 {
			bad("timings.%s must be > 0", name)
		}
	}
	if t.ColdOffAfter < 0 {
		bad("timings.cold_off_after must be >= 0 (0 disables powercut)")
	}
	checkLease := func(kind string, min, def, max time.Duration) {
		if min > def || def > max {
			bad("timings.%s_lease_min <= %s_lease_default <= %s_lease_max violated (%s, %s, %s)", kind, kind, kind, min, def, max)
		}
		for _, d := range []time.Duration{min, def, max} {
			if d%time.Second != 0 {
				bad("timings.%s_lease_* must be whole seconds", kind)
				break
			}
		}
	}
	checkLease("demand", t.DemandLeaseMin, t.DemandLeaseDefault, t.DemandLeaseMax)
	checkLease("worker", t.WorkerLeaseMin, t.WorkerLeaseDefault, t.WorkerLeaseMax)
	checkLease("claim", t.ClaimLeaseMin, t.ClaimLeaseDefault, t.ClaimLeaseMax)
	if t.ClaimCapMin > t.ClaimCapDefault || t.ClaimCapDefault > t.ClaimCapMax {
		bad("timings.claim_cap_min <= claim_cap_default <= claim_cap_max violated (%s, %s, %s)", t.ClaimCapMin, t.ClaimCapDefault, t.ClaimCapMax)
	}
	for _, d := range []time.Duration{t.ClaimCapMin, t.ClaimCapDefault, t.ClaimCapMax} {
		if d%time.Second != 0 {
			bad("timings.claim_cap_* must be whole seconds")
			break
		}
	}

	if c.Limits.MaxDemands <= 0 || c.Limits.MaxClaims <= 0 || c.Limits.MaxTags <= 0 || c.Limits.DecisionsKept <= 0 {
		bad("limits.* must be > 0")
	}
	if c.Events.RingSize <= 0 || c.Events.Heartbeat <= 0 {
		bad("events.ring_size and events.heartbeat must be > 0")
	}
	if c.UI.Enabled && c.UI.HtpasswdFile == "" {
		bad("ui.htpasswd_file is required when ui.enabled")
	}
	if err := validPathPrefix(c.UI.PathPrefix); err != nil {
		bad("ui.path_prefix %q: %v", c.UI.PathPrefix, err)
	}
	// Name tags (a host's full and short name) are reserved: no host_tags
	// value may equal any host_tags key or its short form.
	reserved := map[string]bool{}
	for h := range c.HostTags {
		reserved[h] = true
		if i := strings.IndexByte(h, '.'); i >= 0 {
			reserved[h[:i]] = true
		}
	}
	for _, h := range sortedKeys(c.HostTags) {
		for _, t := range c.HostTags[h] {
			if reserved[t] {
				bad("host_tags.%s: %q is reserved: it is the name of a host in host_tags", h, t)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var prefixSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// validPathPrefix accepts "" or one or more "/segment"s, each made of
// URL-unreserved characters (so it is safe in a ServeMux pattern and needs no
// escaping), with no trailing "/", no empty segment and no "..".
func validPathPrefix(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("must start with /")
	}
	if strings.HasSuffix(p, "/") {
		return errors.New("must not end with /")
	}
	if strings.Contains(p, "..") {
		return errors.New(`must not contain ".."`)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || !prefixSegment.MatchString(seg) {
			return errors.New("each segment must be non-empty and use only letters, digits, . _ ~ -")
		}
	}
	return nil
}
