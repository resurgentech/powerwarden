package eatonpdu

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var log = slog.Default()

// Config holds PDU connection details
type Config struct {
	Host     string
	Username string
	Password string
	LogLevel int
	Proxied  bool
}

// Outlets represents the parsed data for all outlets
type Outlets struct {
	Status []string  `json:"status"`      // PresentStatus.SwitchOnOff
	Power  []float64 `json:"power (W)"`   // ActivePower (W)
	Energy []float64 `json:"energy (Wh)"` // Statistic[5].Energy (Wh)
	Name   []string  `json:"name"`        // iName
}

// cmdOutputWriter wraps a buffer to log output if debug is enabled
type cmdOutputWriter struct {
	buffer   *bytes.Buffer
	loglevel int
	prefix   string
}

// SetLogger sets the package-level logger.
func SetLogger(logger *slog.Logger) {
	if logger != nil {
		log = logger
	}
}

func (w *cmdOutputWriter) Write(p []byte) (n int, err error) {
	n, err = w.buffer.Write(p)
	if w.loglevel >= 4 {
		log.Debug(w.prefix+" RAW", "data", string(p))
	}
	return n, err
}

// RunSSHCommand executes an SSH command and returns the output
func RunSSHCommand(cfg Config, command string) (string, error) {
	sshArgs := []string{
		"-tt",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}
	if !cfg.Proxied {
		sshArgs = append(sshArgs,
			"-o", "PubkeyAcceptedKeyTypes=+ssh-dss",
			"-o", "HostKeyAlgorithms=+ssh-dss",
			"-o", "KexAlgorithms=+diffie-hellman-group1-sha1")
	}
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", cfg.Username, cfg.Host))

	cmdstr := append([]string{"-p", cfg.Password, "ssh"}, sshArgs...)
	log.Debug("Formatted SSH", "args", sshArgs) // never log cmdstr: it holds the password
	cmd := exec.Command("sshpass", cmdstr...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("failed to get stdin pipe: %v", err)
	}
	stdin, ok := stdinPipe.(*os.File)
	if !ok {
		return "", fmt.Errorf("stdin pipe is not an *os.File")
	}

	cmd.Stdout = &cmdOutputWriter{buffer: &bytes.Buffer{}, loglevel: cfg.LogLevel, prefix: "[stdout]"}
	cmd.Stderr = &cmdOutputWriter{buffer: &bytes.Buffer{}, loglevel: cfg.LogLevel, prefix: "[stderr]"}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start SSH: %v", err)
	}

	time.Sleep(3 * time.Second)

	log.Debug("Sending command", "command", command)
	_, err = stdin.Write([]byte(command + "\r\n"))
	if err != nil {
		return "", fmt.Errorf("failed to send command: %v", err)
	}

	time.Sleep(5 * time.Second)

	log.Debug("Sending command: quit")
	_, err = stdin.Write([]byte("quit\r\n"))
	if err != nil {
		log.Info("Failed to send quit", "error", err)
	}

	stdin.Close()
	err = cmd.Wait()
	output := cmd.Stdout.(*cmdOutputWriter).buffer.String() + cmd.Stderr.(*cmdOutputWriter).buffer.String()
	if err != nil {
		log.Debug("SSH session closed with error", "error", err)
	}
	return output, nil
}

// GetOutlets fetches and parses all outlet data into an Outlets struct
func GetOutlets(cfg Config) (Outlets, error) {
	output, err := RunSSHCommand(cfg, "get PDU.OutletSystem.Outlet[x].*")
	if err != nil {
		return Outlets{}, err
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	var outlets Outlets

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "PDU.OutletSystem.") {
			property := line
			i++
			if i >= len(lines) {
				return Outlets{}, fmt.Errorf("missing value for property %s", property)
			}
			valuesLine := strings.TrimSpace(lines[i])
			values := strings.Split(valuesLine, "|")

			switch property {
			case "PDU.OutletSystem.Outlet[x].PresentStatus.SwitchOnOff":
				outlets.Status = make([]string, len(values))
				for j, v := range values {
					outlets.Status[j] = map[bool]string{true: "on", false: "off"}[v == "1"]
				}
			case "PDU.OutletSystem.Outlet[x].ActivePower":
				outlets.Power = make([]float64, len(values))
				for j, v := range values {
					f, err := strconv.ParseFloat(v, 64)
					if err != nil {
						return Outlets{}, fmt.Errorf("failed to parse power value %s: %v", v, err)
					}
					outlets.Power[j] = f
				}
			case "PDU.OutletSystem.Outlet[x].Statistic[5].Energy":
				outlets.Energy = make([]float64, len(values))
				for j, v := range values {
					f, err := strconv.ParseFloat(v, 64)
					if err != nil {
						return Outlets{}, fmt.Errorf("failed to parse energy value %s: %v", v, err)
					}
					outlets.Energy[j] = f
				}
			case "PDU.OutletSystem.Outlet[x].iName":
				outlets.Name = make([]string, len(values))
				for j, v := range values {
					outlets.Name[j] = v
				}
			}
		}
	}
	return outlets, nil
}

// SetOutlet toggles an outlet on or off
func SetOutlet(cfg Config, outlet int, state string) error {
	var cmd string
	switch state {
	case "on":
		cmd = fmt.Sprintf("set PDU.OutletSystem.Outlet[%d].DelayBeforeStartup 0", outlet)
	case "off":
		cmd = fmt.Sprintf("set PDU.OutletSystem.Outlet[%d].DelayBeforeShutdown 0", outlet)
	default:
		return fmt.Errorf("invalid state: %s", state)
	}

	_, err := RunSSHCommand(cfg, cmd)
	return err
}
