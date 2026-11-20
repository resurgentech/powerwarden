package main

// B5: the key list source, driven through the real handler that build wires,
// against keyclient's fake list. Tests are named after keyclient's STATES rows
// (K-1, K-10, K-5/K-8, R-2, R-7); SR-4 and SR-8 name the shared rules that have
// no row.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
)

const (
	localFull  = "hourchd-token-0123456789"
	localAgent = "local-agent-token-0123456"
	keyAlice   = "alice-pulled-key-0123456789"
	keyBob     = "bob-pulled-key-0123456789ab"
	keyCarol   = "carol-pulled-key-012345678"
)

// keyCfg is a wired config with the key list on, pointing at list. Refresh is
// long so that only Start's first pull and explicit Pull calls happen.
func keyCfg(t *testing.T, list *keyclienttest.List) config.Config {
	t.Helper()
	cfg := proxyCfg(t, 0o600)
	dir := filepath.Dir(cfg.StatePath)
	cfg.TokensFile = write(t, dir, "tokens.yaml",
		"hourchd: "+localFull+"\nlocalagent: {token: "+localAgent+", role: agent}\n")
	cfg.HostTags = map[string][]string{"veritech01.labtwo": {"kernel"}}
	cfg.PersonalKeys.URL = list.URL()
	cfg.PersonalKeys.CacheFile = filepath.Join(t.TempDir(), "keys.cache")
	cfg.PersonalKeys.Refresh = time.Hour
	return cfg
}

type running struct {
	d   *daemon
	log *bytes.Buffer
}

// startKeys builds the daemon and starts its key list with a context that ends
// with the test.
func startKeys(t *testing.T, cfg config.Config) *running {
	t.Helper()
	var buf bytes.Buffer
	d, err := build(cfg, slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, nil)), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(d.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return &running{d: d, log: &buf}
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

func (r *running) call(method, path, token, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.d.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// waitProvider waits for the list to come from a pull (Start's first one).
func (r *running) waitProvider(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for r.d.keys.Status().Origin != "provider" {
		if time.Now().After(deadline) {
			t.Fatalf("no good pull: %+v", r.d.keys.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *running) want(t *testing.T, token string, code int) {
	t.Helper()
	if got, body := r.call("GET", "/v1/demands", token, ""); got != code {
		t.Fatalf("GET /v1/demands with %q: %d %s, want %d", token, got, body, code)
	}
}

// K-1: a usable cache at Start serves before the first pull.
func TestK1_CacheServesBeforeFirstPull(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	cfg := keyCfg(t, list)
	first := startKeys(t, cfg)
	first.waitProvider(t)
	first.want(t, keyAlice, 200)
	first.d.Close()

	// Second process: the pull is held, the cache is all there is.
	release := list.Block()
	before := list.Pulls()
	cfg2 := cfg
	second := startKeys(t, cfg2)
	deadline := time.Now().Add(20 * time.Second)
	for list.Pulls() == before {
		if time.Now().After(deadline) {
			t.Fatal("first pull never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := second.d.keys.Status(); st.Origin != "cache" || st.Entries != 1 {
		t.Fatalf("status before the pull answers: %+v", st)
	}
	second.want(t, keyAlice, 200) // served from the cache while the pull is held
	second.want(t, localFull, 200)
	release()
	// The log line is taken right after Start: origin=cache.
	if !strings.Contains(second.log.String(), `"msg":"personal_keys: list origin=cache entries=1 age=`) {
		t.Fatalf("log: %s", second.log)
	}
}

// K-10: the provider is down; the cache keeps working, across a restart too.
func TestK10_ProviderDownCacheKeepsWorking(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	cfg := keyCfg(t, list)
	first := startKeys(t, cfg)
	first.waitProvider(t)

	list.FailWith(keyclienttest.Status(500, ""))
	if err := first.d.keys.Pull(context.Background()); err == nil {
		t.Fatal("a 500 pull reported success")
	}
	first.want(t, keyAlice, 200)
	if st := first.d.keys.Status(); st.LastError == nil || st.Origin != "provider" {
		t.Fatalf("status: %+v", st)
	}
	if c, body := first.call("GET", "/v1/healthz", "", ""); c != 200 {
		t.Fatalf("a down provider made healthz %d: %s", c, body)
	}
	first.d.Close()

	// Restart with the provider still failing: the cache is what serves.
	second := startKeys(t, cfg)
	second.want(t, keyAlice, 200)
	if st := second.d.keys.Status(); st.Origin != "cache" {
		t.Fatalf("origin after restart: %+v", st)
	}
	if c, _ := second.call("GET", "/v1/healthz", "", ""); c != 200 {
		t.Fatalf("healthz after restart: %d", c)
	}
	second.want(t, "unknown-key-0123456789", 401)
}

// K-5, K-8: a good 200, then a 304; the key still works.
func TestK5K8_Good200Then304(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	r := startKeys(t, keyCfg(t, list))
	r.waitProvider(t)
	r.want(t, keyAlice, 200)
	if err := r.d.keys.Pull(context.Background()); err != nil {
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
	if st := r.d.keys.Status(); st.Entries != 1 || st.LastError != nil {
		t.Fatalf("status after 304: %+v", st)
	}
}

// A revoke drops the key at the next pull.
func TestSR8_RevokeDropsKeyAtNextPull(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	list.Add("bob@example", "agent", keyBob)
	cfg := keyCfg(t, list)
	cfg.PersonalKeys.RefreshOnMiss = 0 // a miss must not pull: only the scheduled pull revokes
	r := startKeys(t, cfg)
	r.waitProvider(t)
	r.want(t, keyAlice, 200)
	r.want(t, keyBob, 200)
	list.Remove("alice@example")
	r.want(t, keyAlice, 200) // not yet pulled
	if err := r.d.keys.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.want(t, keyAlice, 401)
	r.want(t, keyBob, 200)
}

// R-2: a key added after the last pull works on first use.
func TestR2_KeyAddedAfterLastPullWorksOnFirstUse(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	r := startKeys(t, keyCfg(t, list))
	r.waitProvider(t)
	before := list.Pulls()
	list.Add("bob@example", "agent", keyBob)
	r.want(t, keyBob, 200)
	if n := list.Pulls() - before; n != 1 {
		t.Fatalf("%d pulls for the miss, want 1", n)
	}
}

// R-2 off: refresh_on_miss 0 never pulls on a miss.
func TestR2_RefreshOnMissZeroNeverPulls(t *testing.T) {
	list := keyclienttest.NewList(t)
	cfg := keyCfg(t, list)
	cfg.PersonalKeys.RefreshOnMiss = 0
	r := startKeys(t, cfg)
	r.waitProvider(t)
	before := list.Pulls()
	list.Add("bob@example", "agent", keyBob)
	r.want(t, keyBob, 401)
	if n := list.Pulls() - before; n != 0 {
		t.Fatalf("%d pulls with refresh_on_miss 0", n)
	}
}

// R-7: a burst of unknown keys starts at most one pull per window.
func TestR7_BurstOfUnknownKeysOnePullPerWindow(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	cfg := keyCfg(t, list)
	cfg.PersonalKeys.RefreshOnMiss = time.Hour // the window outlasts the test: no trailing pull
	r := startKeys(t, cfg)
	r.waitProvider(t)
	before := list.Pulls()
	const n = 40
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = r.call("GET", "/v1/demands", fmt.Sprintf("unknown-key-%016d", i), "")
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

// SR-4: a pulled entry whose name or hash is a tokens.yaml token is skipped,
// and the local token keeps its own role; an entry with an unknown role is skipped.
func TestSR4_LocalTokensWinAndUnknownRoleSkipped(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("localagent", "full", keyBob)       // name clash: skipped
	list.Add("shadow", "full", localAgent)       // hash clash: skipped, localagent stays agent
	list.Add("weird", "root", keyCarol)          // unknown role: skipped
	list.Add("alice@example", "agent", keyAlice) // good
	cfg := keyCfg(t, list)
	cfg.PersonalKeys.RefreshOnMiss = 0
	r := startKeys(t, cfg)
	r.waitProvider(t)
	if st := r.d.keys.Status(); st.Entries != 1 {
		t.Fatalf("entries %d, want only alice", st.Entries)
	}
	r.want(t, keyBob, 401)
	r.want(t, keyCarol, 401)
	r.want(t, keyAlice, 200)
	// The local agent token keeps role agent: a full route is 403, not 200.
	if c, body := r.call("PUT", "/v1/demands/hourchd:d1", localAgent, `{"tags":["kernel"],"source":"hourchd"}`); c != 403 {
		t.Fatalf("local agent token on a full route: %d %s", c, body)
	}
	r.want(t, localAgent, 200)
	r.want(t, localFull, 200)
}

// powerwarden: a pulled agent is held to the route table, and is not an operator.
func TestSR4_PulledAgentRouteTableAndNotOperator(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	list.Add("bob@example", "agent", keyBob)
	list.Add("carol@example", "full", keyCarol)
	r := startKeys(t, keyCfg(t, list))
	r.waitProvider(t)

	// agent route 200, full route 403 with the existing message.
	r.want(t, keyAlice, 200)
	c, body := r.call("PUT", "/v1/demands/hourchd:d1", keyAlice, `{"tags":["kernel"],"source":"hourchd"}`)
	if c != 403 || !strings.Contains(body, "token role agent may not call this operation") {
		t.Fatalf("pulled agent on a full route: %d %s", c, body)
	}
	// A pulled full on the same route is let through.
	c, body = r.call("PUT", "/v1/demands/hourchd:d1", keyCarol, `{"tags":["kernel"],"source":"hourchd"}`)
	if c != 200 {
		t.Fatalf("pulled full on a full route: %d %s", c, body)
	}

	// The pulled key's name is the claim owner.
	c, body = r.call("PUT", "/v1/claims/c1", keyAlice, `{"tags":["kernel"],"purpose":"p"}`)
	if c != 200 {
		t.Fatalf("claim put: %d %s", c, body)
	}
	var claim struct{ Owner string }
	if err := json.Unmarshal([]byte(body), &claim); err != nil || claim.Owner != "alice@example" {
		t.Fatalf("owner %q (%v): %s", claim.Owner, err, body)
	}
	c, body = r.call("GET", "/v1/claims?mine=true", keyAlice, "")
	if c != 200 || !strings.Contains(body, `"owner":"alice@example"`) {
		t.Fatalf("mine: %d %s", c, body)
	}
	// Another pulled agent is not an operator: it cannot release alice's claim.
	if c, body = r.call("DELETE", "/v1/claims/c1", keyBob, ""); c != 403 {
		t.Fatalf("pulled agent released another's claim: %d %s", c, body)
	}
	// A pulled full is an operator and can.
	if c, body = r.call("DELETE", "/v1/claims/c1", keyCarol, ""); c != 204 {
		t.Fatalf("pulled full released a claim: %d %s", c, body)
	}
}

// allow_insecure_http: false refuses a non-loopback http url at start; true allows it.
func TestSR8_AllowInsecureHTTPAtBuild(t *testing.T) {
	const hint = "; set personal_keys.allow_insecure_http: true only for a network path you trust"
	cfg := proxyCfg(t, 0o600)
	cfg.PersonalKeys.URL = "http://keys.example/v1/lists/x"
	cfg.PersonalKeys.CacheFile = filepath.Join(t.TempDir(), "keys.cache")
	d, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil {
		d.Close()
		t.Fatal("plain http to a non-loopback host accepted")
	}
	if !strings.HasPrefix(err.Error(), "personal_keys: ") || !strings.HasSuffix(err.Error(), hint) {
		t.Fatalf("error: %v", err)
	}
	cfg.PersonalKeys.AllowInsecureHTTP = true
	d, err = build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	d.Close()
}

// serve itself refuses it, with the message on the way out.
func TestSR8_ServeRefusesInsecureHTTP(t *testing.T) {
	cfg := proxyCfg(t, 0o600)
	dir := filepath.Dir(cfg.StatePath)
	cfgFile := write(t, dir, "config.yaml", fmt.Sprintf(`listen: "127.0.0.1:0"
state_path: %q
tokens_file: %q
powermaster:
  url: %s
  token_file: %q
ui:
  enabled: false
personal_keys:
  url: http://keys.example/v1/lists/x
  cache_file: %q
`, cfg.StatePath, cfg.TokensFile, cfg.Powermaster.URL, cfg.Powermaster.TokenFile, filepath.Join(dir, "keys.cache")))
	err := serve(context.Background(), cfgFile)
	if err == nil || !strings.HasPrefix(err.Error(), "personal_keys: ") ||
		!strings.HasSuffix(err.Error(), "; set personal_keys.allow_insecure_http: true only for a network path you trust") {
		t.Fatalf("serve: %v", err)
	}
}

// The SR-6 log line: one Info record, the whole text is the message, no attributes.
func TestSR6_LogLineAfterStart(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	list.Block() // hold the first pull: the line is taken right after Start, before any list is live
	r := startKeys(t, keyCfg(t, list))
	var found []map[string]any
	for _, line := range bytes.Split(r.log.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil {
			if m, _ := rec["msg"].(string); strings.HasPrefix(m, "personal_keys:") {
				found = append(found, rec)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d personal_keys lines: %s", len(found), r.log)
	}
	rec := found[0]
	if len(rec) != 3 || rec["level"] != "INFO" { // time, level, msg and nothing else
		t.Fatalf("record has attributes or wrong level: %v", rec)
	}
	if !regexp.MustCompile(`^personal_keys: list origin=none entries=0 age=0s$`).MatchString(rec["msg"].(string)) {
		t.Fatalf("msg %q", rec["msg"])
	}
}

// No url: nothing built, nothing logged, no stats field, behaviour as before.
func TestSR7_NoURLNoStatsFieldNoLog(t *testing.T) {
	cfg := proxyCfg(t, 0o600)
	var buf bytes.Buffer
	d, err := build(cfg, slog.New(slog.NewJSONHandler(&buf, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.keys != nil {
		t.Fatal("a source was built without a url")
	}
	if err := d.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "personal_keys") {
		t.Fatalf("logged: %s", buf.String())
	}
	r := &running{d: d}
	c, body := r.call("GET", "/v1/stats", "hourchd-token-0123456789", "")
	if c != 200 {
		t.Fatalf("stats %d: %s", c, body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["personal_keys"]; ok {
		t.Fatalf("personal_keys present without url: %s", body)
	}
}

// stats carries personal_keys when url is set, with the D7 fields.
func TestSR7_StatsPersonalKeys(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("alice@example", "agent", keyAlice)
	r := startKeys(t, keyCfg(t, list))
	r.waitProvider(t)
	if c, _ := r.call("GET", "/v1/stats", "", ""); c != 401 {
		t.Fatalf("stats unauthenticated: %d", c)
	}
	// An agent (pulled) may read stats.
	c, body := r.call("GET", "/v1/stats", keyAlice, "")
	if c != 200 {
		t.Fatalf("stats %d: %s", c, body)
	}
	var s struct {
		PersonalKeys map[string]any `json:"personal_keys"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
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
	// After a failed pull last_error shows, and the body never carries a hash or a key.
	list.FailWith(keyclienttest.Status(500, ""))
	_ = r.d.keys.Pull(context.Background())
	_, body = r.call("GET", "/v1/stats", keyAlice, "")
	if !strings.Contains(body, `"last_error"`) {
		t.Fatalf("no last_error: %s", body)
	}
	for _, secret := range []string{keyAlice, keyclientHash(keyAlice), list.URL()} {
		if strings.Contains(body, secret) {
			t.Fatalf("stats leaks %q: %s", secret, body)
		}
	}
}

func keyclientHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---- check-config ----

func ckConfig(t *testing.T, personal string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	tok := writeTokens(t, "hourchd: \"aaaaaaaaaaaaaaaa-1\"\nlocalagent: {token: \"bbbbbbbbbbbbbbbb-2\", role: agent}\n")
	cfg := filepath.Join(dir, "config.yaml")
	y := "powermaster:\n  url: http://pm.example:8092\npersonal_keys:\n" + personal
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, tok
}

func runCheck(t *testing.T, cfg, tok string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run([]string{"check-config", "--config", cfg, "--tokens-file", tok}, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSR3_CheckConfigKeyListSuffixes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "keys.cache")
	base := "  url: https://keys.example/v1/lists/x\n  cache_file: " + cache + "\n"

	cfg, tok := ckConfig(t, base)
	code, out, errs := runCheck(t, cfg, tok)
	if code != 0 || !strings.HasSuffix(out, "tokens=2 (full=1, agent=1) key_list=on\n") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	// Nothing was sent or created: check-config does not Start.
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("check-config wrote the cache file")
	}

	// key_list=on goes after key_request=on, and after ui=proxy.
	cfg, tok = ckConfig(t, "  request_url: https://keys.example/r\n"+base)
	code, out, errs = runCheck(t, cfg, tok)
	if code != 0 || !strings.HasSuffix(out, " key_request=on key_list=on\n") {
		t.Fatalf("%d %q %q", code, out, errs)
	}

	// Plain http to loopback: allowed with no flag; the insecure suffix needs the flag.
	cfg, tok = ckConfig(t, "  url: http://127.0.0.1:9/x\n  cache_file: "+cache+"\n")
	if code, out, errs = runCheck(t, cfg, tok); code != 0 || !strings.HasSuffix(out, " key_list=on\n") {
		t.Fatalf("loopback: %d %q %q", code, out, errs)
	}
	cfg, tok = ckConfig(t, "  url: http://keys.example/x\n  cache_file: "+cache+"\n  allow_insecure_http: true\n")
	if code, out, errs = runCheck(t, cfg, tok); code != 0 || !strings.HasSuffix(out, " key_list=on key_list_insecure_http=on\n") {
		t.Fatalf("insecure: %d %q %q", code, out, errs)
	}
	// With the flag on but an https url there is no insecure suffix.
	cfg, tok = ckConfig(t, base+"  allow_insecure_http: true\n")
	if code, out, errs = runCheck(t, cfg, tok); code != 0 || !strings.HasSuffix(out, " key_list=on\n") {
		t.Fatalf("https with flag: %d %q %q", code, out, errs)
	}
	// Request url with insecure: full order.
	cfg, tok = ckConfig(t, "  request_url: https://keys.example/r\n  url: http://keys.example/x\n  cache_file: "+cache+"\n  allow_insecure_http: true\n")
	if code, out, errs = runCheck(t, cfg, tok); code != 0 || !strings.HasSuffix(out, " key_request=on key_list=on key_list_insecure_http=on\n") {
		t.Fatalf("all: %d %q %q", code, out, errs)
	}
	// url unset: unchanged output, no key_list.
	code, out, _ = runCheck(t, "../../config.yaml.example", tok)
	if code != 0 || strings.Contains(out, "key_list") {
		t.Fatalf("url unset: %d %q", code, out)
	}
}

func TestSR3_CheckConfigRefusals(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "keys.cache")
	// plain http without the flag: the SR-2 text including the suffix.
	cfg, tok := ckConfig(t, "  url: http://keys.example/x\n  cache_file: "+cache+"\n")
	code, out, errs := runCheck(t, cfg, tok)
	if code != 1 || strings.Contains(out, "ok:") ||
		!strings.HasPrefix(errs, "personal_keys: ") ||
		!strings.HasSuffix(strings.TrimSpace(errs), "; set personal_keys.allow_insecure_http: true only for a network path you trust") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	// unreadable token_file: personal_keys: + keyclient's error, no suffix.
	missing := filepath.Join(t.TempDir(), "missing.token")
	cfg, tok = ckConfig(t, "  url: https://keys.example/x\n  cache_file: "+cache+"\n  token_file: "+missing+"\n")
	code, out, errs = runCheck(t, cfg, tok)
	if code != 1 || strings.Contains(out, "ok:") || !strings.HasPrefix(errs, "personal_keys: keylist: ") ||
		strings.Contains(errs, "allow_insecure_http") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	// a config error comes before: nothing is built.
	cfg, tok = ckConfig(t, "  url: https://keys.example/x\n")
	code, _, errs = runCheck(t, cfg, tok)
	if code != 1 || !strings.Contains(errs, "personal_keys.cache_file: required when personal_keys.url is set") {
		t.Fatalf("%d %q", code, errs)
	}
	// the pull token is never printed.
	secretFile := filepath.Join(t.TempDir(), "pull.token")
	if err := os.WriteFile(secretFile, []byte("PULL-TOKEN-SECRET-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, tok = ckConfig(t, "  url: https://keys.example/x\n  cache_file: "+cache+"\n  token_file: "+secretFile+"\n")
	code, out, errs = runCheck(t, cfg, tok)
	if code != 0 || strings.Contains(out+errs, "PULL-TOKEN-SECRET-VALUE") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}
