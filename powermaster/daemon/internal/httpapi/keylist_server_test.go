package httpapi

// B5: the key list source, driven through the real handler (New with a real
// keylist.Source) against keyclient's fake list. Tests are named after
// keyclient's STATES rows (K-1, K-10, K-5/K-8, R-2, R-7); SR-4 and SR-7 name the
// shared rules that have no row.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
	"github.com/resurgentech/powerwarden/powermaster/daemon/hw"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/keysource"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

const (
	keyAlice = "pulled-a-key-0123456789abc"
	keyBob   = "pulled-b-key-0123456789abc"
	keyCarol = "pulled-c-key-0123456789abc"
)

type keysEnv struct {
	*env
	src *keylist.Source
	log *bytes.Buffer
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// keyPK is a personal_keys block pointing at list. Refresh is long so that only
// Start's first pull and explicit Pull calls happen.
func keyPK(t *testing.T, list *keyclienttest.List) powermaster.PersonalKeys {
	t.Helper()
	p := powermaster.DefaultConfig().PersonalKeys
	p.URL = list.URL()
	p.CacheFile = filepath.Join(t.TempDir(), "keys.cache")
	p.Refresh = powermaster.Duration(time.Hour)
	return p
}

// startKeysEnv builds a powermaster handler with a started key list. The
// context of Start ends with the test.
func startKeysEnv(t *testing.T, p powermaster.PersonalKeys) *keysEnv {
	t.Helper()
	cfg := powermaster.DefaultConfig()
	ms := func(n int) powermaster.Duration { return powermaster.Duration(time.Duration(n) * time.Millisecond) }
	cfg.Timings = powermaster.Timings{
		PduOutletWait: ms(100), IpmiPingWait: ms(100), ChassisOnWait: ms(100), PingSSHWait: ms(100),
		SSHShutdownWait: ms(50), PostSSHOffWait: ms(50), IpmiOffCommandWait: ms(50), FinalOffWait: ms(50),
		RebootChassisOffWait: ms(50), RebootPduSettle: ms(10), DeadlineSlack: ms(500),
	}
	inv := testInventory()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	lab := hw.NewFakeLab(inv)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := powermaster.New(powermaster.Options{Config: cfg, Inventory: inv, Store: st, HW: lab,
		Step: 2 * time.Millisecond, Resend: 20 * time.Millisecond, DisablePollers: true, Log: quiet,
		Hub: events.NewHub(100)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	toks, err := NewTokens(map[string]string{"powerwarden": pwToken, "operator": jaredToken})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, nil))
	src, err := keysource.New(p, PulledRoles, toks.TakenLocally, log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Manager: mgr, Tokens: toks, Log: quiet, Heartbeat: 50 * time.Millisecond, Keys: src})
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
	kctx, kcancel := context.WithCancel(context.Background())
	t.Cleanup(kcancel)
	if err := keysource.Start(kctx, src, log); err != nil {
		t.Fatal(err)
	}
	return &keysEnv{env: &env{t: t, srv: srv, mgr: mgr, lab: lab, st: st}, src: src, log: &buf}
}

func (k *keysEnv) waitProvider(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for k.src.Status().Origin != "provider" {
		if time.Now().After(deadline) {
			t.Fatalf("no good pull: %+v", k.src.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (k *keysEnv) want(t *testing.T, token string, code int) {
	t.Helper()
	resp, body := k.do("GET", "/v1/hosts", token, "")
	if resp.StatusCode != code {
		t.Fatalf("GET /v1/hosts with %q: %d %s, want %d", token, resp.StatusCode, body, code)
	}
}

// K-1: a usable cache at Start serves before the first pull.
func TestK1_CacheServesBeforeFirstPull(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	p := keyPK(t, list)
	first := startKeysEnv(t, p)
	first.waitProvider(t)
	first.want(t, keyAlice, 200)

	// Second process on the same cache: the pull is held, the cache is all there is.
	release := list.Block()
	before := list.Pulls()
	second := startKeysEnv(t, p)
	deadline := time.Now().Add(20 * time.Second)
	for list.Pulls() == before {
		if time.Now().After(deadline) {
			t.Fatal("first pull never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := second.src.Status(); st.Origin != "cache" || st.Entries != 1 {
		t.Fatalf("status before the pull answers: %+v", st)
	}
	second.want(t, keyAlice, 200) // served from the cache while the pull is held
	second.want(t, pwToken, 200)
	release()
}

// K-10: the provider is down; the cache keeps working, across a restart too.
func TestK10_ProviderDownCacheKeepsWorking(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	p := keyPK(t, list)
	first := startKeysEnv(t, p)
	first.waitProvider(t)

	list.FailWith(keyclienttest.Status(500, ""))
	if err := first.src.Pull(context.Background()); err == nil {
		t.Fatal("a 500 pull reported success")
	}
	first.want(t, keyAlice, 200)
	if st := first.src.Status(); st.LastError == nil || st.Origin != "provider" {
		t.Fatalf("status: %+v", st)
	}
	if r, body := first.do("GET", "/v1/healthz", "", ""); r.StatusCode != 200 {
		t.Fatalf("a down provider made healthz %d: %s", r.StatusCode, body)
	}

	// Restart with the provider still failing: the cache is what serves.
	second := startKeysEnv(t, p)
	second.want(t, keyAlice, 200)
	if st := second.src.Status(); st.Origin != "cache" {
		t.Fatalf("origin after restart: %+v", st)
	}
	if r, _ := second.do("GET", "/v1/healthz", "", ""); r.StatusCode != 200 {
		t.Fatalf("healthz after restart: %d", r.StatusCode)
	}
	second.want(t, "unknown-key-0123456789", 401)
}

// K-5, K-8: a good 200, then a 304; the key still works.
func TestK5K8_Good200Then304(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	r := startKeysEnv(t, keyPK(t, list))
	r.waitProvider(t)
	r.want(t, keyAlice, 200)
	if err := r.src.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := list.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d pulls, want 2", len(reqs))
	}
	if reqs[0].IfNoneMatch != "" {
		t.Fatalf("first pull sent If-None-Match %q", reqs[0].IfNoneMatch)
	}
	if reqs[1].IfNoneMatch == "" {
		t.Fatal("second pull sent no If-None-Match")
	}
	r.want(t, keyAlice, 200)
	if st := r.src.Status(); st.Entries != 1 || st.LastError != nil {
		t.Fatalf("status after 304: %+v", st)
	}
}

// A revoke drops the key at the next pull.
func TestSR8_RevokeDropsKeyAtNextPull(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	list.Add("pulled-b", "full", keyBob)
	p := keyPK(t, list)
	p.RefreshOnMiss = 0 // a miss must not pull: only the scheduled pull revokes
	r := startKeysEnv(t, p)
	r.waitProvider(t)
	r.want(t, keyAlice, 200)
	r.want(t, keyBob, 200)
	list.Remove("pulled-a")
	r.want(t, keyAlice, 200) // not yet pulled
	if err := r.src.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.want(t, keyAlice, 401)
	r.want(t, keyBob, 200)
}

// R-2: a key added after the last pull works on first use.
func TestR2_KeyAddedAfterLastPullWorksOnFirstUse(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	r := startKeysEnv(t, keyPK(t, list))
	r.waitProvider(t)
	before := list.Pulls()
	list.Add("pulled-b", "full", keyBob)
	r.want(t, keyBob, 200)
	if n := list.Pulls() - before; n != 1 {
		t.Fatalf("%d pulls for the miss, want 1", n)
	}
}

// R-2 off: refresh_on_miss 0 never pulls on a miss.
func TestR2_RefreshOnMissZeroNeverPulls(t *testing.T) {
	list := keyclienttest.NewList(t)
	p := keyPK(t, list)
	p.RefreshOnMiss = 0
	r := startKeysEnv(t, p)
	r.waitProvider(t)
	before := list.Pulls()
	list.Add("pulled-b", "full", keyBob)
	r.want(t, keyBob, 401)
	if n := list.Pulls() - before; n != 0 {
		t.Fatalf("%d pulls with refresh_on_miss 0", n)
	}
}

// R-7: a burst of unknown keys starts at most one pull per window.
func TestR7_BurstOfUnknownKeysOnePullPerWindow(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	p := keyPK(t, list)
	p.RefreshOnMiss = powermaster.Duration(time.Hour) // the window outlasts the test: no trailing pull
	r := startKeysEnv(t, p)
	r.waitProvider(t)
	before := list.Pulls()
	const n = 40
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest("GET", r.srv.URL+"/v1/hosts", nil)
			req.Header.Set("Authorization", fmt.Sprintf("Bearer unknown-key-%016d", i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 401 {
			t.Fatalf("unknown key %d: %d", i, c)
		}
	}
	if got := list.Pulls() - before; got != 1 {
		t.Fatalf("%d pulls for a burst of %d unknown keys, want 1", got, n)
	}
	// The window is closed now: another unknown key pulls nothing and answers 401 at once.
	r.want(t, "another-unknown-key-0123", 401)
	if got := list.Pulls() - before; got != 1 {
		t.Fatalf("%d pulls after the burst", got)
	}
}

// SR-4: a pulled entry whose name or hash is a tokens.yaml token is skipped, and
// the local token keeps working; an entry with a role this daemon does not know
// (agent is powerwarden's, not powermaster's) is skipped.
func TestSR4_LocalTokensWinAndUnknownRoleSkipped(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("operator", "full", keyBob)   // name clash: skipped
	list.Add("shadow", "full", pwToken)    // hash clash: skipped
	list.Add("weird", "agent", keyCarol)   // role powermaster does not know: skipped
	list.Add("pulled-a", "full", keyAlice) // good
	p := keyPK(t, list)
	p.RefreshOnMiss = 0
	r := startKeysEnv(t, p)
	r.waitProvider(t)
	if st := r.src.Status(); st.Entries != 1 {
		t.Fatalf("entries %d, want only pulled-a", st.Entries)
	}
	r.want(t, keyBob, 401)
	r.want(t, keyCarol, 401)
	r.want(t, keyAlice, 200)
	r.want(t, pwToken, 200)
	r.want(t, jaredToken, 200)
	// The local token's own name stays the caller: requested_by is "powerwarden".
	r.seed("h2", store.StateSoftOff)
	r.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	resp, b := r.do("POST", "/v1/hosts/h2/power", pwToken, `{"action":"powerup"}`)
	wantStatus(t, resp, b, 202)
	if c := decode[struct {
		RequestedBy string `json:"requested_by"`
	}](t, b); c.RequestedBy != "powerwarden" {
		t.Fatalf("requested_by %q", c.RequestedBy)
	}
}

// The pulled key's name is the caller: it becomes requested_by of a power request.
func TestSR5_PulledNameIsTheCaller(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("operator@desk", "full", keyAlice)
	r := startKeysEnv(t, keyPK(t, list))
	r.waitProvider(t)
	r.seed("h2", store.StateSoftOff)
	r.lab.Set("h2", func(f *hw.FakeHost) { f.SSHDown = true })
	resp, b := r.do("POST", "/v1/hosts/h2/power", keyAlice, `{"action":"powerup","reason":"test"}`)
	wantStatus(t, resp, b, 202)
	if c := decode[struct {
		RequestedBy string `json:"requested_by"`
	}](t, b); c.RequestedBy != "operator@desk" {
		t.Fatalf("requested_by %q, want operator@desk", c.RequestedBy)
	}
	// An empty bearer is never looked up; a miss is the existing 401 and message.
	before := list.Pulls()
	resp, b = r.do("GET", "/v1/hosts", "", "")
	wantStatus(t, resp, b, 401)
	req, _ := http.NewRequest("GET", r.srv.URL+"/v1/hosts", nil)
	req.Header.Set("Authorization", "Bearer    ")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 || list.Pulls() != before {
		t.Fatalf("blank bearer: %d, %d pulls", resp2.StatusCode, list.Pulls()-before)
	}
	resp, b = r.do("GET", "/v1/hosts", "nope-nope-nope-0123456789", "")
	wantStatus(t, resp, b, 401)
	if !strings.Contains(string(b), "missing or invalid bearer token") {
		t.Fatalf("401 message: %s", b)
	}
}

// stats carries personal_keys when url is set, with the D7 fields.
func TestSR7_StatsPersonalKeys(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", keyAlice)
	r := startKeysEnv(t, keyPK(t, list))
	r.waitProvider(t)
	if resp, _ := r.do("GET", "/v1/stats", "", ""); resp.StatusCode != 401 {
		t.Fatalf("stats unauthenticated: %d", resp.StatusCode)
	}
	resp, body := r.do("GET", "/v1/stats", keyAlice, "")
	wantStatus(t, resp, body, 200)
	var s struct {
		PersonalKeys map[string]any `json:"personal_keys"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	pk := s.PersonalKeys
	if pk == nil || pk["origin"] != "provider" || pk["entries"] != float64(1) || pk["cache_failing"] != false {
		t.Fatalf("personal_keys: %v", pk)
	}
	for _, k := range []string{"age_seconds", "list_time", "last_success"} {
		if _, ok := pk[k]; !ok {
			t.Errorf("missing %s: %v", k, pk)
		}
	}
	if _, ok := pk["last_error"]; ok {
		t.Errorf("last_error with no error: %v", pk)
	}
	list.FailWith(keyclienttest.Status(500, ""))
	_ = r.src.Pull(context.Background())
	_, body = r.do("GET", "/v1/stats", keyAlice, "")
	if !strings.Contains(string(body), `"last_error"`) {
		t.Fatalf("no last_error: %s", body)
	}
	for _, secret := range []string{keyAlice, hexOf(keyAlice), list.URL()} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("stats leaks %q: %s", secret, body)
		}
	}
}

// No url: stats has no personal_keys field, as before (newEnv builds no source).
func TestSR7_NoURLNoStatsField(t *testing.T) {
	e := newEnv(t, 100)
	resp, body := e.do("GET", "/v1/stats", pwToken, "")
	wantStatus(t, resp, body, 200)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["personal_keys"]; ok {
		t.Fatalf("personal_keys present without url: %s", body)
	}
}
