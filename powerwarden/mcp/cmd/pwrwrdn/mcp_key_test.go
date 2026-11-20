package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keyreq"
)

const noKeyPrefix = "No powerwarden key on this machine yet. "

type mcpRun struct {
	cs     *mcp.ClientSession
	stdout *syncBuf // every byte the server wrote to its stdout
	done   chan int
}

// teeWC writes to the pipe and records what it carried.
type teeWC struct {
	w   io.WriteCloser
	rec *syncBuf
}

func (t teeWC) Write(p []byte) (int, error) { t.rec.Write(p); return t.w.Write(p) }
func (t teeWC) Close() error                { return t.w.Close() }

// startMCP runs `pwrwrdn mcp` in this process over pipes.
func startMCP(t *testing.T, k *keyRig, args ...string) *mcpRun {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	m := &mcpRun{stdout: &syncBuf{}, done: make(chan int, 1)}
	e := k.env(append([]string{"mcp"}, args...)...)
	e.Args = append([]string{"mcp"}, args...)
	e.Stdin, e.Stdout = inR, teeWC{outW, m.stdout}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		m.done <- dispatch(ctx, e)
		outW.Close()
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	m.cs = cs
	t.Cleanup(func() {
		_ = cs.Close()
		inW.Close()
		cancel()
		select {
		case <-m.done:
		case <-time.After(15 * time.Second):
			t.Error("pwrwrdn mcp did not exit")
		}
	})
	return m
}

// startMCPProcess runs `pwrwrdn mcp` as a real process.
func startMCPProcess(t *testing.T, k *keyRig) *mcpRun {
	t.Helper()
	cmd := k.subprocess("mcp")
	var eb syncBuf
	cmd.Stderr = &eb
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("%v %s", err, eb.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &mcpRun{cs: cs}
}

func (m *mcpRun) call(t *testing.T, tool string, args map[string]any) (isErr bool, text string) {
	t.Helper()
	res, err := m.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	return res.IsError, strings.Join(texts, "\n")
}

// until calls until the answer is no longer the "no key yet" text.
func (m *mcpRun) until(t *testing.T, tool string, args map[string]any) (bool, string) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if isErr, text := m.call(t, tool, args); !strings.HasPrefix(text, noKeyPrefix) {
			return isErr, text
		}
	}
	t.Fatal("the request never settled")
	return false, ""
}

// expectedNotice rebuilds Notice.String() from the pending request as keyclient
// reports it (service, code, expiry) and what the fakes serve.
func expectedNotice(t *testing.T, k *keyRig) string {
	t.Helper()
	n, ok, err := request.Pending(keyreq.Options(keyreq.Params{ServiceURL: k.f.Srv.URL,
		Override: k.getenv["POWERWARDEN_KEY_REQUEST_URL"], TokenPath: k.token}))
	if err != nil || !ok {
		t.Fatalf("pending: %v %v", ok, err)
	}
	p := struct {
		Service, Code string
		Expires       time.Time
	}{n.Service, n.UserCode, n.ExpiresAt}
	return request.Notice{Service: p.Service, ApproveURL: k.p.URL() + "/requests?code=" + p.Code, UserCode: p.Code,
		Role: "agent", Tool: "pwrwrdn/0.2.0", ExpiresAt: p.Expires}.String()
}

// D10: the first call returns the notice as an error result, the second (before
// approval) the same one with no second filing.
func TestMCP_FirstCallsReturnTheNotice(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		k := newKeyRig(t, discovery)
		m := startMCP(t, k)
		isErr, text := m.call(t, "list_hosts", nil)
		want := noKeyPrefix + expectedNotice(t, k) + " Then retry."
		if !isErr || text != want {
			t.Fatalf("isErr=%v\n got %q\nwant %q", isErr, text, want)
		}
		_, code := approveURLAndCode(t, text)
		if !strings.Contains(text, "code "+code+" matches") || !strings.Contains(text, k.p.URL()+"/requests?code="+code) {
			t.Fatalf("no code or approval address: %q", text)
		}
		isErr, text2 := m.call(t, "my_claims", nil)
		if !isErr || text2 != text {
			t.Fatalf("second call %q", text2)
		}
		if n := len(k.p.Filed()); n != 1 {
			t.Fatalf("filed %d", n)
		}
		if f := k.p.Filed()[0]; f.Role != "agent" || f.Tool != "pwrwrdn/0.2.0" {
			t.Fatalf("%+v", f)
		}
		k.noSecrets(text, text2)
		if _, err := os.Stat(k.token); err == nil {
			t.Fatal("token exists")
		}
		assertProtocolOnly(t, m.stdout)
	}
}

// assertProtocolOnly: every line the server wrote to stdout is a JSON-RPC message.
func assertProtocolOnly(t *testing.T, out *syncBuf) {
	t.Helper()
	s := out.String()
	if s == "" {
		t.Fatal("no protocol output at all")
	}
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		var m struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if json.Unmarshal([]byte(l), &m) != nil || m.JSONRPC != "2.0" {
			t.Fatalf("stdout carries something that is not protocol: %q", l)
		}
	}
}

// Approval: the next call succeeds with the provider's key as bearer, and the
// server stays quiet on stdout.
func TestMCP_ApprovalThenServes(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.Script(keyclienttest.Approved())
	m := startMCP(t, k)
	if isErr, text := m.call(t, "list_hosts", nil); !isErr || !strings.HasPrefix(text, noKeyPrefix) {
		t.Fatalf("%v %q", isErr, text)
	}
	isErr, text := m.until(t, "list_hosts", nil)
	if isErr || !strings.HasPrefix(text, `{"hosts":`) {
		t.Fatalf("%v %q", isErr, text)
	}
	if q, ok := k.f.Last("GET", "/v1/hosts"); !ok || q.Auth != "Bearer "+k.p.Key() {
		t.Fatalf("bearer %q", q.Auth)
	}
	if b, _ := os.ReadFile(k.token); strings.TrimSpace(string(b)) != k.p.Key() {
		t.Fatal("key not stored")
	}
	if isErr, _ := m.call(t, "my_claims", nil); isErr {
		t.Fatal("serving normally")
	}
	if len(k.p.Filed()) != 1 {
		t.Fatal("filed twice")
	}
	k.noSecrets(text)
	assertProtocolOnly(t, m.stdout)
}

// L-13: after a denial the next call says so; the call after files a new request.
func TestL13_MCP_DeniedThenFilesAgain(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.Script(keyclienttest.Denied())
	m := startMCP(t, k)
	if _, text := m.call(t, "list_hosts", nil); !strings.HasPrefix(text, noKeyPrefix) {
		t.Fatalf("%q", text)
	}
	isErr, text := m.until(t, "list_hosts", nil)
	if !isErr || text != "key request denied" {
		t.Fatalf("%v %q", isErr, text)
	}
	if n := len(k.p.Filed()); n != 1 {
		t.Fatalf("filed %d", n)
	}
	isErr, text = m.call(t, "list_hosts", nil)
	if !isErr || !strings.HasPrefix(text, noKeyPrefix) || len(k.p.Filed()) != 2 {
		t.Fatalf("%v %q filed=%d", isErr, text, len(k.p.Filed()))
	}
}

func TestMCP_FinalErrorsAreSR9Words(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.Script(keyclienttest.Expired())
	m := startMCP(t, k)
	m.call(t, "list_hosts", nil)
	isErr, text := m.until(t, "list_hosts", nil)
	if !isErr || text != "key request: expired\nrun this again to file a new request" {
		t.Fatalf("%v %q", isErr, text)
	}
}

func TestMCP_ProviderErrorAndNoProvider(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.FileWith(keyclienttest.Status(429, `{"error":"rate_limited","message":"slow down"}`).WithHeader("Retry-After", "3"))
	m := startMCP(t, k)
	isErr, text := m.call(t, "list_hosts", nil)
	if !isErr || text != "key provider refused the request (status 429): slow down (retry after 3s)\ntry again in 3s" {
		t.Fatalf("%v %q", isErr, text)
	}
	k2 := newKeyRig(t, false)
	delete(k2.getenv, "POWERWARDEN_KEY_REQUEST_URL")
	m2 := startMCP(t, k2)
	isErr, text = m2.call(t, "list_hosts", nil)
	if !isErr || text != "token file: stat "+k2.token+": no such file or directory" {
		t.Fatalf("%v %q", isErr, text)
	}
}

// T-29: the lock is held by another flow that is still filing: ErrBusy.
func TestT29_MCPBusy(t *testing.T) {
	k := newKeyRig(t, false)
	real, alias := aliasedToken(t, k)
	holdFiling(t, k, real)
	m := startMCP(t, k, "--token-file", alias)
	isErr, text := m.call(t, "list_hosts", nil)
	if !isErr || text != "another pwrwrdn is handling the key request; retry shortly" {
		t.Fatalf("%v %q", isErr, text)
	}
	if len(k.p.Filed()) != 1 {
		t.Fatal("a second request was filed")
	}
}

// A key already on disk: as before, no request.
func TestMCP_KeyOnDiskUnchanged(t *testing.T) {
	k := newKeyRig(t, false)
	if err := os.WriteFile(k.token, []byte("disk-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := startMCP(t, k)
	if isErr, text := m.call(t, "list_hosts", nil); isErr {
		t.Fatalf("%q", text)
	}
	if len(k.p.Filed()) != 0 {
		t.Fatal("filed")
	}
}

// Retry once on the first call after the key lands.
func TestMCP_RetryOnceAfterTheKeyLands(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.Script(keyclienttest.Approved())
	var n *atomic.Int32
	k.f.Respond, n = hostsAnswer(401, 200)
	m := startMCP(t, k)
	m.call(t, "list_hosts", nil)
	isErr, text := m.until(t, "list_hosts", nil)
	if isErr || n.Load() != 2 {
		t.Fatalf("%v %q calls=%d", isErr, text, n.Load())
	}
	// only the first call is retried
	k.f.Respond, n = hostsAnswer(401, 200)
	if isErr, _ := m.call(t, "list_hosts", nil); !isErr || n.Load() != 1 {
		t.Fatalf("second call: isErr=%v calls=%d", isErr, n.Load())
	}
}

// Nothing in the key path writes to os.Stdout: swap it for a pipe while the
// flow runs.
func TestMCP_NothingWrittenToOsStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var captured bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&captured, r); close(done) }()
	k := newKeyRig(t, false)
	k.p.Script(keyclienttest.Approved())
	m := startMCP(t, k)
	m.call(t, "list_hosts", nil)
	m.until(t, "list_hosts", nil)
	os.Stdout = old
	w.Close()
	<-done
	if captured.Len() != 0 {
		t.Fatalf("os.Stdout got %q", captured.String())
	}
}
