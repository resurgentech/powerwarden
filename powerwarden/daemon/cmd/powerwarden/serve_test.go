package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
)

// fakePowermaster serves just enough of powermaster's API.
func fakePowermaster(t *testing.T, powered *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pm-token" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"unauthorized","message":"no"}`)
			return
		}
		switch {
		case r.URL.Path == "/v1/hosts":
			fmt.Fprint(w, `[{"hostname":"veritech01.labtwo","locked":false,"class":"veritech","state":"soft_off",
			  "status":{"pdu":null,"ipmi":null,"ping":null,"ssh":null,"observed_at":null},"last_error":"","power_failed_at":null,"active_command":null}]`)
		case r.URL.Path == "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/power"):
			powered.Add(1)
			w.WriteHeader(202)
			fmt.Fprint(w, `{"id":1,"hostname":"veritech01.labtwo","action":"powerup","state":"queued","force":false,"escalated":false,
			  "requested_by":"powerwarden","reason":"d1","attempt":"a","created_at":"2026-10-27T12:00:00Z","started_at":null,
			  "deadline":"2026-10-27T12:15:00Z","finished_at":null,"error":""}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func write(t *testing.T, dir, name, content string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// End to end: the wired daemon serves the API and UI, and a demand PUT
// reaches powermaster as a powerup without waiting for the timer.
func TestServeWiring(t *testing.T) {
	var powered atomic.Int32
	pmSrv := fakePowermaster(t, &powered)
	dir := t.TempDir()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg := config.Default()
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.TokensFile = write(t, dir, "tokens.yaml", "hourchd: hourchd-token-0123456789\n")
	cfg.Powermaster.URL = pmSrv.URL
	cfg.Powermaster.TokenFile = write(t, dir, "powermaster.token", "pm-token\n")
	cfg.UI.HtpasswdFile = write(t, dir, "ui.htpasswd", "jared:"+string(hash)+"\n")
	cfg.HostTags = map[string][]string{"veritech01.labtwo": {"kernel"}}

	d, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	srv := httptest.NewServer(d.handler)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.rec.Run(ctx)

	get := func(path string, auth func(*http.Request)) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth != nil {
			auth(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer hourchd-token-0123456789") }
	if c := get("/v1/healthz", nil); c != 200 {
		t.Fatalf("healthz %d", c)
	}
	if c := get("/v1/demands", nil); c != 401 {
		t.Fatalf("unauthenticated %d", c)
	}
	if c := get("/ui/hosts", func(r *http.Request) { r.SetBasicAuth("jared", "pw") }); c != 200 {
		t.Fatalf("ui %d", c)
	}
	if c := get("/ui/hosts", bearer); c != 401 {
		t.Fatalf("ui with bearer only %d", c)
	}

	// Wait for the first host view, then PUT a demand.
	deadline := time.Now().Add(3 * time.Second)
	for d.rec.Passes() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	req, _ := http.NewRequest("PUT", srv.URL+"/v1/demands/hourchd:d1", strings.NewReader(`{"tags":["kernel"],"source":"hourchd"}`))
	bearer(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("put %v %v", resp, err)
	}
	resp.Body.Close()
	for powered.Load() == 0 && time.Since(start) < time.Second {
		time.Sleep(2 * time.Millisecond)
	}
	if powered.Load() != 1 {
		t.Fatal("no powerup within 1s of the demand")
	}
}

const proxyTestSecret = "0123456789abcdef-test-secret"

// proxyCfg is a wired config for ui.auth: proxy. The htpasswd path does not
// exist: proxy mode must not read it.
func proxyCfg(t *testing.T, secretMode os.FileMode) config.Config {
	t.Helper()
	var powered atomic.Int32
	pmSrv := fakePowermaster(t, &powered)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.TokensFile = write(t, dir, "tokens.yaml", "hourchd: hourchd-token-0123456789\n")
	cfg.Powermaster.URL = pmSrv.URL
	cfg.Powermaster.TokenFile = write(t, dir, "powermaster.token", "pm-token\n")
	cfg.UI.HtpasswdFile = filepath.Join(dir, "does-not-exist.htpasswd")
	cfg.UI.Auth = "proxy"
	secret := write(t, dir, "ui-proxy.secret", proxyTestSecret+"\n")
	if err := os.Chmod(secret, secretMode); err != nil {
		t.Fatal(err)
	}
	cfg.UI.Proxy = config.UIProxy{SecretFile: secret, SecretHeader: "X-Example-Proxy-Secret", UserHeader: "X-Example-User"}
	return cfg
}

func TestBuildProxyModeDoesNotReadHtpasswd(t *testing.T) {
	cfg := proxyCfg(t, 0o600)
	d, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("proxy mode with a missing htpasswd file: %v", err)
	}
	defer d.Close()
	do := func(mod func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/ui/hosts", nil)
		mod(req)
		rec := httptest.NewRecorder()
		d.handler.ServeHTTP(rec, req)
		return rec
	}
	rec := do(func(r *http.Request) {
		r.Header.Set("X-Example-Proxy-Secret", proxyTestSecret)
		r.Header.Set("X-Example-User", "alice")
	})
	if rec.Code != 200 {
		t.Fatalf("proxy request: %d", rec.Code)
	}
	rec = do(func(r *http.Request) { r.SetBasicAuth("alice", "pw") })
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("basic auth in proxy mode: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestBuildKeyRequestRoute(t *testing.T) {
	const u = "https://keys.example.com/v1/services/powerwarden/requests"
	for _, want := range []string{"", u} {
		cfg := proxyCfg(t, 0o600)
		cfg.PersonalKeys.RequestURL = want
		d, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		d.handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/key-request", nil))
		d.Close()
		if want == "" {
			if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
				t.Fatalf("unset: %d %s", rec.Code, rec.Body)
			}
			continue
		}
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"request_url":"`+u+`"}` {
			t.Fatalf("set: %d %s", rec.Code, rec.Body)
		}
	}
}

func TestBuildProxyModeRejectsReadableSecret(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o620, 0o602, 0o610, 0o601} {
		d, err := build(proxyCfg(t, mode), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		if err == nil {
			d.Close()
			t.Fatalf("mode %o accepted", mode)
		}
		if !strings.Contains(err.Error(), "mode") || strings.Contains(err.Error(), proxyTestSecret) {
			t.Fatalf("mode %o: %v", mode, err)
		}
	}
}
