// Package inventory loads powermaster's host and PDU inventory. The Vault
// parsing is copied from an earlier powerwarden config.go (readVault/ParseVault),
// Inventory is read once at start.
package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hashicorp/vault-client-go"
)

// SSHKey is a host's base64-encoded SSH key pair.
type SSHKey struct {
	PrivateKey string `json:"private" yaml:"private"`
	PublicKey  string `json:"public" yaml:"public"`
}

// Ipmi holds a host's BMC address and credentials.
type Ipmi struct {
	URL      string `json:"url" yaml:"url"`
	Username string `json:"username" yaml:"username"`
	Password string `json:"password" yaml:"password"`
	IP       string `json:"ip" yaml:"ip"`
	Port     int    `json:"port" yaml:"port"`
}

// PduRef is one PDU outlet feeding a host.
type PduRef struct {
	Name   string `json:"name" yaml:"name"`
	Outlet int    `json:"outlet" yaml:"outlet"`
}

// Host is one managed host. It holds secrets and must never be serialized
// into API responses.
type Host struct {
	Hostname string   `json:"hostname" yaml:"hostname"`
	IP       string   `json:"ip" yaml:"ip"`
	Ipmi     Ipmi     `json:"ipmi" yaml:"ipmi"`
	Pdu      []PduRef `json:"pdu" yaml:"pdu"`
	User     string   `json:"user" yaml:"user"`
	SSHKey   SSHKey   `json:"sshkey" yaml:"sshkey"`
	Locked   bool     `json:"locked" yaml:"locked"`
}

// Outlets returns the host's PDU outlet references, skipping blank names.
func (h Host) Outlets() []PduRef {
	var out []PduRef
	for _, r := range h.Pdu {
		if r.Name != "" {
			out = append(out, r)
		}
	}
	return out
}

// Pdu is one PDU. Name is the map key in Vault.
type Pdu struct {
	Name     string `json:"-" yaml:"-"`
	IP       string `json:"ip" yaml:"ip"`
	Username string `json:"username" yaml:"username"`
	Password string `json:"password" yaml:"password"`
	PduStyle string `json:"pdustyle" yaml:"pdustyle"`
	URL      string `json:"url" yaml:"url"`
}

// Inventory is the set of hosts and PDUs.
type Inventory struct {
	Hosts []Host
	Pdus  map[string]Pdu
}

// Host looks a host up by name.
func (inv *Inventory) Host(name string) (Host, bool) {
	for _, h := range inv.Hosts {
		if h.Hostname == name {
			return h, true
		}
	}
	return Host{}, false
}

// Validate checks required fields, duplicates and PDU references, and fills
// PDU names from map keys.
func (inv *Inventory) Validate() error {
	if inv.Pdus == nil {
		inv.Pdus = map[string]Pdu{}
	}
	for name, p := range inv.Pdus {
		p.Name = name
		switch {
		case p.IP == "":
			return fmt.Errorf("invalid pdu %s: ip required", name)
		case p.Username == "":
			return fmt.Errorf("invalid pdu %s: username required", name)
		case p.Password == "":
			return fmt.Errorf("invalid pdu %s: password required", name)
		case p.PduStyle == "":
			return fmt.Errorf("invalid pdu %s: pdustyle required", name)
		}
		inv.Pdus[name] = p
	}
	seen := map[string]bool{}
	for i := range inv.Hosts {
		h := &inv.Hosts[i]
		if h.Hostname == "" {
			return fmt.Errorf("invalid host #%d: hostname required", i)
		}
		if seen[h.Hostname] {
			return fmt.Errorf("duplicate host %s", h.Hostname)
		}
		seen[h.Hostname] = true
		if h.Ipmi.Username == "" || h.Ipmi.Password == "" {
			return fmt.Errorf("invalid host %s: ipmi username and password required", h.Hostname)
		}
		for _, r := range h.Outlets() {
			if _, ok := inv.Pdus[r.Name]; !ok {
				return fmt.Errorf("host %s references unknown pdu %s", h.Hostname, r.Name)
			}
		}
	}
	return nil
}

// Source loads an inventory. Tests use Static; the daemon uses Vault.
type Source interface {
	Load(ctx context.Context) (*Inventory, error)
}

// Static is a fixed inventory.
type Static struct{ Inventory Inventory }

// Load validates and returns a copy of the static inventory.
func (s Static) Load(context.Context) (*Inventory, error) {
	inv := Inventory{Hosts: append([]Host(nil), s.Inventory.Hosts...), Pdus: map[string]Pdu{}}
	for k, v := range s.Inventory.Pdus {
		inv.Pdus[k] = v
	}
	if err := inv.Validate(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// ParseSecret parses the data of the Vault hosts secret ({"hosts": [...],
// "pdus": {...}}).
func ParseSecret(data map[string]any) (*Inventory, error) {
	hostsRaw, ok := data["hosts"]
	if !ok {
		return nil, fmt.Errorf("no 'hosts' key in vault secret")
	}
	pdusRaw, ok := data["pdus"]
	if !ok {
		pdusRaw = map[string]any{}
	}
	hostsBytes, err := json.Marshal(hostsRaw)
	if err != nil {
		return nil, fmt.Errorf("marshal hosts failed: %v", err)
	}
	pdusBytes, err := json.Marshal(pdusRaw)
	if err != nil {
		return nil, fmt.Errorf("marshal pdus failed: %v", err)
	}
	var inv Inventory
	if err := json.Unmarshal(hostsBytes, &inv.Hosts); err != nil {
		return nil, fmt.Errorf("unmarshal hosts failed: %v", err)
	}
	if err := json.Unmarshal(pdusBytes, &inv.Pdus); err != nil {
		return nil, fmt.Errorf("unmarshal pdus failed: %v", err)
	}
	if err := inv.Validate(); err != nil {
		return nil, fmt.Errorf("invalid vault inventory: %w", err)
	}
	return &inv, nil
}

// DefaultKVMount is the Vault KV v2 mount holding the hosts secret (today's
// POWERWARDEN_VAULT_KV_MOUNT default).
const DefaultKVMount = "kv"

// Vault reads the inventory from a KV v2 secret. Address, token and
// skip-verify come from the named environment variables.
type Vault struct {
	AddrEnv       string
	TokenEnv      string
	SkipVerifyEnv string
	SecretPath    string
	Mount         string // default DefaultKVMount
	Retries       int    // default 3
	Log           *slog.Logger
}

// Load reads and parses the secret, retrying transient failures.
func (v Vault) Load(ctx context.Context) (*Inventory, error) {
	logger := v.Log
	if logger == nil {
		logger = slog.Default()
	}
	if v.SecretPath == "" {
		return nil, fmt.Errorf("vault hosts_secret_path required")
	}
	addr := os.Getenv(v.AddrEnv)
	if addr == "" {
		return nil, fmt.Errorf("%s required", v.AddrEnv)
	}
	token := os.Getenv(v.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("%s required", v.TokenEnv)
	}
	skipVerify := v.SkipVerifyEnv != "" && os.Getenv(v.SkipVerifyEnv) == "true"
	client, err := vault.New(
		vault.WithAddress(addr),
		vault.WithRequestTimeout(30*time.Second),
		vault.WithTLS(vault.TLSConfiguration{InsecureSkipVerify: skipVerify}),
	)
	if err != nil {
		return nil, fmt.Errorf("vault client setup failed: %v", err)
	}
	if err := client.SetToken(token); err != nil {
		return nil, fmt.Errorf("vault token set failed: %v", err)
	}
	mount := v.Mount
	if mount == "" {
		mount = DefaultKVMount
	}
	retries := v.Retries
	if retries <= 0 {
		retries = 3
	}
	var inv *Inventory
	for i := 0; i < retries; i++ {
		inv, err = readVault(ctx, client, v.SecretPath, mount)
		if err == nil {
			return inv, nil
		}
		logger.Warn("vault fetch retry failed", "attempt", i+1, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("vault fetch failed after retries: %v", err)
}

func readVault(ctx context.Context, client *vault.Client, secret, mount string) (*Inventory, error) {
	resp, err := client.Secrets.KvV2Read(ctx, secret, vault.WithMountPath(mount))
	if err != nil {
		return nil, fmt.Errorf("vault read failed (mount=%s, secret=%s): %v", mount, secret, err)
	}
	if resp == nil || resp.Data.Data == nil {
		return nil, fmt.Errorf("vault secret not found (mount=%s, secret=%s)", mount, secret)
	}
	return ParseSecret(resp.Data.Data)
}
