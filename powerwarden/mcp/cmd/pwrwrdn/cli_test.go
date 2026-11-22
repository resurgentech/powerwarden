package main

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rig is a fake powerwarden plus an isolated HOME and token file.
type rig struct {
	t      *testing.T
	f      *pwfake.Fake
	home   string
	token  string
	out    *syncBuf
	errb   *syncBuf
	getenv map[string]string
	term   bool // what env.IsTerminal reports
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := pwfake.New()
	t.Cleanup(f.Srv.Close)
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "powerwarden")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, f: f, home: home, token: tok, out: &syncBuf{}, errb: &syncBuf{},
		getenv: map[string]string{"POWERWARDEN_URL": f.Srv.URL, "USER": "jared"}}
}

func (r *rig) env(args ...string) env {
	return env{Args: args, Getenv: func(k string) string { return r.getenv[k] }, Home: r.home,
		Stdin: io.NopCloser(strings.NewReader("")), Stdout: nopWC{r.out}, Stderr: r.errb,
		IsTerminal: func() bool { return r.term }}
}

func (r *rig) do(args ...string) int {
	return dispatch(context.Background(), r.env(args...))
}

// bind makes every new claim ready on box1.labtwo with the given host state.
func (r *rig) bind(hs pw.HostState) {
	r.f.OnCreate = func(c *pw.Claim) {
		h := "box1.labtwo"
		c.State, c.Detail, c.Hostname, c.HostState, c.Version = pw.Ready, "", &h, &hs, 2
	}
}

func (r *rig) putBody() map[string]any {
	r.t.Helper()
	for _, q := range r.f.Requests() {
		if q.Method == "PUT" && strings.HasPrefix(q.Path, "/v1/claims/") {
			var m map[string]any
			if err := json.Unmarshal(q.Body, &m); err != nil {
				r.t.Fatal(err)
			}
			return m
		}
	}
	r.t.Fatal("no PUT claim recorded")
	return nil
}

func (r *rig) firstID() string {
	for _, q := range r.f.Requests() {
		if q.Method == "PUT" {
			return strings.TrimPrefix(q.Path, "/v1/claims/")
		}
	}
	r.t.Fatal("no PUT")
	return ""
}

func (r *rig) deletes() int {
	n := 0
	for _, q := range r.f.Requests() {
		if q.Method == "DELETE" {
			n++
		}
	}
	return n
}

func (r *rig) powers() int {
	n := 0
	for _, q := range r.f.Requests() {
		if q.Method == "POST" && strings.HasSuffix(q.Path, "/power") {
			n++
		}
	}
	return n
}

func TestRunExitCodePropagates(t *testing.T) {
	r := newRig(t)
	r.bind(pw.On)
	if rc := r.do("run", "--tags", "gpu", "--", "sh", "-c", "exit 3"); rc != 3 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	if r.deletes() != 1 {
		t.Fatalf("claim not released on command exit: %d deletes", r.deletes())
	}
}

func TestRunSetsHostAndClaimID(t *testing.T) {
	r := newRig(t)
	r.bind(pw.On)
	if rc := r.do("run", "--tags", "gpu", "--", "sh", "-c", "echo $POWERWARDEN_HOST $POWERWARDEN_CLAIM_ID"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	if !regexp.MustCompile(`^box1\.labtwo pwrwrdn-[0-9a-f]{12}\n$`).MatchString(r.out.String()) {
		t.Fatalf("stdout %q", r.out)
	}
}

func TestRunNeverDetachedAndLeased(t *testing.T) {
	r := newRig(t)
	r.bind(pw.On)
	if rc := r.do("run", "--tags", "gpu", "--mode", "reserve", "--max", "2h", "--purpose", "nightly", "--", "true"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	b := r.putBody()
	if _, has := b["detached"]; has {
		t.Fatalf("run sent detached: %v", b)
	}
	if b["mode"] != "reserve" || b["purpose"] != "nightly" || b["max_seconds"] != float64(7200) {
		t.Fatalf("body %v", b)
	}
	if s, _ := b["session"].(string); !regexp.MustCompile(`^s-[0-9a-f]{12}$`).MatchString(s) {
		t.Fatalf("session %v", b["session"])
	}
}

func TestRunDefaultPurpose(t *testing.T) {
	r := newRig(t)
	r.bind(pw.On)
	if rc := r.do("run", "--tags", "gpu", "--", "true"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	if p := r.putBody()["purpose"]; p != "pwrwrdn run: true" {
		t.Fatalf("purpose %v", p)
	}
}

func TestRunReleasesOnSIGTERM(t *testing.T) {
	r := newRig(t)
	r.bind(pw.On)
	sig := make(chan os.Signal, 1)
	e := r.env("run", "--tags", "gpu", "--", "sh", "-c", "echo up; exec sleep 30")
	e.Signals = sig
	done := make(chan int, 1)
	go func() { done <- dispatch(context.Background(), e) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.out.String(), "up") {
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sig <- syscallSIGTERM
	select {
	case rc := <-done:
		if rc != 143 {
			t.Fatalf("rc=%d stderr=%s", rc, r.errb)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not exit after SIGTERM")
	}
	if r.deletes() != 1 {
		t.Fatalf("claim not released on SIGTERM: %d deletes", r.deletes())
	}
}

func TestRunOnSendsPowerupAndWaits(t *testing.T) {
	r := newRig(t)
	r.bind(pw.SoftOff)
	var powerAt time.Time
	var mu sync.Mutex
	r.f.OnPower = func(c *pw.Claim, action string) {
		mu.Lock()
		powerAt = time.Now()
		mu.Unlock()
		id := c.Id
		go func() {
			time.Sleep(300 * time.Millisecond)
			c2, _ := r.f.Claim(id)
			on := pw.On
			c2.HostState, c2.Version = &on, c2.Version+1
			r.f.SetClaim(&c2)
		}()
	}
	if rc := r.do("run", "--mode", "dedicated", "--on", "--tags", "gpu", "--", "sh", "-c", "echo ran"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	mu.Lock()
	defer mu.Unlock()
	if powerAt.IsZero() || time.Since(powerAt) < 300*time.Millisecond {
		t.Fatalf("command ran before host_state became on (power at %v)", powerAt)
	}
	pr, _ := r.f.Last("POST", "/v1/claims/"+r.firstID()+"/power")
	if !strings.Contains(string(pr.Body), `"powerup"`) {
		t.Fatalf("power body %s", pr.Body)
	}
	if r.out.String() != "ran\n" {
		t.Fatalf("stdout %q", r.out)
	}
}

func TestRunDedicatedOffRunsAtOnce(t *testing.T) {
	r := newRig(t)
	r.bind(pw.SoftOff)
	if rc := r.do("run", "--mode", "dedicated", "--tags", "gpu", "--", "sh", "-c", "echo ran"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	if r.powers() != 0 || r.out.String() != "ran\n" {
		t.Fatalf("powers=%d stdout=%q", r.powers(), r.out)
	}
}

func TestRunFailedClaimExitsNonZeroWithoutRunning(t *testing.T) {
	r := newRig(t)
	r.f.OnCreate = func(c *pw.Claim) { c.State, c.Detail, c.Version = pw.Failed, "no_host", 2 }
	if rc := r.do("run", "--tags", "nope", "--", "sh", "-c", "echo ran"); rc == 0 {
		t.Fatalf("rc=0 stdout=%q", r.out)
	}
	if r.out.String() != "" || !strings.Contains(r.errb.String(), "no_host") || r.deletes() != 1 {
		t.Fatalf("stdout=%q stderr=%q deletes=%d", r.out, r.errb, r.deletes())
	}
}

func TestClaimIsDetached(t *testing.T) {
	r := newRig(t)
	if rc := r.do("claim", "--tags", "gpu", "--max", "4h"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	b := r.putBody()
	if b["detached"] != true {
		t.Fatalf("not detached: %v", b)
	}
	if _, has := b["lease_seconds"]; has {
		t.Fatalf("detached claim sent lease_seconds: %v", b)
	}
	if b["purpose"] != "pwrwrdn claim by jared@"+shortHostForTest() || b["max_seconds"] != float64(14400) {
		t.Fatalf("body %v", b)
	}
	out := r.out.String()
	if !regexp.MustCompile(`pwrwrdn-[0-9a-f]{12}`).MatchString(out) || !strings.Contains(out, "pending") || !strings.Contains(out, "expires_at") {
		t.Fatalf("stdout %q", out)
	}
	if r.deletes() != 0 {
		t.Fatal("claim released")
	}
}

func TestClaimWaitBlocksUntilReady(t *testing.T) {
	r := newRig(t)
	go func() {
		for r.f.Count("GET", "/v1/claims/"+firstOr(r)) < 2 {
			time.Sleep(10 * time.Millisecond)
		}
		id := r.firstID()
		c, _ := r.f.Claim(id)
		h, on := "box1.labtwo", pw.On
		c.State, c.Hostname, c.HostState, c.Version = pw.Ready, &h, &on, c.Version+1
		r.f.SetClaim(&c)
	}()
	start := time.Now()
	if rc := r.do("claim", "--tags", "gpu", "--wait"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	if r.f.Count("GET", "/v1/claims/"+r.firstID()) < 2 || time.Since(start) < 10*time.Millisecond {
		t.Fatal("did not block")
	}
	if !strings.Contains(r.out.String(), "box1.labtwo") {
		t.Fatalf("stdout %q", r.out)
	}
}

// firstOr returns the claim id once a PUT exists, else a placeholder.
func firstOr(r *rig) string {
	for _, q := range r.f.Requests() {
		if q.Method == "PUT" {
			return strings.TrimPrefix(q.Path, "/v1/claims/")
		}
	}
	return "-"
}

func TestClaimWaitFailedExitsNonZero(t *testing.T) {
	r := newRig(t)
	r.f.OnCreate = func(c *pw.Claim) { c.State, c.Detail, c.Version = pw.Failed, "no_host", 2 }
	if rc := r.do("claim", "--tags", "x", "--wait"); rc == 0 || !strings.Contains(r.errb.String(), "no_host") {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
}

func TestReleaseAndExtend(t *testing.T) {
	r := newRig(t)
	if rc := r.do("release", "pwrwrdn-abc"); rc != 0 || r.f.Count("DELETE", "/v1/claims/pwrwrdn-abc") != 1 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	r.f.SetClaim(&pw.Claim{Id: "pwrwrdn-abc", ExpiresAt: time.Now()})
	if rc := r.do("extend", "pwrwrdn-abc", "2h"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	q, _ := r.f.Last("POST", "/v1/claims/pwrwrdn-abc/extend")
	if !strings.Contains(string(q.Body), `"max_seconds":7200`) {
		t.Fatalf("body %s", q.Body)
	}
}

func TestPowerSuccessAndEnvDefault(t *testing.T) {
	r := newRig(t)
	r.f.SetClaim(&pw.Claim{Id: "c1"})
	if rc := r.do("power", "c1", "reboot"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	if out := r.out.String(); !strings.Contains(out, "reboot") || !strings.Contains(out, "box1.labtwo") {
		t.Fatalf("stdout %q", out)
	}
	r.getenv["POWERWARDEN_CLAIM_ID"] = "c1"
	if rc := r.do("power", "powerup"); rc != 0 {
		t.Fatalf("env default rc=%d %s", rc, r.errb)
	}
	if r.f.Count("POST", "/v1/claims/c1/power") != 2 {
		t.Fatal("expected two power posts")
	}
}

func TestPowerRefusalExitsNonZero(t *testing.T) {
	r := newRig(t)
	r.f.Respond = func(q pwfake.Req) (int, any, bool) {
		if strings.HasSuffix(q.Path, "/power") {
			return 409, pwfake.Err(pw.ErrorCodeHostShared, "host is shared"), true
		}
		return 0, nil, false
	}
	if rc := r.do("power", "c1", "powerdown"); rc == 0 || !strings.Contains(r.errb.String(), "host_shared") {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	if rc := r.do("power", "powerdown"); rc == 0 || !strings.Contains(r.errb.String(), "POWERWARDEN_CLAIM_ID") {
		t.Fatalf("no id: rc=%d stderr=%s", rc, r.errb)
	}
}

func TestPowerWaitOn(t *testing.T) {
	r := newRig(t)
	off := pw.SoftOff
	r.f.SetClaim(&pw.Claim{Id: "c1", Version: 1, HostState: &off})
	var at time.Time
	r.f.OnPower = func(c *pw.Claim, _ string) {
		at = time.Now()
		go func() {
			time.Sleep(300 * time.Millisecond)
			c2, _ := r.f.Claim("c1")
			on := pw.On
			c2.HostState, c2.Version = &on, 2
			r.f.SetClaim(&c2)
		}()
	}
	if rc := r.do("power", "c1", "powerup", "--wait-on", "10s"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	if time.Since(at) < 300*time.Millisecond {
		t.Fatal("did not wait for host_state on")
	}
	// a host that never comes on times out non-zero
	r2 := newRig(t)
	r2.f.SetClaim(&pw.Claim{Id: "c1", Version: 1, HostState: &off})
	if rc := r2.do("power", "c1", "powerup", "--wait-on", "300ms"); rc == 0 {
		t.Fatal("timeout returned 0")
	}
}

func fakeBao(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bao"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

const testBaoPath = "team/pw/user_tokens"

const baoOK = `[ "$1 $2 $3 $5" = "kv get -mount=kv team/pw/user_tokens" ] || { echo "bad args: $*" >&2; exit 9; }
case "$4" in
-field=jared@box) echo tok-from-bao ;;
*) echo "Field \"${4#-field=}\" not present in secret" >&2; exit 1 ;;
esac
`

func TestSetupTokenWritesPrivateFileAndRefusesOverwrite(t *testing.T) {
	fakeBao(t, baoOK)
	r := newRig(t)
	tok := filepath.Join(t.TempDir(), "cfg", "powerwarden", "token")
	if rc := r.do("setup-token", "--bao-path", testBaoPath, "--name", "jared@box", "--token-file", tok); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	fi, err := os.Stat(tok)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	di, _ := os.Stat(filepath.Dir(tok))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	if b, _ := os.ReadFile(tok); string(b) != "tok-from-bao\n" {
		t.Fatalf("content %q", b)
	}
	if !strings.Contains(r.out.String(), "jared@box") {
		t.Fatalf("owner not printed: %q", r.out)
	}
	if strings.Contains(r.out.String()+r.errb.String(), "tok-from-bao") {
		t.Fatal("token leaked to output")
	}
	// second run without --force refuses and keeps the file
	_ = os.WriteFile(tok, []byte("old\n"), 0o600)
	if rc := r.do("setup-token", "--bao-path", testBaoPath, "--name", "jared@box", "--token-file", tok); rc == 0 || !strings.Contains(r.errb.String(), "--force") {
		t.Fatalf("overwrite not refused: rc=%d %s", rc, r.errb)
	}
	if b, _ := os.ReadFile(tok); string(b) != "old\n" {
		t.Fatalf("file changed: %q", b)
	}
	if rc := r.do("setup-token", "--bao-path", testBaoPath, "--name", "jared@box", "--token-file", tok, "--force"); rc != 0 {
		t.Fatalf("force rc=%d %s", rc, r.errb)
	}
	if b, _ := os.ReadFile(tok); string(b) != "tok-from-bao\n" {
		t.Fatalf("not overwritten: %q", b)
	}
}

func TestSetupTokenDefaultNameAndMissingName(t *testing.T) {
	fakeBao(t, `echo "$4" > "$(dirname "$0")/field"; echo "Field \"${4#-field=}\" not present in secret" >&2; exit 1
`)
	r := newRig(t)
	tok := filepath.Join(t.TempDir(), "token")
	rc := r.do("setup-token", "--bao-path", testBaoPath, "--token-file", tok)
	want := "jared@" + shortHostForTest()
	if rc == 0 || !strings.Contains(r.errb.String(), want) || !strings.Contains(r.errb.String(), "store a user token at kv/"+testBaoPath+" under field "+want) {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
	if _, err := os.Stat(tok); err == nil {
		t.Fatal("token file written for a missing name")
	}
}

func TestSetupTokenBaoPathFromEnvAndMount(t *testing.T) {
	fakeBao(t, `[ "$1 $2 $3 $5" = "kv get -mount=secret env/path" ] || { echo "bad args: $*" >&2; exit 9; }
echo tok-env
`)
	r := newRig(t)
	r.getenv["PWRWRDN_BAO_PATH"] = "env/path"
	r.getenv["PWRWRDN_BAO_MOUNT"] = "secret"
	tok := filepath.Join(t.TempDir(), "token")
	if rc := r.do("setup-token", "--name", "jared@box", "--token-file", tok); rc != 0 {
		t.Fatalf("env: rc=%d %s", rc, r.errb)
	}
	// flags win over env
	fakeBao(t, `[ "$1 $2 $3 $5" = "kv get -mount=m2 flag/path" ] || { echo "bad args: $*" >&2; exit 9; }
echo tok-flag
`)
	if rc := r.do("setup-token", "--name", "jared@box", "--token-file", tok, "--force", "--bao-path", "flag/path", "--bao-mount", "m2"); rc != 0 {
		t.Fatalf("flag: rc=%d %s", rc, r.errb)
	}
	if b, _ := os.ReadFile(tok); string(b) != "tok-flag\n" {
		t.Fatalf("content %q", b)
	}
}

func TestSetupTokenUnsetBaoPathFails(t *testing.T) {
	fakeBao(t, "echo should-not-run >&2; exit 9\n")
	r := newRig(t)
	tok := filepath.Join(t.TempDir(), "token")
	rc := r.do("setup-token", "--name", "jared@box", "--token-file", tok)
	e := r.errb.String()
	if rc == 0 || !strings.Contains(e, "--bao-path") || !strings.Contains(e, "PWRWRDN_BAO_PATH") || strings.Contains(e, "should-not-run") {
		t.Fatalf("rc=%d stderr=%s", rc, e)
	}
	if _, err := os.Stat(tok); err == nil {
		t.Fatal("token file written")
	}
}

func hostsRequests(r *rig) int {
	n := 0
	for _, q := range r.f.Requests() {
		if q.Method == "GET" && q.Path == "/v1/hosts" {
			n++
		}
	}
	return n
}

func TestURLFlagBeatsEnvBeatsFile(t *testing.T) {
	// file points nowhere reachable, env points at the fake, flag at a second fake
	r := newRig(t)
	if err := os.WriteFile(filepath.Join(r.home, ".config", "powerwarden", "url"), []byte("http://127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := r.do("hosts"); rc != 0 || hostsRequests(r) != 1 {
		t.Fatalf("env over file: rc=%d %s", rc, r.errb)
	}
	r2 := newRig(t)
	r2.getenv["POWERWARDEN_URL"] = "http://127.0.0.1:1"
	if rc := r2.do("hosts", "--url", r.f.Srv.URL); rc != 0 || hostsRequests(r) != 2 {
		t.Fatalf("flag over env: rc=%d %s", rc, r2.errb)
	}
	// file alone
	r3 := newRig(t)
	delete(r3.getenv, "POWERWARDEN_URL")
	if err := os.WriteFile(filepath.Join(r3.home, ".config", "powerwarden", "url"), []byte(r3.f.Srv.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := r3.do("hosts"); rc != 0 || hostsRequests(r3) != 1 {
		t.Fatalf("file: rc=%d %s", rc, r3.errb)
	}
}

func TestURLNotSetNamesAllThreeSources(t *testing.T) {
	for _, args := range [][]string{{"hosts"}, {"claims"}, {"release", "x"}} {
		r := newRig(t)
		delete(r.getenv, "POWERWARDEN_URL")
		if rc := r.do(args...); rc == 0 ||
			!strings.Contains(r.errb.String(), "powerwarden URL not set: use --url, POWERWARDEN_URL, or write it to ~/.config/powerwarden/url") {
			t.Errorf("%v: rc=%d %s", args, rc, r.errb)
		}
	}
}

func TestSetupTokenURLWritesPrivateURLFile(t *testing.T) {
	fakeBao(t, baoOK)
	r := newRig(t)
	cfg := filepath.Join(t.TempDir(), "cfg", "powerwarden")
	tok := filepath.Join(cfg, "token")
	// the url file sits beside the token file
	if rc := r.do("setup-token", "--bao-path", testBaoPath, "--name", "jared@box", "--token-file", tok, "--url", "https://pw.example.com/"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	u := filepath.Join(cfg, "url")
	fi, err := os.Stat(u)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("url file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(u); string(b) != "https://pw.example.com\n" {
		t.Fatalf("content %q", b)
	}
	// an existing url file blocks a second run unless --force, and nothing is fetched first
	_ = os.Remove(tok)
	if rc := r.do("setup-token", "--bao-path", testBaoPath, "--name", "jared@box", "--token-file", tok, "--url", "https://other.example.com"); rc == 0 || !strings.Contains(r.errb.String(), "--force") {
		t.Fatalf("url overwrite not refused: rc=%d %s", rc, r.errb)
	}
	if b, _ := os.ReadFile(u); string(b) != "https://pw.example.com\n" {
		t.Fatalf("url file changed: %q", b)
	}
	if rc := r.do("setup-token", "--bao-path", testBaoPath, "--name", "jared@box", "--token-file", tok, "--url", "https://other.example.com", "--force"); rc != 0 {
		t.Fatalf("force rc=%d %s", rc, r.errb)
	}
	if b, _ := os.ReadFile(u); string(b) != "https://other.example.com\n" {
		t.Fatalf("not overwritten: %q", b)
	}
}

func TestGroupReadableTokenRefused(t *testing.T) {
	r := newRig(t)
	if err := os.Chmod(r.token, 0o640); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"hosts"}, {"claims"}, {"claim", "--tags", "a"}, {"run", "--tags", "a", "--", "true"}, {"release", "x"}} {
		r.errb = &syncBuf{}
		if rc := r.do(args...); rc != 1 || !strings.Contains(r.errb.String(), "must not be readable") {
			t.Fatalf("%v: rc=%d %s", args, rc, r.errb)
		}
	}
	if len(r.f.Requests()) != 0 {
		t.Fatal("contacted the server with a refused token")
	}
}

func TestHostsTable(t *testing.T) {
	r := newRig(t)
	soon := time.Now().Add(2*time.Hour + 30*time.Minute)
	r.f.Hosts = []pw.HostView{
		{Hostname: "box1.labtwo", Tags: []string{"gpu", "a100"}, NameTags: []string{"box1.labtwo", "box1"}, State: pw.On, JobsBlocked: true,
			Claims: []pw.HostClaim{{Id: "c1", Owner: "jared@box", Mode: pw.Dedicated, ExpiresAt: soon, State: pw.Ready}}},
		{Hostname: "box2.labtwo", Tags: nil, NameTags: []string{"box2.labtwo", "box2"}, State: pw.SoftOff},
	}
	if rc := r.do("hosts", "--tags", "gpu,a100"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	q, _ := r.f.Last("GET", "/v1/hosts")
	if v, _ := url.ParseQuery(q.Query["tags"][0]); v == nil || q.Query["tags"][0] != "gpu,a100" {
		t.Fatalf("query %v", q.Query)
	}
	out := r.out.String()
	for _, want := range []string{"HOST", "TAGS", "NAME TAGS", "STATE", "JOBS BLOCKED", "CLAIMS",
		"box1.labtwo", "gpu,a100", "box1.labtwo,box1", "on", "yes", "jared@box dedicated 2h29m", "soft_off", "-"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestClaimsTable(t *testing.T) {
	r := newRig(t)
	h := "box1.labtwo"
	lease := time.Now().Add(10 * time.Minute)
	r.f.SetClaim(&pw.Claim{Id: "c1", Owner: "jared@box", Mode: pw.Hold, State: pw.Ready, Hostname: &h, LeaseUntil: &lease, ExpiresAt: time.Now().Add(5*time.Hour + 30*time.Second)})
	r.f.SetClaim(&pw.Claim{Id: "c2", Owner: "ann@x", Mode: pw.Reserve, State: pw.Pending, Detail: "waiting for box1", Detached: true, ExpiresAt: time.Now().Add(time.Hour + 30*time.Second)})
	if rc := r.do("claims", "--mine"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	q, _ := r.f.Last("GET", "/v1/claims")
	if q.Query["mine"][0] != "true" {
		t.Fatalf("query %v", q.Query)
	}
	out := r.out.String()
	for _, want := range []string{"ID", "OWNER", "MODE", "HOST", "STATE", "LEASE", "CAP",
		"c1", "jared@box", "hold", "box1.labtwo", "ready", "9m", "5h0m",
		"c2", "ann@x", "reserve", "pending: waiting for box1", "detached", "1h0m"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
