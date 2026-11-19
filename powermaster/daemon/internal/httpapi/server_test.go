package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	api "github.com/resurgentech/powerwarden/powermaster/api"

	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
	"github.com/resurgentech/powerwarden/powermaster/daemon/hw"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

const (
	pwToken    = "tok-powerwarden-0123456789"
	jaredToken = "tok-jared-abcdef"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	mgr *powermaster.Manager
	lab *hw.FakeLab
	st  *store.Store
}

func testInventory() *inventory.Inventory {
	inv := &inventory.Inventory{
		Hosts: []inventory.Host{
			{Hostname: "h1", IP: "10.9.9.1", User: "root",
				Ipmi:   inventory.Ipmi{IP: "10.9.8.1", Username: "ipmiadmin", Password: "s3cret-ipmi"},
				SSHKey: inventory.SSHKey{PrivateKey: "UFJJVktFWQ==", PublicKey: "ssh-ed25519 AAAApublic"},
				Pdu:    []inventory.PduRef{{Name: "p1", Outlet: 1}}},
			{Hostname: "h2", IP: "10.9.9.2", Ipmi: inventory.Ipmi{IP: "10.9.8.2", Username: "ipmiadmin", Password: "s3cret-ipmi"},
				Pdu: []inventory.PduRef{{Name: "p1", Outlet: 2}}},
			{Hostname: "locked1", IP: "10.9.9.3", Locked: true, Ipmi: inventory.Ipmi{IP: "10.9.8.3", Username: "ipmiadmin", Password: "s3cret-ipmi"},
				Pdu: []inventory.PduRef{{Name: "p1", Outlet: 3}}},
		},
		Pdus: map[string]inventory.Pdu{"p1": {IP: "10.9.7.1", Username: "pduadmin", Password: "s3cret-pdu", PduStyle: "kasa_hs300"}},
	}
	if err := inv.Validate(); err != nil {
		panic(err)
	}
	return inv
}

func newEnv(t *testing.T, ringSize int) *env {
	t.Helper()
	return newEnvKeyRequest(t, ringSize, "")
}

// newEnvKeyRequest is newEnv with Options.KeyRequestURL set.
func newEnvKeyRequest(t *testing.T, ringSize int, keyURL string) *env {
	t.Helper()
	cfg := powermaster.DefaultConfig()
	ms := func(n int) powermaster.Duration { return powermaster.Duration(time.Duration(n) * time.Millisecond) }
	cfg.Timings = powermaster.Timings{
		PduOutletWait: ms(100), IpmiPingWait: ms(100), ChassisOnWait: ms(100), PingSSHWait: ms(100),
		SSHShutdownWait: ms(50), PostSSHOffWait: ms(50), IpmiOffCommandWait: ms(50), FinalOffWait: ms(50),
		RebootChassisOffWait: ms(50), RebootPduSettle: ms(10), DeadlineSlack: ms(500),
	}
	cfg.Events.RingSize = ringSize
	inv := testInventory()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	lab := hw.NewFakeLab(inv)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := powermaster.New(powermaster.Options{Config: cfg, Inventory: inv, Store: st, HW: lab,
		Step: 2 * time.Millisecond, Resend: 20 * time.Millisecond, DisablePollers: true, Log: quiet,
		Hub: events.NewHub(ringSize)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	toks, err := NewTokens(map[string]string{"powerwarden": pwToken, "jared": jaredToken})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Manager: mgr, Tokens: toks, Log: quiet, Heartbeat: 50 * time.Millisecond, KeyRequestURL: keyURL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		cancel()
		mgr.Wait()
		st.Close()
	})
	return &env{t: t, srv: srv, mgr: mgr, lab: lab, st: st}
}

func (e *env) do(method, path, token, body string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) seed(host, state string) {
	e.t.Helper()
	if _, err := e.st.Seed(host, state, time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func wantStatus(t *testing.T, resp *http.Response, body []byte, code int) {
	t.Helper()
	if resp.StatusCode != code {
		t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, code, body)
	}
}

func TestEverySpecOperationHasARoute(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			want = append(want, method+" "+path)
		}
	}
	e := newEnv(t, 100)
	s, _ := New(Options{Manager: e.mgr, Tokens: mustTokens(t)})
	got := s.Routes()
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("routes differ from spec\nspec:\n%s\nmux:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	// And the mux really dispatches each of them (no 404/405 from routing).
	for _, op := range got {
		method, path, _ := strings.Cut(op, " ")
		path = strings.NewReplacer("{hostname}", "h1", "{id}", "999").Replace(path)
		if strings.HasSuffix(path, "/events") {
			continue // streams; covered below
		}
		resp, body := e.do(method, path, jaredToken, `{"action":"powerup"}`)
		if resp.StatusCode == http.StatusMethodNotAllowed || (resp.StatusCode == 404 && strings.Contains(string(body), "no such route")) {
			t.Fatalf("%s not routed: %d %s", op, resp.StatusCode, body)
		}
	}
}

func mustTokens(t *testing.T) *Tokens {
	toks, err := NewTokens(map[string]string{"x": "y"})
	if err != nil {
		t.Fatal(err)
	}
	return toks
}

func TestUnauthorized(t *testing.T) {
	e := newEnv(t, 100)
	r1, b1 := e.do("GET", "/v1/hosts", "", "")
	r2, b2 := e.do("GET", "/v1/hosts", "wrong-token", "")
	wantStatus(t, r1, b1, 401)
	wantStatus(t, r2, b2, 401)
	if string(b1) != string(b2) {
		t.Fatalf("401 bodies differ: %s vs %s", b1, b2)
	}
	if decode[api.Error](t, b1).Error != api.ErrorCodeUnauthorized {
		t.Fatalf("body = %s", b1)
	}
	r3, b3 := e.do("POST", "/v1/hosts/h2/power", "", `{"action":"powerup"}`)
	wantStatus(t, r3, b3, 401)
	// healthz needs no token and returns no inventory.
	r4, b4 := e.do("GET", "/v1/healthz", "", "")
	wantStatus(t, r4, b4, 200)
	if strings.Contains(string(b4), "h1") || decode[api.Healthz](t, b4).Status != api.Ok {
		t.Fatalf("healthz = %s", b4)
	}
}

func TestPowerRequestStatuses(t *testing.T) {
	e := newEnv(t, 100)
	e.seed("h2", store.StateSoftOff)
	e.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true }) // keeps the command running ~100ms

	// 202 and requested_by from the token name.
	r, b := e.do("POST", "/v1/hosts/h2/power", pwToken, `{"action":"powerup","reason":"demand d-17"}`)
	wantStatus(t, r, b, 202)
	c := decode[api.Command](t, b)
	if c.RequestedBy != "powerwarden" || c.Reason != "demand d-17" || c.Action != api.Powerup || c.State != api.Queued {
		t.Fatalf("command = %+v", c)
	}
	// Idempotent: same action returns the same row, 202.
	r, b = e.do("POST", "/v1/hosts/h2/power", jaredToken, `{"action":"powerup"}`)
	wantStatus(t, r, b, 202)
	if again := decode[api.Command](t, b); again.Id != c.Id || again.RequestedBy != "powerwarden" {
		t.Fatalf("idempotent enqueue returned %+v", again)
	}
	// A different action in flight: 409 command_in_flight.
	r, b = e.do("POST", "/v1/hosts/h2/power", jaredToken, `{"action":"reboot"}`)
	wantStatus(t, r, b, 409)
	if decode[api.Error](t, b).Error != api.ErrorCodeCommandInFlight {
		t.Fatalf("body = %s", b)
	}

	// 200 already there.
	e.seed("h1", store.StateOn)
	r, b = e.do("POST", "/v1/hosts/h1/power", jaredToken, `{"action":"powerup"}`)
	wantStatus(t, r, b, 200)
	if got := decode[api.Command](t, b); got.State != api.Succeeded || got.RequestedBy != "jared" {
		t.Fatalf("already-there = %+v", got)
	}

	// powercut: 409 locked without force; 200 already there from cold_off.
	r, b = e.do("POST", "/v1/hosts/locked1/power", jaredToken, `{"action":"powercut"}`)
	wantStatus(t, r, b, 409)
	if decode[api.Error](t, b).Error != api.ErrorCodeHostLocked {
		t.Fatalf("body = %s", b)
	}
	e.seed("locked1", store.StateColdOff)
	r, b = e.do("POST", "/v1/hosts/locked1/power?force=true", jaredToken, `{"action":"powercut"}`)
	wantStatus(t, r, b, 200)
	if got := decode[api.Command](t, b); got.State != api.Succeeded || got.Action != api.Powercut {
		t.Fatalf("powercut already-there = %+v", got)
	}

	// Locked: 409 without force, 202 with.
	r, b = e.do("POST", "/v1/hosts/locked1/power", jaredToken, `{"action":"reboot"}`)
	wantStatus(t, r, b, 409)
	if decode[api.Error](t, b).Error != api.ErrorCodeHostLocked {
		t.Fatalf("body = %s", b)
	}
	r, b = e.do("POST", "/v1/hosts/locked1/power?force=true", jaredToken, `{"action":"reboot"}`)
	wantStatus(t, r, b, 202)
	if got := decode[api.Command](t, b); !got.Force {
		t.Fatalf("forced = %+v", got)
	}

	// 404 and 400s.
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/v1/hosts/nope/power", `{"action":"powerup"}`, 404},
		{"/v1/hosts/h1/power", `{"action":"powerup","extra":1}`, 400},
		{"/v1/hosts/h1/power", `{"action":"explode"}`, 400},
		{"/v1/hosts/h1/power", `{"action":"powerup","reason":"` + strings.Repeat("x", 257) + `"}`, 400},
		{"/v1/hosts/h1/power?force=maybe", `{"action":"powerup"}`, 400},
		{"/v1/hosts/h1/power", `not json`, 400},
	} {
		r, b := e.do("POST", tc.path, jaredToken, tc.body)
		wantStatus(t, r, b, tc.code)
	}

	// powercut: 202 accepted from on.
	r, b = e.do("POST", "/v1/hosts/h1/power", jaredToken, `{"action":"powercut"}`)
	wantStatus(t, r, b, 202)
	if got := decode[api.Command](t, b); got.Action != api.Powercut {
		t.Fatalf("powercut accepted = %+v", got)
	}

	// Commands routes.
	r, b = e.do("GET", "/v1/commands/"+itoa(c.Id), jaredToken, "")
	wantStatus(t, r, b, 200)
	r, b = e.do("GET", "/v1/commands?hostname=h2", jaredToken, "")
	wantStatus(t, r, b, 200)
	if list := decode[[]api.Command](t, b); len(list) != 1 || list[0].Id != c.Id {
		t.Fatalf("list = %s", b)
	}
	r, b = e.do("GET", "/v1/commands?since=yesterday", jaredToken, "")
	wantStatus(t, r, b, 400)
	r, b = e.do("GET", "/v1/commands/123456", jaredToken, "")
	wantStatus(t, r, b, 404)
	r, b = e.do("GET", "/v1/stats", jaredToken, "")
	wantStatus(t, r, b, 200)
	if st := decode[api.Stats](t, b); st.LastCommandAt == nil {
		t.Fatalf("stats = %s", b)
	}
}

func itoa(n int64) string { return jsonNum(n) }

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

var secrets = []string{"10.9.", "s3cret", "ipmiadmin", "pduadmin", "UFJJVktFWQ", "AAAApublic", "\"ip\"", "password", "sshkey", pwToken, jaredToken}

func TestHostJSONAndEventsHaveNoSecrets(t *testing.T) {
	e := newEnv(t, 100)
	e.lab.Set("h1", func(f *hw.FakeHost) { f.Chassis = true })
	e.seed("h2", store.StateSoftOff)
	e.do("POST", "/v1/hosts/h2/power", jaredToken, `{"action":"powerup"}`)
	time.Sleep(300 * time.Millisecond)

	_, list := e.do("GET", "/v1/hosts", jaredToken, "")
	_, one := e.do("GET", "/v1/hosts/h1", jaredToken, "")
	var hosts []api.Host
	if err := json.Unmarshal(list, &hosts); err != nil || len(hosts) != 3 {
		t.Fatalf("list must be a bare array of 3 hosts: %s", list)
	}
	d := decode[api.HostDetail](t, one)
	for _, h := range hosts {
		if h.StateSince.IsZero() {
			t.Fatalf("list host %s has no state_since: %s", h.Hostname, list)
		}
	}
	if d.StateSince.IsZero() {
		t.Fatalf("detail has no state_since: %s", one)
	}
	if strings.Contains(string(list), "budgets") {
		t.Fatal("list must omit budgets")
	}
	if d.Budgets.Powerup == "" || d.Budgets.Reboot == "" || d.Budgets.Powercut == "" {
		t.Fatalf("detail = %s", one)
	}
	// Collect events from the ring (resume from 0).
	evs := readEvents(t, e, "0", 200*time.Millisecond)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	all := string(list) + string(one)
	for _, ev := range evs {
		all += ev.data
		if ev.event == "host" {
			if he := decode[api.HostEvent](t, []byte(ev.data)); he.StateSince.IsZero() {
				t.Fatalf("host event has no state_since: %s", ev.data)
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Fatalf("leaked %q", s)
		}
	}
}

type sse struct {
	id, event, data string
}

// readEvents opens /v1/events (with Last-Event-ID if non-empty) and collects
// events until idle for the given duration.
func readEvents(t *testing.T, e *env, lastID string, idle time.Duration) []sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+jaredToken)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events status %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan sse, 1000)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var cur sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.event != "" {
					ch <- cur
				}
				cur = sse{}
			case strings.HasPrefix(line, "id: "):
				cur.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.data = line[6:]
			}
		}
	}()
	var out []sse
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(idle):
			return out
		}
	}
}

func TestEventsResumeAndResync(t *testing.T) {
	e := newEnv(t, 5)
	hub := e.mgr.Hub()
	for i := 0; i < 8; i++ {
		hub.Publish(events.TypeHost, map[string]any{"hostname": "h2", "n": i})
	}
	// Ring holds 4..8; resume after 5 -> 6, 7, 8.
	evs := readEvents(t, e, "5", 150*time.Millisecond)
	if len(evs) != 3 || evs[0].id != "6" || evs[2].id != "8" {
		t.Fatalf("resume = %+v", evs)
	}
	for _, ev := range evs {
		m := decode[map[string]any](t, []byte(ev.data))
		if m["type"] != ev.event || jsonNum(int64(m["seq"].(float64))) != ev.id {
			t.Fatalf("type/seq mismatch: %+v", ev)
		}
	}
	// Too old -> resync.
	evs = readEvents(t, e, "1", 150*time.Millisecond)
	if len(evs) == 0 || evs[0].event != "resync" {
		t.Fatalf("want resync first, got %+v", evs)
	}
	if r := decode[api.ResyncEvent](t, []byte(evs[0].data)); r.Type != api.Resync || r.Seq != 8 {
		t.Fatalf("resync = %s", evs[0].data)
	}
	// Bad header -> 400.
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+jaredToken)
	req.Header.Set("Last-Event-ID", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad Last-Event-ID status = %d", resp.StatusCode)
	}
}

func TestEventsLiveWithinOneSecondAndHeartbeat(t *testing.T) {
	e := newEnv(t, 100)
	e.seed("h2", store.StateSoftOff)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+jaredToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	start := time.Now()
	e.do("POST", "/v1/hosts/h2/power", jaredToken, `{"action":"powerup"}`)
	sawCommand, sawHeartbeat := false, false
	timeout := time.After(time.Second)
	for !(sawCommand && sawHeartbeat) {
		select {
		case l := <-lines:
			if l == "event: command" && !sawCommand {
				sawCommand = true
				if time.Since(start) > time.Second {
					t.Fatal("event later than 1s")
				}
			}
			if l == ": heartbeat" {
				sawHeartbeat = true
			}
		case <-timeout:
			t.Fatalf("command=%v heartbeat=%v", sawCommand, sawHeartbeat)
		}
	}
}

func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens.yaml")
	os.WriteFile(p, []byte("powerwarden: abc\njared: def\n"), 0o600)
	toks, err := LoadTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := toks.Lookup("def"); !ok || n != "jared" {
		t.Fatalf("lookup = %q %v", n, ok)
	}
	if _, ok := toks.Lookup(""); ok {
		t.Fatal("empty token matched")
	}
	if _, ok := toks.Lookup("abcd"); ok {
		t.Fatal("wrong token matched")
	}
	os.Chmod(p, 0o644)
	if _, err := LoadTokens(p); err == nil {
		t.Fatal("want error for group/other-readable tokens file")
	}
	os.WriteFile(filepath.Join(dir, "e.yaml"), []byte("{}\n"), 0o600)
	if _, err := LoadTokens(filepath.Join(dir, "e.yaml")); err == nil {
		t.Fatal("want fail-closed error for no tokens")
	}
	if _, err := NewTokens(map[string]string{"a": "same", "b": "same"}); err == nil {
		t.Fatal("want duplicate token error")
	}
}
