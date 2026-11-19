package kasapdu

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"
)

var log = slog.Default()

// SetLogger sets the package-level logger.
func SetLogger(logger *slog.Logger) {
	if logger != nil {
		log = logger
	}
}

// Config holds connection details for a Kasa HS300 power strip.
type Config struct {
	Host     string
	LogLevel int
}

type childInfo struct {
	ID    string `json:"id"`
	State int    `json:"state"`
	Alias string `json:"alias"`
}

type sysInfoResponse struct {
	System struct {
		GetSysinfo struct {
			Children []childInfo `json:"children"`
		} `json:"get_sysinfo"`
	} `json:"system"`
}

type setRelayResponse struct {
	System struct {
		SetRelayState struct {
			ErrCode int `json:"err_code"`
		} `json:"set_relay_state"`
	} `json:"system"`
}

// encrypt applies the Kasa XOR cipher (key=171, chaining).
func encrypt(data []byte) []byte {
	key := byte(171)
	result := make([]byte, len(data))
	for i, b := range data {
		key ^= b
		result[i] = key
	}
	return result
}

// decrypt reverses the Kasa XOR cipher.
func decrypt(data []byte) []byte {
	key := byte(171)
	result := make([]byte, len(data))
	for i, b := range data {
		result[i] = key ^ b
		key = b
	}
	return result
}

// send opens a TCP connection to the device, sends an encrypted command, and
// returns the decrypted response. Uses 4-byte big-endian length prefix.
func send(host, cmd string, timeoutSec int) ([]byte, error) {
	d := time.Duration(timeoutSec) * time.Second
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:9999", host), d)
	if err != nil {
		return nil, fmt.Errorf("connect %s:9999: %w", host, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(d))

	payload := encrypt([]byte(cmd))
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := conn.Write(append(header, payload...)); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}
	respLen := binary.BigEndian.Uint32(lenBuf)
	respBuf := make([]byte, respLen)
	if _, err := io.ReadFull(conn, respBuf); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return decrypt(respBuf), nil
}

// GetOutlets returns a map of 0-indexed outlet number → "on"/"off".
func GetOutlets(cfg Config) (map[int]string, error) {
	resp, err := send(cfg.Host, `{"system":{"get_sysinfo":{}}}`, 10)
	if err != nil {
		return nil, fmt.Errorf("get_sysinfo: %w", err)
	}
	if log != nil {
		log.Debug("kasapdu get_sysinfo response", "host", cfg.Host, "resp", string(resp))
	}
	var parsed sysInfoResponse
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("parse sysinfo: %w", err)
	}
	result := make(map[int]string)
	for i, child := range parsed.System.GetSysinfo.Children {
		if child.State == 1 {
			result[i] = "on"
		} else {
			result[i] = "off"
		}
	}
	return result, nil
}

// SetOutlet sets a 0-indexed outlet to "on" or "off".
func SetOutlet(cfg Config, outlet int, state string) error {
	if state != "on" && state != "off" {
		return fmt.Errorf("invalid state: %s", state)
	}

	// Fetch sysinfo to get child IDs.
	resp, err := send(cfg.Host, `{"system":{"get_sysinfo":{}}}`, 10)
	if err != nil {
		return fmt.Errorf("get_sysinfo for SetOutlet: %w", err)
	}
	var parsed sysInfoResponse
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return fmt.Errorf("parse sysinfo: %w", err)
	}
	children := parsed.System.GetSysinfo.Children
	if outlet < 0 || outlet >= len(children) {
		return fmt.Errorf("outlet %d out of range (0-%d)", outlet, len(children)-1)
	}

	relayState := 0
	if state == "on" {
		relayState = 1
	}
	cmd := fmt.Sprintf(
		`{"context":{"child_ids":["%s"]},"system":{"set_relay_state":{"state":%d}}}`,
		children[outlet].ID, relayState,
	)
	setResp, err := send(cfg.Host, cmd, 10)
	if err != nil {
		return fmt.Errorf("set_relay_state: %w", err)
	}
	if log != nil {
		log.Debug("kasapdu set_relay_state response", "host", cfg.Host, "outlet", outlet, "state", state, "resp", string(setResp))
	}
	var result setRelayResponse
	if err := json.Unmarshal(setResp, &result); err != nil {
		return fmt.Errorf("parse set_relay_state response: %w", err)
	}
	if result.System.SetRelayState.ErrCode != 0 {
		return fmt.Errorf("set_relay_state err_code: %d", result.System.SetRelayState.ErrCode)
	}
	return nil
}
