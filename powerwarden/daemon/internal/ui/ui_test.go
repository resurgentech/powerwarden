package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/apptest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

const v1 = "veritech01.labtwo"

// trap fails the test on any powermaster call.
type trap struct {
	t     *testing.T
	calls atomic.Int32
}

func (p *trap) hit(what string) {
	p.calls.Add(1)
	p.t.Errorf("UI caused a powermaster call: %s", what)
}
func (p *trap) ListHosts(context.Context) ([]pm.Host, error) { p.hit("ListHosts"); return nil, nil }
func (p *trap) Power(_ context.Context, h, a, _ string) (pm.Command, error) {
	p.hit("Power " + a + " " + h)
	return pm.Command{}, nil
}
func (p *trap) Events(context.Context, int64) (<-chan pm.Event, error) {
	p.hit("Events")
	return nil, nil
}

type uenv struct {
	*apptest.Env
	srv  *httptest.Server
	trap *trap
}

func setup(t *testing.T, tweak func(*config.Config)) *uenv {
	t.Helper()
	seed := func(c *config.Config) {
		c.HostTags = map[string][]string{v1: {"kernel"}}
		if tweak != nil {
			tweak(c)
		}
	}
	env := apptest.New(t, seed, pm.Host{Hostname: v1, State: pm.StateOn})
	env.Pass() // some reconcile state for the pages
	tr := &trap{t: t}
	env.App.Rec.PM = tr // from here on, any powermaster call fails the test
	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	users, err := ParseHtpasswd([]byte("# ops\njared:" + string(hash) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := New(env.App, users, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &uenv{Env: env, srv: srv, trap: tr}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *uenv) req(t *testing.T, method, path string, form url.Values, mod func(*http.Request)) (int, string, http.Header) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.srv.URL)
	}
	req.SetBasicAuth("jared", "s3cret")
	if mod != nil {
		mod(req)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// W4: UI basic auth required.
func TestBasicAuthRequired(t *testing.T) {
	e := setup(t, nil)
	for _, p := range []string{"/ui/hosts", "/ui/demands", "/ui/workers", "/ui/decisions", "/ui/events", "/ui/static/app.js"} {
		code, _, hdr := e.req(t, "GET", p, nil, func(r *http.Request) { r.Header.Del("Authorization") })
		if code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s without auth: %d", p, code)
		}
		if code, _, _ := e.req(t, "GET", p, nil, func(r *http.Request) { r.SetBasicAuth("jared", "wrong") }); code != 401 {
			t.Errorf("%s wrong password: %d", p, code)
		}
	}
	code, _, _ := e.req(t, "POST", "/ui/hosts/"+v1+"/exclude", url.Values{"exclude": {"true"}},
		func(r *http.Request) { r.Header.Del("Authorization") })
	if code != 401 {
		t.Fatalf("post without auth: %d", code)
	}
}

func TestHostsPageColdOffCountdown(t *testing.T) {
	e := setup(t, nil)
	e.PM.SetState(v1, pm.StateSoftOff) // state_since = now
	e.Clock.Advance(10 * time.Minute)
	hosts, _ := e.PM.ListHosts(context.Background())
	e.App.View.Replace(hosts, e.Clock.Now())
	e.Pass()
	_, body, _ := e.req(t, "GET", "/ui/hosts", nil, nil)
	if !strings.Contains(body, "cold off in 50:00") {
		t.Fatalf("no cold-off countdown in hosts page")
	}
}

func TestPagesRender(t *testing.T) {
	e := setup(t, nil)
	e.Store.PutWorker(context.Background(), store.WorkerPut{WorkerID: "tiny1", Hostname: "sdf1.labtwo", Tags: []string{"tiny"}, JobStatus: "idle"}, e.Clock.Now())
	e.Store.PutWorker(context.Background(), store.WorkerPut{WorkerID: "v1w", Hostname: v1, Tags: []string{"tiny"}, JobStatus: "idle"}, e.Clock.Now())
	checks := map[string][]string{
		"/ui/hosts":     {v1, "idle dwell", "Exclude", "powermaster <b class=\"ok\">connected"},
		"/ui/workers":   {"tiny1", "unknown hostname", "tags differ from host"},
		"/ui/demands":   {"Manual demand", "No open demands"},
		"/ui/decisions": {"No decisions yet"},
	}
	for p, wants := range checks {
		code, body, hdr := e.req(t, "GET", p, nil, nil)
		if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d", p, code)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", p, w)
			}
		}
		if strings.Contains(strings.ToLower(body), "powerup") && p != "/ui/decisions" {
			t.Errorf("%s: power control on a page", p)
		}
	}
	// Partial render for the live script: no <html> shell.
	_, body, _ := e.req(t, "GET", "/ui/hosts", nil, func(r *http.Request) { r.Header.Set("X-Partial", "1") })
	if strings.Contains(body, "<html") || !strings.Contains(body, v1) {
		t.Fatal("partial render wrong")
	}
	if code, _, _ := e.req(t, "GET", "/ui/", nil, nil); code != http.StatusSeeOther {
		t.Fatalf("/ui/ should redirect: %d", code)
	}
	if code, body, _ := e.req(t, "GET", "/ui/static/app.js", nil, nil); code != 200 || !strings.Contains(body, "EventSource") {
		t.Fatal("app.js not served")
	}
}

func TestCSRFGuard(t *testing.T) {
	e := setup(t, nil)
	form := url.Values{"exclude": {"true"}}
	for name, mod := range map[string]func(*http.Request){
		"evil origin":  func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"evil referer": func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Referer", "https://evil.example/x") },
		"neither":      func(r *http.Request) { r.Header.Del("Origin") },
	} {
		if code, _, _ := e.req(t, "POST", "/ui/hosts/"+v1+"/exclude", form, mod); code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
	p, _ := e.Store.HostPolicies(context.Background())
	if p[v1].Exclude {
		t.Fatal("cross-origin post took effect")
	}
	// Same-origin Referer is accepted.
	code, _, _ := e.req(t, "POST", "/ui/hosts/"+v1+"/exclude", form, func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Referer", e.srv.URL+"/ui/hosts")
	})
	if code != http.StatusSeeOther {
		t.Fatalf("same-origin referer: %d", code)
	}
	p, _ = e.Store.HostPolicies(context.Background())
	if !p[v1].Exclude {
		t.Fatal("exclude not applied")
	}
}

func TestActions(t *testing.T) {
	e := setup(t, nil)
	code, _, hdr := e.req(t, "POST", "/ui/demands", url.Values{"id": {"manual:hold"}, "tags": {"kernel, x86"}, "lease_seconds": {"600"}}, nil)
	if code != http.StatusSeeOther || strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("create: %d %s", code, hdr.Get("Location"))
	}
	d, err := e.Store.GetDemand(context.Background(), "manual:hold")
	if err != nil || d.Source != "manual" || len(d.Tags) != 2 || d.LeaseSeconds != 600 || d.ExternalRef != "ui:jared" {
		t.Fatalf("stored %+v %v", d, err)
	}
	_, body, _ := e.req(t, "GET", "/ui/demands", nil, nil)
	if !strings.Contains(body, "manual:hold") || !strings.Contains(body, "/ui/demands/manual:hold/delete") {
		t.Fatal("manual demand not listed with delete")
	}
	// Invalid input comes back as an error, not a 500.
	_, _, hdr = e.req(t, "POST", "/ui/demands", url.Values{"id": {"bad id"}}, nil)
	if !strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatal("bad id accepted")
	}
	// hourchd demands cannot be deleted from the UI.
	e.Store.PutDemand(context.Background(), store.DemandPut{ID: "hourchd:1", Source: "hourchd"}, e.Clock.Now())
	_, _, hdr = e.req(t, "POST", "/ui/demands/hourchd:1/delete", url.Values{}, nil)
	if !strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatal("hourchd demand deleted from UI")
	}
	e.req(t, "POST", "/ui/demands/manual:hold/delete", url.Values{}, nil)
	if _, err := e.Store.GetDemand(context.Background(), "manual:hold"); err == nil {
		t.Fatal("manual demand not deleted")
	}
	// Exclude an unknown host: error, no row.
	_, _, hdr = e.req(t, "POST", "/ui/hosts/nope.labtwo/exclude", url.Values{"exclude": {"true"}}, nil)
	if !strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatal("unknown host excluded")
	}
}

func TestUIEventsStream(t *testing.T) {
	e := setup(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/ui/events", nil)
	req.SetBasicAuth("jared", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for e.App.Hub.Subscribers() == 0 {
		time.Sleep(time.Millisecond)
	}
	e.App.SetExclude(context.Background(), v1, true)
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	for !strings.Contains(string(buf[:n]), "event: host") {
		m, err := resp.Body.Read(buf[n:])
		if err != nil {
			t.Fatalf("no host event: %q", buf[:n])
		}
		n += m
	}
}

// W4: UI never calls powermaster: every page and action above ran with the trap installed.
func TestUINeverCallsPowermaster(t *testing.T) {
	e := setup(t, nil)
	for _, p := range []string{"/ui/hosts", "/ui/demands", "/ui/workers", "/ui/decisions"} {
		e.req(t, "GET", p, nil, nil)
		e.req(t, "GET", p, nil, func(r *http.Request) { r.Header.Set("X-Partial", "1") })
	}
	e.req(t, "POST", "/ui/hosts/"+v1+"/exclude", url.Values{"exclude": {"true"}}, nil)
	e.req(t, "POST", "/ui/hosts/"+v1+"/exclude", url.Values{"exclude": {"false"}}, nil)
	e.req(t, "POST", "/ui/demands", url.Values{"id": {"manual:x"}, "tags": {"kernel"}}, nil)
	e.req(t, "POST", "/ui/demands/manual:x/delete", url.Values{}, nil)
	if n := e.trap.calls.Load(); n != 0 {
		t.Fatalf("%d powermaster calls from the UI", n)
	}
}

// The site root redirects to the hosts page without asking for a login.
func TestRootRedirectsToUI(t *testing.T) {
	e := setup(t, nil)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/hosts" {
		t.Fatalf("GET / = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestUITagsRejectNameTagAndFlagsDuplicates(t *testing.T) {
	e := setup(t, nil)
	_, _, hdr := e.req(t, "POST", "/ui/hosts/"+v1+"/tags", url.Values{"tags": {"kernel veritech01"}}, nil)
	if loc := hdr.Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("name-tag edit not rejected: Location %q", loc)
	}
	_, body, _ := e.req(t, "GET", "/ui/hosts", nil, nil)
	if strings.Contains(body, "duplicate short name") {
		t.Fatal("duplicate flag shown with unique names")
	}
	e.PM.AddHost(pm.Host{Hostname: "veritech01.labone", State: pm.StateOn})
	hosts, _ := e.PM.ListHosts(context.Background())
	e.App.View.Replace(hosts, e.Clock.Now())
	_, body, _ = e.req(t, "GET", "/ui/hosts", nil, nil)
	if !strings.Contains(body, "duplicate short name") {
		t.Fatal("duplicate short name not flagged on hosts page")
	}
}

func TestUIFlagsStaleExplicitTag(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.HostTags = map[string][]string{v1: {"kernel", "veritech02"}} })
	e.PM.AddHost(pm.Host{Hostname: "veritech02.labtwo", State: pm.StateOn})
	hosts, _ := e.PM.ListHosts(context.Background())
	e.App.View.Replace(hosts, e.Clock.Now())
	_, body, _ := e.req(t, "GET", "/ui/hosts", nil, nil)
	if !strings.Contains(body, "ignored: veritech02") {
		t.Fatal("stale explicit tag not flagged on hosts page")
	}
}

// ---- PWC9: claims page ----

func (e *uenv) uiClaims(t *testing.T) []string {
	t.Helper()
	cs, err := e.App.Claims(context.Background(), store.ClaimFilter{})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, c := range cs {
		ids = append(ids, c.Id)
	}
	return ids
}

func TestClaimsPageBasicAuthRequired(t *testing.T) {
	e := setup(t, nil)
	if code, _, hdr := e.req(t, "GET", "/ui/claims", nil, func(r *http.Request) { r.Header.Del("Authorization") }); code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("get without auth: %d", code)
	}
	for _, p := range []string{"/ui/claims", "/ui/claims/x/delete"} {
		if code, _, _ := e.req(t, "POST", p, url.Values{"tags": {"kernel"}}, func(r *http.Request) { r.Header.Del("Authorization") }); code != 401 {
			t.Fatalf("post %s without auth: %d", p, code)
		}
	}
	if len(e.uiClaims(t)) != 0 {
		t.Fatal("unauthenticated post made a claim")
	}
}

func TestClaimCreateAndListAndRelease(t *testing.T) {
	e := setup(t, nil)
	code, _, hdr := e.req(t, "POST", "/ui/claims", url.Values{"tags": {"veritech01"}, "mode": {"reserve"}, "purpose": {"bench run"}, "max_seconds": {"7200"}}, nil)
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/ui/claims") || strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("create: %d %s", code, hdr.Get("Location"))
	}
	cs, err := e.App.Claims(context.Background(), store.ClaimFilter{})
	if err != nil || len(cs) != 1 {
		t.Fatalf("claims %+v %v", cs, err)
	}
	c := cs[0]
	if !strings.HasPrefix(c.Id, "ui-") || c.Owner != "ui:jared" || !c.Detached || c.Session != "" ||
		c.Mode != "reserve" || c.Purpose != "bench run" || c.LeaseUntil != nil {
		t.Fatalf("claim %+v", c)
	}
	if want := e.Clock.Now().Add(7200 * time.Second); !c.ExpiresAt.Equal(want) {
		t.Fatalf("hard cap %v want %v", c.ExpiresAt, want)
	}
	_, body, _ := e.req(t, "GET", "/ui/claims", nil, nil)
	for _, w := range []string{c.Id, "ui:jared", "bench run", "reserve", "detached", v1, "/ui/claims/" + c.Id + "/delete", "Cap left"} {
		if !strings.Contains(body, w) {
			t.Errorf("claims page missing %q", w)
		}
	}
	_, hbody, _ := e.req(t, "GET", "/ui/hosts", nil, nil)
	for _, w := range []string{c.Id, "ui:jared", "bench run", "detached", "jobs blocked"} {
		if !strings.Contains(hbody, w) {
			t.Errorf("hosts page missing %q", w)
		}
	}
	// A bad create comes back through ?err=, not a 500 and not a claim.
	_, _, hdr = e.req(t, "POST", "/ui/claims", url.Values{"tags": {""}, "purpose": {"x"}}, nil)
	if !strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("empty tags accepted: %s", hdr.Get("Location"))
	}
	_, _, hdr = e.req(t, "POST", "/ui/claims", url.Values{"tags": {"veritech01"}, "purpose": {"x"}, "max_seconds": {"soon"}}, nil)
	if !strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("bad hard cap accepted: %s", hdr.Get("Location"))
	}
	if n := len(e.uiClaims(t)); n != 1 {
		t.Fatalf("%d claims after bad creates", n)
	}
	// Release works, even for a claim owned by someone else.
	if _, err := e.App.PutClaim(context.Background(), app.Caller{Name: "ann@box"}, store.ClaimPut{ID: "ann1", Tags: []string{"veritech01"}, Purpose: "p"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{c.Id, "ann1"} {
		code, _, hdr = e.req(t, "POST", "/ui/claims/"+id+"/delete", url.Values{}, nil)
		if code != http.StatusSeeOther || strings.Contains(hdr.Get("Location"), "err=") {
			t.Fatalf("release %s: %d %s", id, code, hdr.Get("Location"))
		}
	}
	if n := len(e.uiClaims(t)); n != 0 {
		t.Fatalf("%d claims after release", n)
	}
	// Releasing a missing claim is fine.
	if _, _, hdr = e.req(t, "POST", "/ui/claims/gone/delete", url.Values{}, nil); strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("release of absent claim errored: %s", hdr.Get("Location"))
	}
}

func TestClaimPostsCrossOriginRefused(t *testing.T) {
	e := setup(t, nil)
	if _, err := e.App.PutClaim(context.Background(), app.Caller{Name: "ann@box"}, store.ClaimPut{ID: "ann1", Tags: []string{"veritech01"}, Purpose: "p"}); err != nil {
		t.Fatal(err)
	}
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	if code, _, _ := e.req(t, "POST", "/ui/claims", url.Values{"tags": {"veritech01"}, "purpose": {"x"}}, evil); code != 403 {
		t.Fatalf("cross-origin create: %d", code)
	}
	if code, _, _ := e.req(t, "POST", "/ui/claims/ann1/delete", url.Values{}, evil); code != 403 {
		t.Fatalf("cross-origin release: %d", code)
	}
	if ids := e.uiClaims(t); len(ids) != 1 || ids[0] != "ann1" {
		t.Fatalf("cross-origin post took effect: %v", ids)
	}
}

func TestClaimsPageShowsLeaseNotDetached(t *testing.T) {
	e := setup(t, nil)
	e.App.PutClaim(context.Background(), app.Caller{Name: "ann@box"}, store.ClaimPut{ID: "live1", Tags: []string{"veritech01"}, Purpose: "p", Session: "mcp-7"})
	_, body, _ := e.req(t, "GET", "/ui/claims", nil, nil)
	for _, w := range []string{"live1", "ann@box", "mcp-7", "15:00", "Lease left"} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q", w)
		}
	}
	rows := body
	if parts := strings.SplitN(body, "<tbody>", 2); len(parts) == 2 {
		rows = strings.SplitN(parts[1], "</tbody>", 2)[0]
	}
	if strings.Contains(rows, "detached") {
		t.Error("attached claim shown as detached")
	}
	if strings.Contains(body, "/power") {
		t.Error("power control on claims page")
	}
}

func TestAppJSListensForClaimEvents(t *testing.T) {
	e := setup(t, nil)
	_, body, _ := e.req(t, "GET", "/ui/static/app.js", nil, nil)
	if !strings.Contains(body, `"claim"`) {
		t.Fatal("app.js does not listen for claim events")
	}
}
