package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keyreq"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

// TestMain lets a test run this binary as a real second pwrwrdn process.
func TestMain(m *testing.M) {
	if os.Getenv("PWRWRDN_TEST_REEXEC") == "1" {
		main() // exits
		return
	}
	os.Exit(m.Run())
}

const approvePrefix = "Approve this key request for "

// keyRig is a rig with no token file, a fake key provider, and the key request
// URL given either by the env var (override) or by discovery on the service.
type keyRig struct {
	*rig
	p *keyclienttest.Provider
}

func newKeyRig(t *testing.T, discovery bool) *keyRig {
	t.Helper()
	shrinkRetry(t)
	r := newRig(t)
	if err := os.Remove(r.token); err != nil {
		t.Fatal(err)
	}
	r.getenv["USER"] = "tester"
	p := keyclienttest.NewProvider(t)
	if discovery {
		r.f.KeyRequestURL = p.RequestURL()
	} else {
		r.getenv["POWERWARDEN_KEY_REQUEST_URL"] = p.RequestURL()
	}
	return &keyRig{rig: r, p: p}
}

func shrinkRetry(t *testing.T) {
	t.Helper()
	old := keyreq.RetryDelay
	keyreq.RetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { keyreq.RetryDelay = old })
}

func (k *keyRig) pending() string { return request.PendingPath(k.token) }

func (k *keyRig) serviceHost() string { return strings.TrimPrefix(k.f.Srv.URL, "http://") }

// noSecrets asserts that nothing the run printed holds the poll secret, the
// key or the bearer.
func (k *keyRig) noSecrets(texts ...string) {
	k.t.Helper()
	if k.p.PollSecret() == "" && len(k.p.Filed()) > 0 {
		k.t.Fatal("provider has no poll secret to look for")
	}
	for _, s := range append(texts, k.out.String(), k.errb.String()) {
		for _, secret := range []string{k.p.PollSecret(), k.p.Key(), "Bearer "} {
			if secret != "" && strings.Contains(s, secret) {
				k.t.Fatalf("a secret (%d bytes) leaked into output: %q", len(secret), s)
			}
		}
	}
}

func (k *keyRig) stderrLines() []string {
	return strings.Split(strings.TrimRight(k.errb.String(), "\n"), "\n")
}

func (k *keyRig) reset() { k.out, k.errb = &syncBuf{}, &syncBuf{} }

func (k *keyRig) noToken() {
	k.t.Helper()
	if _, err := os.Lstat(k.token); err == nil {
		k.t.Fatal("token file exists")
	}
}

func (k *keyRig) assertNotice(line string) {
	k.t.Helper()
	if !strings.HasPrefix(line, approvePrefix+k.serviceHost()+" at "+k.p.URL()+"/requests?code=") ||
		!strings.Contains(line, " and check that the code ") || !strings.Contains(line, " matches. Asked for: role agent, tool pwrwrdn/0.2.0. The request expires at ") {
		k.t.Fatalf("not the notice: %q", line)
	}
}

const pendingLine = "key request pending; run this again after approving"

// T-16 / T-10: no terminal, Wait false: the notice, the pending line, exit 75;
// no token; the pending file is kept.
func TestT16_NoTerminalPending(t *testing.T) {
	for _, mode := range []struct {
		name      string
		discovery bool
	}{{"override", false}, {"discovery", true}} {
		t.Run(mode.name, func(t *testing.T) {
			k := newKeyRig(t, mode.discovery)
			k.p.Script(keyclienttest.Approved()) // a Wait true flow would succeed
			if rc := k.do("hosts"); rc != 75 {
				t.Fatalf("rc=%d %s", rc, k.errb)
			}
			lines := k.stderrLines()
			if len(lines) != 2 || lines[1] != pendingLine {
				t.Fatalf("stderr %q", lines)
			}
			k.assertNotice(lines[0])
			k.noToken()
			if fi, err := os.Stat(k.pending()); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("pending file: %v %v", fi, err)
			}
			if k.out.String() != "" {
				t.Fatalf("stdout %q", k.out)
			}
			if n := len(k.p.Filed()); n != 1 {
				t.Fatalf("filed %d", n)
			}
			f := k.p.Filed()[0]
			if f.Role != "agent" || f.Tool != "pwrwrdn/0.2.0" || !strings.HasPrefix(f.NameHint, "tester@") || strings.Contains(f.NameHint, ".") {
				t.Fatalf("filing %+v", f)
			}
			if k.f.Count("GET", "/v1/hosts") != 0 {
				t.Fatal("the service was called without a key")
			}
			k.noSecrets()
		})
	}
}

func TestT16_RoleFlag(t *testing.T) {
	k := newKeyRig(t, false)
	if rc := k.do("hosts", "--role", "ops"); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if f := k.p.Filed(); len(f) != 1 || f[0].Role != "ops" {
		t.Fatalf("%+v", f)
	}
}

func TestT16_KeyRequestURLFlagWinsOverEnv(t *testing.T) {
	k := newKeyRig(t, false)
	k.getenv["POWERWARDEN_KEY_REQUEST_URL"] = "https://nowhere.example.com/never"
	if rc := k.do("hosts", "--key-request-url", k.p.RequestURL()); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if len(k.p.Filed()) != 1 {
		t.Fatal("flag was not used")
	}
}

// --no-key-request forces the non-terminal path even with a terminal.
func TestT16_NoKeyRequestFlagWithTerminal(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	k.p.Script(keyclienttest.Approved())
	if rc := k.do("hosts", "--no-key-request"); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	lines := k.stderrLines()
	if len(lines) != 2 || lines[1] != pendingLine {
		t.Fatalf("stderr %q", lines)
	}
	k.assertNotice(lines[0])
	k.noToken()
	if k.f.Count("GET", "/v1/hosts") != 0 {
		t.Fatal("proceeded")
	}
}

// Every command that loads a token takes the same path.
func TestT16_EveryTokenCommandRequestsAKey(t *testing.T) {
	k := newKeyRig(t, false)
	for _, args := range [][]string{
		{"hosts"}, {"claims"}, {"claim", "--tags", "a"}, {"release", "x"}, {"extend", "x", "1h"},
		{"power", "x", "powerup"}, {"run", "--tags", "a", "--", "true"},
	} {
		k.reset()
		if rc := k.do(args...); rc != 75 {
			t.Fatalf("%v: rc=%d %s", args, rc, k.errb)
		}
		lines := k.stderrLines()
		if lines[len(lines)-1] != pendingLine {
			t.Fatalf("%v: %q", args, lines)
		}
	}
	if n := len(k.p.Filed()); n != 1 {
		t.Fatalf("filed %d: later runs resume the one request", n)
	}
	if len(k.f.Requests()) != 0 {
		t.Fatalf("service called: %v", k.f.Requests())
	}
}

// T-10, L-8: a terminal waits for the approval, then the command goes on.
func TestT10_TerminalApprovalProceeds(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		k := newKeyRig(t, discovery)
		k.term = true
		k.p.Script(keyclienttest.Pending(), keyclienttest.Approved())
		if rc := k.do("hosts"); rc != 0 {
			t.Fatalf("rc=%d %s", rc, k.errb)
		}
		if !strings.Contains(k.out.String(), "HOST") {
			t.Fatalf("stdout %q", k.out)
		}
		k.assertNotice(strings.TrimRight(k.errb.String(), "\n"))
		fi, err := os.Stat(k.token)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("token: %v %v", fi, err)
		}
		if b, _ := os.ReadFile(k.token); strings.TrimSpace(string(b)) != k.p.Key() {
			t.Fatal("stored key differs")
		}
		if q, ok := k.f.Last("GET", "/v1/hosts"); !ok || q.Auth != "Bearer "+k.p.Key() {
			t.Fatalf("bearer %q", q.Auth)
		}
		if len(k.p.Filed()) != 1 {
			t.Fatal("filed twice")
		}
		// the stored key is used as any token afterwards: no new request
		k.reset()
		if rc := k.do("hosts"); rc != 0 || k.errb.String() != "" || len(k.p.Filed()) != 1 {
			t.Fatalf("second run rc=%d %s", rc, k.errb)
		}
		out := k.out.String()
		if strings.Contains(out, k.p.Key()) {
			t.Fatal("key in stdout")
		}
	}
}

// T-16: a later run makes the poll that is due and goes on with the key.
func TestT16_LaterRunCollectsTheKey(t *testing.T) {
	k := newKeyRig(t, false)
	if rc := k.do("hosts"); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	k.p.Script(keyclienttest.Approved())
	time.Sleep(1200 * time.Millisecond) // the pending file records when the next poll is due
	k.reset()
	if rc := k.do("hosts"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if k.errb.String() != "" {
		t.Fatalf("a run that collects the key shows no notice: %q", k.errb)
	}
	if len(k.p.Filed()) != 1 {
		t.Fatal("filed again")
	}
	if _, err := os.Stat(k.pending()); err == nil {
		t.Fatal("pending file left")
	}
}

// T-29: another name for the same token file takes the lock like another
// process; the holder is still filing, so there is nothing to report: ErrBusy.
func holdFiling(t *testing.T, k *keyRig, tokenPath string) {
	t.Helper()
	k.p.FileWith(keyclienttest.Created().After(5 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = request.Ensure(ctx, keyreq.Options(keyreq.Params{ServiceURL: k.f.Srv.URL, Override: k.p.RequestURL(),
			TokenPath: tokenPath, NameHint: "holder@box"}))
	}()
	t.Cleanup(func() { cancel(); <-done })
	for end := time.Now().Add(10 * time.Second); len(k.p.Filed()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("holder never filed")
		}
	}
	time.Sleep(50 * time.Millisecond)
}

// aliasedToken makes the token path twice: the real one and a symlinked dir.
func aliasedToken(t *testing.T, k *keyRig) (real, alias string) {
	t.Helper()
	dir := filepath.Join(k.home, "real")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(k.home, "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "token"), filepath.Join(link, "token")
}

func TestT29_CLIBusy(t *testing.T) {
	k := newKeyRig(t, false)
	real, alias := aliasedToken(t, k)
	holdFiling(t, k, real)
	if rc := k.do("hosts", "--token-file", alias); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	want := "another process is handling the key request for " + alias + "; try again shortly\n"
	if k.errb.String() != want {
		t.Fatalf("stderr %q, want %q", k.errb, want)
	}
	if len(k.p.Filed()) != 1 {
		t.Fatal("a second request was filed")
	}
}

// L-13, L-15, L-14, L-16, T-8: the final outcomes with a terminal (Wait true).
func TestFinalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		o           keyclienttest.Outcome
		want        string // stderr after the notice
		pendingKept bool
	}{
		{"L13_denied", keyclienttest.Denied(), "key request denied\n", false},
		{"L15_expired", keyclienttest.Expired(), "key request: expired\nrun this again to file a new request\n", false},
		{"L14_consumed", keyclienttest.Consumed(), "key request: the key was already handed out and not received; request again\nrun this again to file a new request\n", false},
		{"L16_unauthorized", keyclienttest.Unauthorized(), "key request: poll refused as unauthorized: unauthorized\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKeyRig(t, false)
			k.term = true
			k.p.Script(tc.o)
			if rc := k.do("hosts"); rc != 1 {
				t.Fatalf("rc=%d %s", rc, k.errb)
			}
			s := k.errb.String()
			i := strings.Index(s, "\n")
			k.assertNotice(s[:i])
			if s[i+1:] != tc.want {
				t.Fatalf("stderr %q, want %q", s[i+1:], tc.want)
			}
			if strings.Contains(s, "pwrwrdn:") {
				t.Fatalf("SR-9 lines are printed bare: %q", s)
			}
			k.noToken()
			if _, err := os.Stat(k.pending()); (err == nil) != tc.pendingKept {
				t.Fatalf("pending kept=%v, want %v", err == nil, tc.pendingKept)
			}
			if k.f.Count("GET", "/v1/hosts") != 0 {
				t.Fatal("proceeded")
			}
			k.noSecrets()
		})
	}
}

// T-8: a provider refusal with Retry-After.
func TestT8_ProviderErrorRetryAfter(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.FileWith(keyclienttest.Status(429, `{"error":"rate_limited","message":"slow down"}`).WithHeader("Retry-After", "7"))
	if rc := k.do("hosts"); rc != 1 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	want := "key provider refused the request (status 429): slow down (retry after 7s)\ntry again in 7s\n"
	if k.errb.String() != want {
		t.Fatalf("stderr %q, want %q", k.errb, want)
	}
	k.noToken()
	k.reset()
	k.p.FileWith(keyclienttest.Status(500, `{"error":"boom","message":"it broke"}`))
	if rc := k.do("hosts"); rc != 1 || k.errb.String() != "key provider refused the request (status 500): it broke\n" {
		t.Fatalf("rc=%d %q", rc, k.errb)
	}
}

// T-7: no provider anywhere: today's no-token error, nothing created.
func TestT7_NoProviderKeepsTheNoTokenError(t *testing.T) {
	k := newKeyRig(t, false)
	delete(k.getenv, "POWERWARDEN_KEY_REQUEST_URL") // and the fake service has no discovery answer
	if rc := k.do("hosts"); rc != 1 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	want := "pwrwrdn: token file: stat " + k.token + ": no such file or directory\n"
	if k.errb.String() != want {
		t.Fatalf("stderr %q, want %q", k.errb, want)
	}
	ents, _ := os.ReadDir(filepath.Dir(k.token))
	if len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

// A refused token file is not a missing one: no request.
func TestKeyRequestNotFiledForARefusedTokenFile(t *testing.T) {
	k := newKeyRig(t, false)
	if err := os.WriteFile(k.token, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := k.do("hosts"); rc != 1 || !strings.Contains(k.errb.String(), "must not be readable") || len(k.p.Filed()) != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
}

// ---- retry once on a just-issued key ----

func hostsAnswer(statuses ...int) (func(pwfake.Req) (int, any, bool), *atomic.Int32) {
	var n atomic.Int32
	return func(q pwfake.Req) (int, any, bool) {
		if q.Path != "/v1/hosts" {
			return 0, nil, false
		}
		i := int(n.Add(1)) - 1
		if i < len(statuses) && statuses[i] != 200 {
			return statuses[i], pwfake.Err("unauthorized", "no"), true
		}
		return 0, nil, false
	}, &n
}

func TestRetryOnceOnAFreshKey(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	k.p.Script(keyclienttest.Approved())
	var n *atomic.Int32
	k.f.Respond, n = hostsAnswer(401, 200)
	if rc := k.do("hosts"); rc != 0 || n.Load() != 2 {
		t.Fatalf("rc=%d calls=%d %s", rc, n.Load(), k.errb)
	}
	// the retry is only for the first call: the key is now on disk
	k.reset()
	k.f.Respond, n = hostsAnswer(401, 200)
	if rc := k.do("hosts"); rc != 1 || n.Load() != 1 {
		t.Fatalf("on-disk key: rc=%d calls=%d", rc, n.Load())
	}
}

func TestRetryOnceThenTheFailureSurfaces(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	k.p.Script(keyclienttest.Approved())
	var n *atomic.Int32
	k.f.Respond, n = hostsAnswer(401, 401, 401)
	if rc := k.do("hosts"); rc != 1 || n.Load() != 2 || !strings.Contains(k.errb.String(), "401") {
		t.Fatalf("rc=%d calls=%d %s", rc, n.Load(), k.errb)
	}
	k.noSecrets()
}

func TestNoRetryForAKeyAlreadyOnDisk(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	if err := os.WriteFile(k.token, []byte("disk-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var n *atomic.Int32
	k.f.Respond, n = hostsAnswer(401, 200)
	if rc := k.do("hosts"); rc != 1 || n.Load() != 1 || len(k.p.Filed()) != 0 {
		t.Fatalf("rc=%d calls=%d %s", rc, n.Load(), k.errb)
	}
}

// ---- key request ----

func TestKeyRequestAlreadyPresent(t *testing.T) {
	k := newKeyRig(t, false)
	if err := os.WriteFile(k.token, []byte("disk-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := k.do("key", "request"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if want := "a key is already at " + k.token + "\n"; k.out.String() != want {
		t.Fatalf("stdout %q", k.out)
	}
	if len(k.p.Filed()) != 0 || len(k.f.Requests()) != 0 || k.errb.String() != "" {
		t.Fatal("network or output")
	}
}

func TestKeyRequestTerminalStores(t *testing.T) {
	k := newKeyRig(t, true)
	k.term = true
	k.p.Script(keyclienttest.ApprovedWith("tester@box", "agent", k.p.Key()))
	if rc := k.do("key", "request"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if want := "key for tester@box (role agent) stored at " + k.token + "\n"; k.out.String() != want {
		t.Fatalf("stdout %q, want %q", k.out, want)
	}
	k.assertNotice(strings.TrimRight(k.errb.String(), "\n"))
	if fi, err := os.Stat(k.token); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	k.noSecrets()
	if k.f.Count("GET", "/v1/hosts") != 0 {
		t.Fatal("key request calls nothing else")
	}
}

func TestKeyRequestStoredWithoutName(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	k.p.Script(keyclienttest.ApprovedWith("", "", k.p.Key()))
	if rc := k.do("key", "request"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if want := "key stored at " + k.token + "\n"; k.out.String() != want {
		t.Fatalf("stdout %q", k.out)
	}
}

func TestKeyRequestNoTerminalIsPending(t *testing.T) {
	k := newKeyRig(t, false)
	if rc := k.do("key", "request", "--role", "ops"); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	lines := k.stderrLines()
	if len(lines) != 2 || lines[1] != pendingLine || k.out.String() != "" {
		t.Fatalf("%q %q", lines, k.out)
	}
	if f := k.p.Filed(); len(f) != 1 || f[0].Role != "ops" {
		t.Fatalf("%+v", f)
	}
}

func TestKeyRequestOutcomes(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	k.p.Script(keyclienttest.Denied())
	if rc := k.do("key", "request"); rc != 1 || !strings.HasSuffix(k.errb.String(), "\nkey request denied\n") {
		t.Fatalf("rc=%d %q", rc, k.errb)
	}
	k2 := newKeyRig(t, false)
	delete(k2.getenv, "POWERWARDEN_KEY_REQUEST_URL")
	if rc := k2.do("key", "request"); rc != 1 || !strings.Contains(k2.errb.String(), "no key provider") {
		t.Fatalf("rc=%d %q", rc, k2.errb)
	}
}

func TestKeyUsage(t *testing.T) {
	k := newKeyRig(t, false)
	for _, args := range [][]string{{"key"}, {"key", "bogus"}} {
		k.reset()
		if rc := k.do(args...); rc != 2 || !strings.Contains(k.errb.String(), "usage: pwrwrdn key request|status [flags]") {
			t.Fatalf("%v rc=%d %q", args, rc, k.errb)
		}
	}
}

// ---- key status ----

func TestKeyStatusAbsent(t *testing.T) {
	k := newKeyRig(t, false)
	if rc := k.do("key", "status"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	want := "token    " + k.token + ": absent\npending  " + k.token + ".pending: none\n"
	if k.out.String() != want {
		t.Fatalf("%q want %q", k.out, want)
	}
	if len(k.f.Requests()) != 0 || len(k.p.Filed()) != 0 || len(k.p.Polls()) != 0 {
		t.Fatal("key status used the network")
	}
	ents, _ := os.ReadDir(filepath.Dir(k.token))
	if len(ents) != 0 {
		t.Fatalf("key status created %v", ents)
	}
}

func TestKeyStatusPresent(t *testing.T) {
	k := newKeyRig(t, false)
	if err := os.WriteFile(k.token, []byte("disk-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := k.do("key", "status", "--token-file", k.token, "--url", "https://powerwarden.example.com"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	want := "token    " + k.token + ": present\npending  " + k.token + ".pending: none\n"
	if k.out.String() != want || strings.Contains(k.out.String(), "disk-key") {
		t.Fatalf("%q", k.out)
	}
}

// fileRequest files a request with a no-terminal command and returns the code
// its notice showed; the outputs are then cleared.
func (k *keyRig) fileRequest() (code string) {
	k.t.Helper()
	if rc := k.do("hosts"); rc != 75 {
		k.t.Fatalf("rc=%d %s", rc, k.errb)
	}
	_, code = approveURLAndCode(k.t, k.stderrLines()[0])
	k.reset()
	return code
}

// status runs `key status` and checks that nothing it printed holds a secret
// and that it touched neither the provider nor the service.
func (k *keyRig) status(wantRC int, args ...string) string {
	k.t.Helper()
	filed, polls, reqs := len(k.p.Filed()), len(k.p.Polls()), len(k.f.Requests())
	if rc := k.do(append([]string{"key", "status"}, args...)...); rc != wantRC {
		k.t.Fatalf("rc=%d want %d; stdout %q stderr %q", rc, wantRC, k.out, k.errb)
	}
	k.noSecrets()
	if len(k.p.Filed()) != filed || len(k.p.Polls()) != polls || len(k.f.Requests()) != reqs {
		k.t.Fatal("key status used the network")
	}
	return k.out.String()
}

func (k *keyRig) tokenAbsentLine() string { return "token    " + k.token + ": absent\n" }

// T-30: a current request: its service, code and expiry, nothing more.
func TestT30_KeyStatusCurrent(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		k := newKeyRig(t, discovery)
		code := k.fileRequest()
		out := k.status(0)
		re := regexp.MustCompile(`^` + regexp.QuoteMeta(k.tokenAbsentLine()) + `pending  ` + regexp.QuoteMeta(k.pending()) +
			`: service ` + regexp.QuoteMeta(k.serviceHost()) + `, code ` + regexp.QuoteMeta(code) +
			`, expires \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\n$`)
		if !re.MatchString(out) {
			t.Fatalf("discovery=%v: %q", discovery, out)
		}
		if k.errb.String() != "" {
			t.Fatalf("stderr %q", k.errb)
		}
	}
}

// T-30, ErrPendingExpired: the request's time has run out.
func TestT30_KeyStatusExpired(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.SetExpiresIn(1)
	k.fileRequest()
	time.Sleep(1200 * time.Millisecond)
	if out := k.status(0); out != k.tokenAbsentLine()+"pending  "+k.pending()+": expired\n" {
		t.Fatalf("%q", out)
	}
}

// T-30, ErrPendingForeign: filed for another service URL.
func TestT30_KeyStatusForeign(t *testing.T) {
	k := newKeyRig(t, false)
	k.fileRequest()
	if out := k.status(0, "--url", "http://127.0.0.1:1"); out != k.tokenAbsentLine()+"pending  "+k.pending()+": filed for another service\n" {
		t.Fatalf("%q", out)
	}
}

// T-30, ErrPendingUnusable: exit 1 with keyclient's message, which names the
// file and says to remove it; the file's content is never echoed.
func TestT30_KeyStatusUnusable(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		k := newKeyRig(t, false)
		k.fileRequest()
		if err := os.Chmod(k.pending(), 0o640); err != nil {
			t.Fatal(err)
		}
		k.status(1)
		s := k.errb.String()
		if !strings.HasPrefix(s, "pwrwrdn: key request: the pending file "+k.pending()+" ") || !strings.Contains(s, "remove it") {
			t.Fatalf("%q", s)
		}
	})
	t.Run("content", func(t *testing.T) {
		k := newKeyRig(t, false)
		if err := os.MkdirAll(filepath.Dir(k.token), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(k.pending(), []byte(`{"x":"TOP-SECRET-XYZ" garbage`), 0o600); err != nil {
			t.Fatal(err)
		}
		k.status(1)
		s := k.errb.String()
		if !strings.HasPrefix(s, "pwrwrdn: key request: the pending file "+k.pending()+" ") || !strings.Contains(s, "remove it") ||
			strings.Contains(s, "TOP-SECRET") || strings.Contains(s, "garbage") {
			t.Fatalf("%q", s)
		}
	})
}

// No service URL configured: with no pending file there is nothing to check;
// with one, the status says plainly that it cannot tell.
func TestKeyStatusWithoutAServiceURL(t *testing.T) {
	k := newKeyRig(t, false)
	delete(k.getenv, "POWERWARDEN_URL")
	delete(k.getenv, "POWERWARDEN_KEY_REQUEST_URL")
	if out := k.status(0); out != k.tokenAbsentLine()+"pending  "+k.pending()+": none\n" {
		t.Fatalf("%q", out)
	}

	k2 := newKeyRig(t, false)
	k2.fileRequest()
	delete(k2.getenv, "POWERWARDEN_URL")
	delete(k2.getenv, "POWERWARDEN_KEY_REQUEST_URL")
	if out := k2.status(1); out != k2.tokenAbsentLine() {
		t.Fatalf("%q", out)
	}
	want := "pwrwrdn: cannot check the pending request without the powerwarden URL: powerwarden URL not set: use --url, POWERWARDEN_URL, or write it to ~/.config/powerwarden/url\n"
	if k2.errb.String() != want {
		t.Fatalf("%q", k2.errb)
	}
}

// ---- setup-token stays, deprecated ----

func TestSetupTokenDeprecated(t *testing.T) {
	c := commands["setup-token"]
	if !strings.HasPrefix(c.summary, "deprecated: ") || !strings.Contains(c.summary, "pwrwrdn key request") {
		t.Fatalf("summary %q", c.summary)
	}
	if k := commands["key"]; k == nil || k.subs["request"] == nil || k.subs["status"] == nil || k.summary == "" {
		t.Fatal("no key command")
	}
	r := newRig(t)
	rc := r.do("setup-token") // fails for want of an OpenBao path, after the warning
	lines := strings.Split(r.errb.String(), "\n")
	if rc != 1 || lines[0] != "pwrwrdn: setup-token is deprecated; use pwrwrdn key request" || !strings.Contains(lines[1], "OpenBao path not set") {
		t.Fatalf("rc=%d %q", rc, r.errb)
	}
}

// ---- a real second process ----

// subprocess runs this test binary as `pwrwrdn args...` with the rig's env.
func (k *keyRig) subprocess(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "PWRWRDN_TEST_REEXEC=1", "HOME="+k.home, "USER=tester",
		"POWERWARDEN_URL="+k.f.Srv.URL, "POWERWARDEN_KEY_REQUEST_URL="+k.getenv["POWERWARDEN_KEY_REQUEST_URL"],
		"POWERWARDEN_TOKEN_FILE=", "POWERWARDEN_CA_FILE=")
	return cmd
}

func (k *keyRig) runProcess(args ...string) (stderr string, rc int) {
	k.t.Helper()
	cmd := k.subprocess(args...)
	var eb syncBuf
	cmd.Stderr = &eb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return eb.String(), ee.ExitCode()
	}
	if err != nil {
		k.t.Fatal(err)
	}
	return eb.String(), 0
}

func approveURLAndCode(t *testing.T, notice string) (url, code string) {
	t.Helper()
	m := regexp.MustCompile(` at (\S+) and check that the code (\S+) matches\.`).FindStringSubmatch(notice)
	if m == nil {
		t.Fatalf("no URL and code in %q", notice)
	}
	return m[1], m[2]
}

// T-11: a second pwrwrdn process, with no terminal, resumes the request the
// first one filed: the same notice (marked resumed), no second filing.
func TestT11_SecondProcessResumesTheSameRequest(t *testing.T) {
	k := newKeyRig(t, false)
	e1, rc := k.runProcess("hosts")
	if rc != 75 {
		t.Fatalf("first rc=%d %s", rc, e1)
	}
	e2, rc := k.runProcess("claims")
	if rc != 75 {
		t.Fatalf("second rc=%d %s", rc, e2)
	}
	l1, l2 := strings.Split(strings.TrimSpace(e1), "\n"), strings.Split(strings.TrimSpace(e2), "\n")
	if len(l1) != 2 || len(l2) != 2 || l1[1] != pendingLine || l2[1] != pendingLine {
		t.Fatalf("%q / %q", l1, l2)
	}
	u1, c1 := approveURLAndCode(t, l1[0])
	u2, c2 := approveURLAndCode(t, l2[0])
	if u1 != u2 || c1 != c2 || !strings.HasPrefix(l2[0], "Resuming an earlier key request. "+approvePrefix) {
		t.Fatalf("notices differ:\n%s\n%s", l1[0], l2[0])
	}
	if n := len(k.p.Filed()); n != 1 {
		t.Fatalf("filed %d", n)
	}
}

// T-22: while an MCP process polls (holding the lock), a CLI process gets the
// same request at once from the pending file; nothing is filed twice.
func TestT22_MCPProcessThenCLIProcessShareTheRequest(t *testing.T) {
	k := newKeyRig(t, false)
	m := startMCPProcess(t, k)
	isErr, text := m.call(t, "list_hosts", nil)
	if !isErr || !strings.HasPrefix(text, "No powerwarden key on this machine yet. "+approvePrefix) {
		t.Fatalf("%v %q", isErr, text)
	}
	notice := strings.TrimSuffix(strings.TrimPrefix(text, "No powerwarden key on this machine yet. "), " Then retry.")
	e, rc := k.runProcess("hosts")
	if rc != 75 {
		t.Fatalf("rc=%d %s", rc, e)
	}
	lines := strings.Split(strings.TrimSpace(e), "\n")
	if len(lines) != 2 || lines[1] != pendingLine {
		t.Fatalf("%q", lines)
	}
	u1, c1 := approveURLAndCode(t, notice)
	u2, c2 := approveURLAndCode(t, lines[0])
	if u1 != u2 || c1 != c2 {
		t.Fatalf("notices differ:\n%s\n%s", notice, lines[0])
	}
	if n := len(k.p.Filed()); n != 1 {
		t.Fatalf("filed %d", n)
	}
	if strings.Contains(text, k.p.PollSecret()) || strings.Contains(e, k.p.PollSecret()) {
		t.Fatal("poll secret leaked")
	}
}

// With no $USER the name hint is the short host alone, never "@host".
func TestNameHintWithoutUserCLI(t *testing.T) {
	k := newKeyRig(t, false)
	k.getenv["USER"] = ""
	if rc := k.do("hosts"); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	if f := k.p.Filed(); len(f) != 1 || f[0].NameHint != shortHostForTest() {
		t.Fatalf("%+v want hint %q", f, shortHostForTest())
	}
}

// A first call that carries a body (claim) is replayed with the same body.
func TestRetryReplaysTheBodyOfTheFirstCall(t *testing.T) {
	k := newKeyRig(t, false)
	k.term = true
	k.p.Script(keyclienttest.Approved())
	var n atomic.Int32
	k.f.Respond = func(q pwfake.Req) (int, any, bool) {
		if q.Method == "PUT" && n.Add(1) == 1 {
			return 401, pwfake.Err("unauthorized", "no"), true
		}
		return 0, nil, false
	}
	if rc := k.do("claim", "--tags", "a", "--purpose", "replay"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	var bodies []string
	for _, q := range k.f.Requests() {
		if q.Method == "PUT" {
			bodies = append(bodies, string(q.Body))
		}
	}
	if len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] || !strings.Contains(bodies[1], "replay") {
		t.Fatalf("PUT bodies %q", bodies)
	}
}
