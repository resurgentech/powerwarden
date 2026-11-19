package kasapdu

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSysinfo = map[string]interface{}{
	"system": map[string]interface{}{
		"get_sysinfo": map[string]interface{}{
			"children": []map[string]interface{}{
				{"id": "8006000000000000000000000000000000000000", "state": 1, "alias": "Plug 1"},
				{"id": "8006000000000000000000000000000000000001", "state": 0, "alias": "Plug 2"},
				{"id": "8006000000000000000000000000000000000002", "state": 1, "alias": "Plug 3"},
				{"id": "8006000000000000000000000000000000000003", "state": 0, "alias": "Plug 4"},
				{"id": "8006000000000000000000000000000000000004", "state": 1, "alias": "Plug 5"},
				{"id": "8006000000000000000000000000000000000005", "state": 0, "alias": "Plug 6"},
			},
		},
	},
}

var testSetRelayOK = map[string]interface{}{
	"system": map[string]interface{}{
		"set_relay_state": map[string]interface{}{"err_code": 0},
	},
}

var testSetRelayErr = map[string]interface{}{
	"system": map[string]interface{}{
		"set_relay_state": map[string]interface{}{"err_code": -1},
	},
}

// kasaServer spins up a local TCP listener that speaks the Kasa wire protocol.
// handler receives the decrypted JSON command string and returns a response object.
// Returns host:port and a close func.
func kasaServer(t *testing.T, handler func(cmd string) interface{}) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveKasaConn(conn, handler)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

func serveKasaConn(conn net.Conn, handler func(string) interface{}) {
	defer conn.Close()
	for {
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(lenBuf))
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		resp := handler(string(decrypt(body)))
		respJSON, _ := json.Marshal(resp)
		enc := encrypt(respJSON)
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(len(enc)))
		conn.Write(append(header, enc...))
	}
}

// sendToAddr is like send() but accepts an explicit host:port instead of appending :9999.
func sendToAddr(addr, cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	payload := encrypt([]byte(cmd))
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := conn.Write(append(header, payload...)); err != nil {
		return nil, err
	}
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, err
	}
	respBuf := make([]byte, binary.BigEndian.Uint32(lenBuf))
	if _, err := io.ReadFull(conn, respBuf); err != nil {
		return nil, err
	}
	return decrypt(respBuf), nil
}

// getOutletsFrom is GetOutlets() against an explicit addr (bypasses :9999 suffix).
func getOutletsFrom(addr string) (map[int]string, error) {
	resp, err := sendToAddr(addr, `{"system":{"get_sysinfo":{}}}`)
	if err != nil {
		return nil, err
	}
	var parsed sysInfoResponse
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, err
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

// setOutletTo is SetOutlet() against an explicit addr (bypasses :9999 suffix).
func setOutletTo(addr string, outlet int, state string) error {
	if state != "on" && state != "off" {
		return fmt.Errorf("invalid state: %s", state)
	}
	resp, err := sendToAddr(addr, `{"system":{"get_sysinfo":{}}}`)
	if err != nil {
		return err
	}
	var parsed sysInfoResponse
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return err
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
	setResp, err := sendToAddr(addr, cmd)
	if err != nil {
		return err
	}
	var result setRelayResponse
	if err := json.Unmarshal(setResp, &result); err != nil {
		return err
	}
	if result.System.SetRelayState.ErrCode != 0 {
		return fmt.Errorf("set_relay_state err_code: %d", result.System.SetRelayState.ErrCode)
	}
	return nil
}

// --- Tests ---

func TestEncryptDecryptRoundtrip(t *testing.T) {
	cases := []string{
		`{"system":{"get_sysinfo":{}}}`,
		`{"context":{"child_ids":["abc123"]},"system":{"set_relay_state":{"state":1}}}`,
		"",
		strings.Repeat("x", 256),
	}
	for _, msg := range cases {
		b := []byte(msg)
		assert.Equal(t, b, decrypt(encrypt(b)), "roundtrip failed: %q", msg)
	}
}

func TestEncryptKnownVector(t *testing.T) {
	// key=171, first byte: 171 XOR '{' (123) = 208
	enc := encrypt([]byte("{"))
	assert.Equal(t, byte(171^123), enc[0])
	// second byte: 208 XOR '}' (125) = 181
	enc2 := encrypt([]byte("{}"))
	assert.Equal(t, byte(208^125), enc2[1])
}

func TestGetOutlets(t *testing.T) {
	addr, stop := kasaServer(t, func(_ string) interface{} { return testSysinfo })
	defer stop()

	outlets, err := getOutletsFrom(addr)
	require.NoError(t, err)
	require.Len(t, outlets, 6)
	assert.Equal(t, "on", outlets[0])
	assert.Equal(t, "off", outlets[1])
	assert.Equal(t, "on", outlets[2])
	assert.Equal(t, "off", outlets[3])
	assert.Equal(t, "on", outlets[4])
	assert.Equal(t, "off", outlets[5])
}

func TestSetOutletOn(t *testing.T) {
	var capturedCmd string
	call := 0
	addr, stop := kasaServer(t, func(cmd string) interface{} {
		call++
		if call == 1 {
			return testSysinfo
		}
		capturedCmd = cmd
		return testSetRelayOK
	})
	defer stop()

	err := setOutletTo(addr, 1, "on")
	require.NoError(t, err)
	assert.Equal(t, 2, call, "expected sysinfo + set_relay_state calls")
	assert.Contains(t, capturedCmd, `"state":1`)
	assert.Contains(t, capturedCmd, "8006000000000000000000000000000000000001")
}

func TestSetOutletOff(t *testing.T) {
	var capturedCmd string
	call := 0
	addr, stop := kasaServer(t, func(cmd string) interface{} {
		call++
		if call == 1 {
			return testSysinfo
		}
		capturedCmd = cmd
		return testSetRelayOK
	})
	defer stop()

	err := setOutletTo(addr, 0, "off")
	require.NoError(t, err)
	assert.Contains(t, capturedCmd, `"state":0`)
	assert.Contains(t, capturedCmd, "8006000000000000000000000000000000000000")
}

func TestSetOutletInvalidState(t *testing.T) {
	// Rejected before any network call; addr is irrelevant.
	err := setOutletTo("127.0.0.1:1", 0, "flicker")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid state")
}

func TestSetOutletOutOfRange(t *testing.T) {
	addr, stop := kasaServer(t, func(_ string) interface{} { return testSysinfo })
	defer stop()

	err := setOutletTo(addr, 6, "on") // HS300 has outlets 0-5
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

func TestSetOutletDeviceError(t *testing.T) {
	call := 0
	addr, stop := kasaServer(t, func(_ string) interface{} {
		call++
		if call == 1 {
			return testSysinfo
		}
		return testSetRelayErr
	})
	defer stop()

	err := setOutletTo(addr, 0, "on")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "err_code")
}
