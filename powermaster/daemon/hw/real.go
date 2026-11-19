package hw

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/bougou/go-ipmi"

	"github.com/resurgentech/powerwarden/powermaster/daemon/hw/eatonpdu"
	"github.com/resurgentech/powerwarden/powermaster/daemon/hw/kasapdu"
	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
)

// Real is the production Provider.
type Real struct {
	Log *slog.Logger
	// LogLevel >= 4 enables raw PDU output, >= 5 go-ipmi debug (as powerwarden).
	LogLevel int
	// Timeout bounds one network probe (ping, SSH connect). Default 5s.
	Timeout time.Duration
}

func (r Real) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r Real) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 5 * time.Second
}

// IPMI returns a BMC client for h. Calls are serialized per host.
func (r Real) IPMI(h inventory.Host) IPMI {
	return &realIPMI{cfg: h.Ipmi, host: h.Hostname, log: r.logger(), debug: r.LogLevel >= 5}
}

// Net returns a network client for h.
func (r Real) Net(h inventory.Host) Net {
	return &realNet{host: h, log: r.logger(), timeout: r.timeout()}
}

// PDU returns a client for p by its pdustyle.
func (r Real) PDU(p inventory.Pdu) (PDU, error) {
	eatonpdu.SetLogger(r.logger())
	kasapdu.SetLogger(r.logger())
	switch p.PduStyle {
	case "eaton_ePDU_G3_Managed", "eaton_ePDU_G3_Managed_proxied":
		return &eatonPDU{cfg: eatonpdu.Config{Host: p.IP, Username: p.Username, Password: p.Password,
			LogLevel: r.LogLevel, Proxied: p.PduStyle == "eaton_ePDU_G3_Managed_proxied"}}, nil
	case "kasa_hs300":
		return &kasaPDU{cfg: kasapdu.Config{Host: p.IP, LogLevel: r.LogLevel}}, nil
	}
	return nil, fmt.Errorf("pdu %s: unsupported pdustyle %q", p.Name, p.PduStyle)
}

// --- IPMI (adapted from powerwarden ipmi.go) ---

type realIPMI struct {
	mu    sync.Mutex
	cfg   inventory.Ipmi
	host  string
	log   *slog.Logger
	debug bool
}

func (i *realIPMI) connect(ctx context.Context) (*ipmi.Client, error) {
	port := i.cfg.Port
	if port == 0 {
		port = 623
	}
	client, err := ipmi.NewClient(i.cfg.IP, port, i.cfg.Username, i.cfg.Password)
	if err != nil {
		return nil, fmt.Errorf("ipmi client for %s: %v", i.host, err)
	}
	client.WithDebug(i.debug)
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("ipmi connect %s: %v", i.host, err)
	}
	return client, nil
}

func (i *realIPMI) ChassisPower(ctx context.Context) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	client, err := i.connect(ctx)
	if err != nil {
		return false, err
	}
	defer client.Close(context.Background())
	st, err := client.GetChassisStatus(ctx)
	if err != nil {
		return false, fmt.Errorf("chassis status %s: %v", i.host, err)
	}
	return st.PowerIsOn, nil
}

func (i *realIPMI) SetChassisPower(ctx context.Context, on bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	client, err := i.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Close(context.Background())
	ctl := ipmi.ChassisControlPowerDown
	if on {
		ctl = ipmi.ChassisControlPowerUp
	}
	if _, err := client.ChassisControl(ctx, ctl); err != nil {
		return fmt.Errorf("chassis control %s on=%v: %v", i.host, on, err)
	}
	i.log.Info("ipmi chassis command sent", "hostname", i.host, "on", on)
	return nil
}

// --- PDUs (adapted from powerwarden pdu.go) ---

type eatonPDU struct {
	mu  sync.Mutex
	cfg eatonpdu.Config
}

func (e *eatonPDU) OutletStates(ctx context.Context) (map[int]bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	outlets, err := eatonpdu.GetOutlets(e.cfg)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for i, s := range outlets.Status {
		out[i] = s == "on"
	}
	return out, nil
}

func (e *eatonPDU) SetOutlet(ctx context.Context, outlet int, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return eatonpdu.SetOutlet(e.cfg, outlet+1, onOff(on)) // Eaton is 1-based
}

type kasaPDU struct {
	mu  sync.Mutex
	cfg kasapdu.Config
}

func (k *kasaPDU) OutletStates(ctx context.Context) (map[int]bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	outlets, err := kasapdu.GetOutlets(k.cfg)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for i, s := range outlets {
		out[i] = s == "on"
	}
	return out, nil
}

func (k *kasaPDU) SetOutlet(ctx context.Context, outlet int, on bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return kasapdu.SetOutlet(k.cfg, outlet, onOff(on)) // Kasa is 0-based
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// --- Network (adapted from powerwarden network.go) ---

type realNet struct {
	host    inventory.Host
	log     *slog.Logger
	timeout time.Duration

	keyOnce sync.Once
	keyPath string
}

func ping(ctx context.Context, addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	return exec.CommandContext(ctx, "ping", "-c", "1", "-W", strconv.Itoa(secs), addr).Run() == nil
}

func (n *realNet) PingHost(ctx context.Context) bool { return ping(ctx, n.host.IP, n.timeout) }
func (n *realNet) PingBMC(ctx context.Context) bool  { return ping(ctx, n.host.Ipmi.IP, n.timeout) }

// sshKey writes the host's private key to a 0600 temp file once.
func (n *realNet) sshKey() string {
	n.keyOnce.Do(func() {
		if n.host.SSHKey.PrivateKey == "" {
			return
		}
		key, err := base64.StdEncoding.DecodeString(n.host.SSHKey.PrivateKey)
		if err != nil {
			n.log.Warn("ssh key is not base64", "hostname", n.host.Hostname)
			return
		}
		f, err := os.CreateTemp("", "powermaster-ssh-key-*")
		if err != nil {
			n.log.Warn("ssh key temp file", "hostname", n.host.Hostname, "error", err)
			return
		}
		defer f.Close()
		if err := f.Chmod(0o600); err != nil {
			os.Remove(f.Name())
			return
		}
		if _, err := f.Write(key); err != nil {
			os.Remove(f.Name())
			return
		}
		n.keyPath = f.Name()
	})
	return n.keyPath
}

func (n *realNet) ssh(ctx context.Context, command string) error {
	ctx, cancel := context.WithTimeout(ctx, n.timeout+2*time.Second)
	defer cancel()
	args := []string{
		"-o", "ConnectTimeout=" + strconv.Itoa(int(n.timeout/time.Second)),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
	}
	if k := n.sshKey(); k != "" {
		args = append(args, "-i", k)
	}
	args = append(args, n.host.User+"@"+n.host.IP, command)
	return exec.CommandContext(ctx, "ssh", args...).Run()
}

func (n *realNet) SSH(ctx context.Context) bool { return n.ssh(ctx, "exit 0") == nil }

func (n *realNet) Shutdown(ctx context.Context) error {
	return n.ssh(ctx, "sudo shutdown -h now")
}
