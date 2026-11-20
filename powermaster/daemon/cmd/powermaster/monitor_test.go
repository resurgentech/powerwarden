package main

// B9: `powermaster monitor` asks for a key on first use (SR-9). The tests use
// a fake powermaster service (httptest) and keyclient's fake key provider, and
// replace the TUI with a function that records the call. Names cite the rows of
// keyclient's docs/STATES.md where a test proves one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/keyreq"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/tui"
)

const (
	pendingLine = "key request pending; run this again after approving"
	retryLine   = "run this again to file a new request"
)

type rig struct {
	t         *testing.T
	prov      *keyclienttest.Provider
	svc       *httptest.Server
	tokenPath string
	stderr    bytes.Buffer
	terminal  bool
	env       map[string]string

	mu        sync.Mutex
	discovery bool
	hostCodes []int    // statuses for GET /v1/hosts, in order; then 200
	hostAuth  []string // Authorization header of each GET /v1/hosts
	hostTimes []time.Time

	tuiCalls int
	tuiHosts int // what a ListHosts from inside the TUI saw
	tuiErr   error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, prov: keyclienttest.NewProvider(t), discovery: true,
		env: map[string]string{"USER": "tester"}}
	r.prov.SetInterval(1)
	r.tokenPath = filepath.Join(t.TempDir(), "cfg", "token")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/key-request", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		on := r.discovery
		r.mu.Unlock()
		if !on {
			http.NotFound(w, nil)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"request_url": r.prov.RequestURL()})
	})
	mux.HandleFunc("GET /v1/hosts", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hostAuth = append(r.hostAuth, req.Header.Get("Authorization"))
		r.hostTimes = append(r.hostTimes, time.Now())
		code := 200
		if len(r.hostCodes) > 0 {
			code, r.hostCodes = r.hostCodes[0], r.hostCodes[1:]
		}
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == 200 {
			_, _ = w.Write([]byte("[]"))
		} else {
			_, _ = w.Write([]byte(`{"error":"unauthorized","message":"bad token"}`))
		}
	})
	r.svc = httptest.NewServer(mux)
	t.Cleanup(r.svc.Close)
	old := retryDelay
	retryDelay = 20 * time.Millisecond
	t.Cleanup(func() { retryDelay = old })
	return r
}

func (r *rig) menv() monitorEnv {
	return monitorEnv{
		getenv:     func(k string) string { return r.env[k] },
		home:       r.t.TempDir(),
		stderr:     &r.stderr,
		isTerminal: func() bool { return r.terminal },
		hostname:   func() (string, error) { return "box.example.net", nil },
		runTUI: func(ctx context.Context, c tui.API, _ tui.Config) error {
			r.tuiCalls++
			_, r.tuiErr = c.ListHosts(ctx)
			return nil
		},
	}
}

// run runs monitor against the fake service with the rig's token path, then
// checks that nothing it printed holds the poll secret or the key.
func (r *rig) run(extra ...string) error {
	r.t.Helper()
	args := append([]string{"--api-url", r.svc.URL, "--token-file", r.tokenPath}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := monitorWith(ctx, args, r.menv())
	reportError(&r.stderr, err) // what main prints, on the same stderr
	all := r.stderr.String()
	for _, secret := range []string{r.prov.PollSecret(), r.prov.Key()} {
		if secret != "" && strings.Contains(all, secret) {
			r.t.Fatalf("output holds a secret %q: %s", secret, all)
		}
	}
	return err
}

func (r *rig) lines() []string {
	s := strings.TrimRight(r.stderr.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (r *rig) override() []string { return []string{"--key-request-url", r.prov.RequestURL()} }

func (r *rig) hostCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hostAuth...)
}

func (r *rig) wantNoToken() {
	r.t.Helper()
	if _, err := os.Stat(r.tokenPath); !errors.Is(err, os.ErrNotExist) {
		r.t.Fatalf("token file exists or unreadable: %v", err)
	}
}

func asExit(t *testing.T, err error) *keyreq.ExitError {
	t.Helper()
	var e *keyreq.ExitError
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *keyreq.ExitError", err)
	}
	return e
}

// T-16 + SR-9: no terminal, no token: the notice, then the pending line, exit
// 75; the TUI never starts; the request stays on disk.
func TestT16_NoTerminalPrintsNoticeAndExits75(t *testing.T) {
	for _, via := range []string{"flag", "env", "discovery"} {
		t.Run(via, func(t *testing.T) {
			r := newRig(t)
			var extra []string
			switch via {
			case "flag":
				extra = r.override()
			case "env":
				r.env["POWERMASTER_KEY_REQUEST_URL"] = r.prov.RequestURL()
			}
			err := r.run(extra...)
			e := asExit(t, err)
			if e.Code != 75 {
				t.Fatalf("exit %d, want 75", e.Code)
			}
			lines := r.lines()
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "Approve this key request for ") ||
				!strings.Contains(lines[0], "check that the code ") || lines[1] != pendingLine {
				t.Fatalf("stderr = %q", lines)
			}
			if r.tuiCalls != 0 {
				t.Fatal("TUI started")
			}
			r.wantNoToken()
			if _, err := os.Stat(request.PendingPath(r.tokenPath)); err != nil {
				t.Fatalf("pending file: %v", err)
			}
			if f := r.prov.Filed(); len(f) != 1 {
				t.Fatalf("filed %d requests, want 1", len(f))
			}
		})
	}
}

func TestNoKeyRequestFlagBehavesAsNoTerminal(t *testing.T) {
	r := newRig(t)
	r.terminal = true
	err := r.run(append(r.override(), "--no-key-request")...)
	if e := asExit(t, err); e.Code != 75 {
		t.Fatalf("exit %d, want 75", e.Code)
	}
	lines := r.lines()
	if len(lines) != 2 || lines[1] != pendingLine {
		t.Fatalf("stderr = %q", lines)
	}
	if r.tuiCalls != 0 {
		t.Fatal("TUI started")
	}
	r.wantNoToken()
}

// L-13 family: with a terminal the monitor waits; on approval the key is
// stored 0600, used as the bearer, and the TUI starts.
func TestTerminalPendingThenApproved(t *testing.T) {
	r := newRig(t)
	r.terminal = true
	r.prov.Script(keyclienttest.Pending(), keyclienttest.Approved())
	if err := r.run(r.override()...); err != nil {
		t.Fatal(err)
	}
	if r.tuiCalls != 1 {
		t.Fatalf("TUI calls = %d, want 1", r.tuiCalls)
	}
	b, err := os.ReadFile(r.tokenPath)
	if err != nil || strings.TrimSpace(string(b)) != r.prov.Key() {
		t.Fatalf("token file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(r.tokenPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", fi.Mode().Perm())
	}
	calls := r.hostCalls()
	if len(calls) != 2 { // the probe, then the TUI's own call
		t.Fatalf("host calls = %q", calls)
	}
	for _, a := range calls {
		if a != "Bearer "+r.prov.Key() {
			t.Fatalf("bearer = %q", a)
		}
	}
	if r.tuiErr != nil {
		t.Fatalf("TUI call: %v", r.tuiErr)
	}
	if n := len(r.lines()); n != 1 { // the notice only
		t.Fatalf("stderr = %q", r.lines())
	}
}

func TestOutcomes(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*rig)
		lines func(*rig) []string
		code  int
	}{
		{"T-denied", func(r *rig) { r.prov.Script(keyclienttest.Denied()) },
			func(*rig) []string { return []string{"key request denied"} }, 1},
		{"expired", func(r *rig) { r.prov.Script(keyclienttest.Expired()) },
			func(*rig) []string { return []string{request.ErrExpired.Error(), retryLine} }, 1},
		{"consumed", func(r *rig) { r.prov.Script(keyclienttest.Consumed()) },
			func(*rig) []string { return []string{request.ErrConsumed.Error(), retryLine} }, 1},
		{"provider error with Retry-After", func(r *rig) {
			r.prov.FileWith(keyclienttest.Status(429, `{"error":"rate_limited","message":"slow down"}`).WithHeader("Retry-After", "7"))
		}, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.terminal = true
			c.setup(r)
			err := r.run(r.override()...)
			e := asExit(t, err)
			if e.Code != c.code {
				t.Fatalf("exit %d, want %d", e.Code, c.code)
			}
			lines := r.lines()
			if c.lines != nil {
				want := c.lines(r)
				// The notice is printed first when the request was filed.
				if len(lines) < len(want) {
					t.Fatalf("stderr = %q, want tail %q", lines, want)
				}
				got := lines[len(lines)-len(want):]
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("stderr = %q, want tail %q", lines, want)
				}
			}
			if r.tuiCalls != 0 {
				t.Fatal("TUI started")
			}
			r.wantNoToken()
		})
	}
}

func TestProviderErrorRetryAfterLine(t *testing.T) {
	r := newRig(t)
	r.terminal = true
	r.prov.FileWith(keyclienttest.Status(429, `{"error":"rate_limited","message":"slow down"}`).WithHeader("Retry-After", "7"))
	err := r.run(r.override()...)
	if e := asExit(t, err); e.Code != 1 {
		t.Fatalf("exit %d", e.Code)
	}
	lines := r.lines()
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "key provider refused the request (status 429)") || lines[1] != "try again in 7s" {
		t.Fatalf("stderr = %q", lines)
	}
}

// T-29 family: another holder of the lock, no request in the pending file:
// ErrBusy, with the exact line and exit 75.
func TestBusyLockExits75(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Dir(r.tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(r.tokenPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	err = r.run(r.override()...)
	e := asExit(t, err)
	want := "another process is handling the key request for " + r.tokenPath + "; try again shortly"
	if e.Code != 75 || len(r.lines()) != 1 || r.lines()[0] != want {
		t.Fatalf("exit %d, stderr %q, want %q", e.Code, r.lines(), want)
	}
	if r.tuiCalls != 0 {
		t.Fatal("TUI started")
	}
}

// T-7: no provider: today's error, unchanged, and nothing created on disk.
func TestT7_NoProviderKeepsTodaysError(t *testing.T) {
	r := newRig(t)
	r.terminal = true
	r.discovery = false
	err := r.run()
	if err == nil {
		t.Fatal("no error")
	}
	var ee *keyreq.ExitError
	if errors.As(err, &ee) {
		t.Fatalf("got an exit error %v, want today's plain error", err)
	}
	want := "token file: open " + r.tokenPath + ": no such file or directory"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
	if code := keyreqCode(err); code != 1 || r.stderr.String() != "powermaster: "+want+"\n" {
		t.Fatalf("code %d, stderr %q", code, r.stderr.String())
	}
	if _, err := os.Stat(filepath.Dir(r.tokenPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory created: %v", err)
	}
	if r.tuiCalls != 0 || len(r.prov.Filed()) != 0 {
		t.Fatalf("tui %d, filed %d", r.tuiCalls, len(r.prov.Filed()))
	}
}

func approvedRig(t *testing.T, codes ...int) *rig {
	r := newRig(t)
	r.terminal = true
	r.hostCodes = codes
	r.prov.Script(keyclienttest.Approved())
	return r
}

// SR-9: a just-issued key whose first call gets 401 is retried once, after a
// wait.
func TestRetryOnceOnJustIssuedKey(t *testing.T) {
	r := approvedRig(t, 401, 200)
	if err := r.run(r.override()...); err != nil {
		t.Fatal(err)
	}
	if len(r.hostTimes) < 2 || r.hostTimes[1].Sub(r.hostTimes[0]) < retryDelay {
		t.Fatalf("did not wait %v between the probe and the retry: %v", retryDelay, r.hostTimes)
	}
	calls := r.hostCalls()
	if len(calls) != 3 { // probe 401, retry 200, the TUI's call
		t.Fatalf("host calls = %q", calls)
	}
	if r.tuiCalls != 1 {
		t.Fatalf("TUI calls = %d", r.tuiCalls)
	}
}

func TestRetryTwice401Fails(t *testing.T) {
	r := approvedRig(t, 401, 401, 401, 401)
	err := r.run(r.override()...)
	if err == nil || keyreqCode(err) != 1 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if n := len(r.hostCalls()); n != 2 {
		t.Fatalf("host calls = %d, want exactly 2", n)
	}
	if r.tuiCalls != 0 {
		t.Fatal("TUI started")
	}
}

func keyreqCode(err error) int {
	var e *keyreq.ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return 1
}

// A key already on disk gets no probe and no retry: today's path.
func TestKeyOnDiskNoProbeNoRetry(t *testing.T) {
	r := newRig(t)
	r.terminal = true
	r.hostCodes = []int{401}
	if err := os.MkdirAll(filepath.Dir(r.tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.tokenPath, []byte("on-disk-token-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.run(r.override()...); err != nil {
		t.Fatal(err)
	}
	if r.tuiCalls != 1 {
		t.Fatalf("TUI calls = %d", r.tuiCalls)
	}
	calls := r.hostCalls()
	if len(calls) != 1 { // only the TUI's own call, which got the 401
		t.Fatalf("host calls = %q", calls)
	}
	if len(r.prov.Filed()) != 0 {
		t.Fatal("filed a request")
	}
	if r.stderr.Len() != 0 {
		t.Fatalf("stderr = %q", r.stderr.String())
	}
}

func TestRoleNameHintAndTool(t *testing.T) {
	r := newRig(t)
	_ = r.run(r.override()...)
	f := r.prov.Filed()
	if len(f) != 1 {
		t.Fatalf("filed %d", len(f))
	}
	if f[0].Role != "full" || f[0].NameHint != "tester@box" || f[0].Tool != "powermaster/0.4.0" {
		t.Fatalf("filed %+v", f[0])
	}

	r2 := newRig(t)
	_ = r2.run(append(r2.override(), "--role", "ops")...)
	if f := r2.prov.Filed(); len(f) != 1 || f[0].Role != "ops" {
		t.Fatalf("filed %+v", f)
	}

	// No user name: the hint is the short host alone, never "@host".
	r3 := newRig(t)
	delete(r3.env, "USER")
	_ = r3.run(r3.override()...)
	if f := r3.prov.Filed(); len(f) != 1 || f[0].NameHint != "box" {
		t.Fatalf("filed %+v", f)
	}
}

// The flag wins over the env var.
func TestFlagBeatsEnv(t *testing.T) {
	r := newRig(t)
	r.env["POWERMASTER_KEY_REQUEST_URL"] = "http://127.0.0.1:1/never"
	err := r.run(r.override()...)
	if e := asExit(t, err); e.Code != 75 {
		t.Fatalf("exit %d, lines %q", e.Code, r.lines())
	}
}

// Printing: SR-9 lines are bare; anything else keeps the prefix.
func TestReportError(t *testing.T) {
	var b bytes.Buffer
	if c := reportError(&b, &keyreq.ExitError{Lines: []string{"a", "b"}, Code: 75}); c != 75 || b.String() != "a\nb\n" {
		t.Fatalf("code %d out %q", c, b.String())
	}
	b.Reset()
	if c := reportError(&b, errors.New("boom")); c != 1 || b.String() != "powermaster: boom\n" {
		t.Fatalf("code %d out %q", c, b.String())
	}
	if c := reportError(&b, nil); c != 0 {
		t.Fatalf("code %d", c)
	}
}
