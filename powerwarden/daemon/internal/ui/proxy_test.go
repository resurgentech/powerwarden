package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/apptest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

const (
	pxSecretHeader = "X-Example-Proxy-Secret"
	pxUserHeader   = "X-Example-User"
	pxSecret1      = "0123456789abcdef-test-secret"
	pxSecret2      = "fedcba9876543210-other-secret"
)

// v1Short is the fixture host's short name, a name tag a claim can ask for.
var v1Short, _, _ = strings.Cut(v1, ".")

func writeSecretFile(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ui-proxy.secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // not subject to the umask
		t.Fatal(err)
	}
	return p
}

// setupProxy is setup for ui.auth: proxy over a 0600 secret file with two
// secrets (a rotation window).
func setupProxy(t *testing.T, tweak func(*config.Config)) *uenv {
	t.Helper()
	seed := func(c *config.Config) {
		c.HostTags = map[string][]string{v1: {"kernel"}}
		if tweak != nil {
			tweak(c)
		}
	}
	env := apptest.New(t, seed, pm.Host{Hostname: v1, State: pm.StateOn})
	env.Pass()
	tr := &trap{t: t}
	env.App.Rec.PM = tr
	p, err := LoadProxyAuth(writeSecretFile(t, 0o600, "# rotation\n"+pxSecret1+"\n\n"+pxSecret2+"\n"), pxSecretHeader, pxUserHeader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := NewProxy(env.App, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &uenv{Env: env, srv: srv, trap: tr}
}

// preq is req with proxy headers (secret 1, user alice) instead of basic auth.
func (e *uenv) preq(t *testing.T, method, path string, form url.Values, mod func(*http.Request)) (int, string, http.Header) {
	t.Helper()
	return e.req(t, method, path, form, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set(pxSecretHeader, pxSecret1)
		r.Header.Set(pxUserHeader, "alice")
		if mod != nil {
			mod(r)
		}
	})
}

func TestProxyAcceptsEitherSecretLine(t *testing.T) {
	e := setupProxy(t, nil)
	for _, secret := range []string{pxSecret1, pxSecret2} {
		for _, p := range append([]string{"/ui/static/app.js"}, "/ui/hosts", "/ui/demands", "/ui/workers", "/ui/decisions", "/ui/claims") {
			code, _, hdr := e.preq(t, "GET", p, nil, func(r *http.Request) { r.Header.Set(pxSecretHeader, secret) })
			if code != 200 {
				t.Errorf("%s with %s...: %d", p, secret[:4], code)
			}
			if hdr.Get("X-Frame-Options") != "DENY" || !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Errorf("%s: security headers missing: %v", p, hdr)
			}
		}
	}
	// the events stream authenticates the same way (headers only, no body read)
	req, _ := http.NewRequest("GET", e.srv.URL+"/ui/events", nil)
	req.Header.Set(pxSecretHeader, pxSecret2)
	req.Header.Set(pxUserHeader, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/ui/events: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProxyUserIsTheCaller(t *testing.T) {
	e := setupProxy(t, nil)
	code, _, hdr := e.preq(t, "POST", "/ui/claims", url.Values{"tags": {v1Short}, "mode": {"reserve"}, "purpose": {"x"}}, nil)
	if code != http.StatusSeeOther || strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("create: %d %s", code, hdr.Get("Location"))
	}
	cs, err := e.App.Claims(context.Background(), store.ClaimFilter{})
	if err != nil || len(cs) != 1 || cs[0].Owner != "ui:alice" {
		t.Fatalf("claims %+v %v", cs, err)
	}
	_, body, _ := e.preq(t, "GET", "/ui/claims", nil, nil)
	if !strings.Contains(body, "ui:alice") {
		t.Fatal("claims page does not show the proxy user")
	}
	// another proxy user is an operator too and may release it
	code, _, _ = e.preq(t, "POST", "/ui/claims/"+cs[0].Id+"/delete", url.Values{}, func(r *http.Request) { r.Header.Set(pxUserHeader, "bob@example") })
	if code != http.StatusSeeOther {
		t.Fatalf("release: %d", code)
	}
	if cs, _ := e.App.Claims(context.Background(), store.ClaimFilter{}); len(cs) != 0 {
		t.Fatalf("claim not released: %+v", cs)
	}
	// other writes work too
	code, _, _ = e.preq(t, "POST", "/ui/hosts/"+v1+"/exclude", url.Values{"exclude": {"true"}}, nil)
	if code != http.StatusSeeOther {
		t.Fatalf("exclude: %d", code)
	}
}

func TestProxyRejectsBadSecret(t *testing.T) {
	e := setupProxy(t, nil)
	cases := map[string]func(*http.Request){
		"missing":      func(r *http.Request) { r.Header.Del(pxSecretHeader) },
		"empty":        func(r *http.Request) { r.Header.Set(pxSecretHeader, "") },
		"wrong":        func(r *http.Request) { r.Header.Set(pxSecretHeader, "not-the-secret-at-all") },
		"prefix":       func(r *http.Request) { r.Header.Set(pxSecretHeader, pxSecret1[:len(pxSecret1)-1]) },
		"suffix":       func(r *http.Request) { r.Header.Set(pxSecretHeader, pxSecret1+"x") },
		"leading":      func(r *http.Request) { r.Header.Set(pxSecretHeader, "x"+pxSecret1) },
		"both lines":   func(r *http.Request) { r.Header.Set(pxSecretHeader, pxSecret1+pxSecret2) },
		"two values":   func(r *http.Request) { r.Header.Add(pxSecretHeader, pxSecret2) },
		"case changed": func(r *http.Request) { r.Header.Set(pxSecretHeader, strings.ToUpper(pxSecret1)) },
	}
	for name, mod := range cases {
		for _, p := range []string{"/ui/hosts", "/ui/events", "/ui/static/app.js"} {
			code, _, hdr := e.preq(t, "GET", p, nil, mod)
			if code != 401 || hdr.Get("WWW-Authenticate") != "" {
				t.Errorf("%s %s: %d WWW-Authenticate=%q", name, p, code, hdr.Get("WWW-Authenticate"))
			}
		}
		code, _, hdr := e.preq(t, "POST", "/ui/claims", url.Values{"tags": {v1Short}}, mod)
		if code != 401 || hdr.Get("WWW-Authenticate") != "" {
			t.Errorf("%s post: %d %q", name, code, hdr.Get("WWW-Authenticate"))
		}
	}
	if cs, _ := e.App.Claims(context.Background(), store.ClaimFilter{}); len(cs) != 0 {
		t.Fatal("rejected post made a claim")
	}
}

func TestProxyRejectsBadUser(t *testing.T) {
	e := setupProxy(t, nil)
	cases := map[string]func(*http.Request){
		"missing":   func(r *http.Request) { r.Header.Del(pxUserHeader) },
		"empty":     func(r *http.Request) { r.Header.Set(pxUserHeader, "") },
		"space":     func(r *http.Request) { r.Header.Set(pxUserHeader, "a b") },
		"colon":     func(r *http.Request) { r.Header.Set(pxUserHeader, "x:y") },
		"too long":  func(r *http.Request) { r.Header.Set(pxUserHeader, strings.Repeat("a", 65)) },
		"slash":     func(r *http.Request) { r.Header.Set(pxUserHeader, "a/b") },
		"two users": func(r *http.Request) { r.Header.Add(pxUserHeader, "bob") },
	}
	for name, mod := range cases {
		code, _, hdr := e.preq(t, "GET", "/ui/hosts", nil, mod)
		if code != 401 || hdr.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: %d WWW-Authenticate=%q", name, code, hdr.Get("WWW-Authenticate"))
		}
	}
	// the name rule's edges are accepted
	for _, ok := range []string{"a", strings.Repeat("a", 64), "alice@host.example", "a_b-c.d"} {
		if code, _, _ := e.preq(t, "GET", "/ui/hosts", nil, func(r *http.Request) { r.Header.Set(pxUserHeader, ok) }); code != 200 {
			t.Errorf("user %q: %d", ok, code)
		}
	}
}

func TestProxyIgnoresBasicAuth(t *testing.T) {
	e := setupProxy(t, nil)
	code, _, hdr := e.req(t, "GET", "/ui/hosts", nil, func(r *http.Request) { r.SetBasicAuth("alice", "anything") })
	if code != 401 || hdr.Get("WWW-Authenticate") != "" {
		t.Fatalf("basic auth without proxy headers: %d %q", code, hdr.Get("WWW-Authenticate"))
	}
	// credentials next to good proxy headers change nothing: the proxy user wins
	code, _, _ = e.preq(t, "POST", "/ui/claims", url.Values{"tags": {v1Short}, "mode": {"reserve"}, "purpose": {"x"}}, func(r *http.Request) { r.SetBasicAuth("mallory", "x") })
	if code != http.StatusSeeOther {
		t.Fatalf("post: %d", code)
	}
	cs, _ := e.App.Claims(context.Background(), store.ClaimFilter{})
	if len(cs) != 1 || cs[0].Owner != "ui:alice" {
		t.Fatalf("claims %+v", cs)
	}
}

func TestProxyCrossOriginStillRefused(t *testing.T) {
	e := setupProxy(t, nil)
	code, _, _ := e.preq(t, "POST", "/ui/claims", url.Values{"tags": {v1Short}}, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin post: %d", code)
	}
	if code, _, _ := e.preq(t, "POST", "/ui/claims", url.Values{"tags": {v1Short}}, func(r *http.Request) { r.Header.Del("Origin") }); code != http.StatusForbidden {
		t.Fatalf("post without origin: %d", code)
	}
}

func TestProxyWithPathPrefix(t *testing.T) {
	e := setupProxy(t, func(c *config.Config) { c.UI.PathPrefix = "/p" })
	if code, body, _ := e.preq(t, "GET", "/p/ui/hosts", nil, nil); code != 200 || !strings.Contains(body, "/p/ui/") {
		t.Fatalf("prefixed page: %d", code)
	}
	if code, _, hdr := e.preq(t, "GET", "/p/ui/hosts", nil, func(r *http.Request) { r.Header.Del(pxSecretHeader) }); code != 401 || hdr.Get("WWW-Authenticate") != "" {
		t.Fatalf("prefixed page without secret: %d", code)
	}
}

func TestHtpasswdModeUnchangedByAuthSetting(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.UI.Auth = "htpasswd" })
	if code, _, _ := e.req(t, "GET", "/ui/hosts", nil, nil); code != 200 {
		t.Fatalf("htpasswd: %d", code)
	}
	code, _, hdr := e.req(t, "GET", "/ui/hosts", nil, func(r *http.Request) { r.Header.Del("Authorization") })
	if code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("htpasswd without auth: %d %q", code, hdr.Get("WWW-Authenticate"))
	}
	// proxy-style headers mean nothing in htpasswd mode
	code, _, _ = e.req(t, "GET", "/ui/hosts", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set(pxSecretHeader, pxSecret1)
		r.Header.Set(pxUserHeader, "alice")
	})
	if code != 401 {
		t.Fatalf("proxy headers in htpasswd mode: %d", code)
	}
}

// ---- secret file loader ----

func TestLoadProxyAuthModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o644 | 0o100} {
		_, err := LoadProxyAuth(writeSecretFile(t, mode, pxSecret1+"\n"), pxSecretHeader, pxUserHeader)
		if err == nil || !strings.Contains(err.Error(), "mode") || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %o: %v", mode, err)
		}
		if err != nil && strings.Contains(err.Error(), pxSecret1) {
			t.Errorf("mode %o: error holds the secret", mode)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400, 0o700} {
		if _, err := LoadProxyAuth(writeSecretFile(t, mode, pxSecret1+"\n"), pxSecretHeader, pxUserHeader); err != nil {
			t.Errorf("mode %o: %v", mode, err)
		}
	}
}

func TestLoadProxyAuthContent(t *testing.T) {
	short := "tooshort-secret"
	cases := map[string]string{
		"empty":         "",
		"only comments": "# nothing\n\n   \n# more\n",
		"short line":    pxSecret1 + "\n" + short + "\n",
	}
	for name, body := range cases {
		_, err := LoadProxyAuth(writeSecretFile(t, 0o600, body), pxSecretHeader, pxUserHeader)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, leak := range []string{short, pxSecret1} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error leaks a line: %v", name, err)
			}
		}
	}
	if _, err := LoadProxyAuth(filepath.Join(t.TempDir(), "absent"), pxSecretHeader, pxUserHeader); err == nil {
		t.Error("missing file accepted")
	}
	// whitespace around a line is trimmed; CRLF files work
	p, err := LoadProxyAuth(writeSecretFile(t, 0o600, "  "+pxSecret1+"  \r\n"), pxSecretHeader, pxUserHeader)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(pxSecretHeader, pxSecret1)
	r.Header.Set(pxUserHeader, "alice")
	if n, ok := p.Identify(r); !ok || n != "alice" {
		t.Fatalf("identify: %q %v", n, ok)
	}
}
