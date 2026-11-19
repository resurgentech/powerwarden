package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/apptest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

const (
	tokAnn = "ann-token-0123456789abcd"
	v2     = "veritech02.labtwo"
	vLock  = "locked01.labtwo"
)

// setupClaims runs the API over three hosts: v1 and v2 (tag kernel, on, so a
// hold claim is ready at once) and a locked host (tag gpu, off). Tokens:
// hourchd, jared, ann.
func setupClaims(t *testing.T, tweak func(*config.Config)) *tenv {
	t.Helper()
	seed := func(c *config.Config) {
		c.HostTags = map[string][]string{v1: {"kernel"}, v2: {"kernel"}, vLock: {"gpu"}}
		if tweak != nil {
			tweak(c)
		}
	}
	env := apptest.New(t, seed, pm.Host{Hostname: v1, State: pm.StateOn}, pm.Host{Hostname: v2, State: pm.StateOn},
		pm.Host{Hostname: vLock, State: pm.StateSoftOff, Locked: true})
	toks, err := NewTokens(map[string]string{"hourchd": tokHourchd, "jared": tokJared, "ann": tokAnn})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{App: env.App, Tokens: toks, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &tenv{Env: env, srv: srv, mux: mux}
}

func (e *tenv) putClaim(t *testing.T, id, token, body string) (int, api.Claim, []byte) {
	t.Helper()
	code, b := e.do(t, "PUT", "/v1/claims/"+id, token, body)
	var c api.Claim
	if code == 200 {
		c = mustJSON[api.Claim](t, b)
	}
	return code, c, b
}

func errCode(t *testing.T, b []byte) api.ErrorCode {
	t.Helper()
	return mustJSON[api.Error](t, b).Error
}

func TestPutClaimIdempotentOwnerAndSession(t *testing.T) {
	e := setupClaims(t, nil)
	body := `{"tags":["veritech01"],"purpose":"debug","session":"mcp-1"}`
	code, first, b := e.putClaim(t, "c1", tokJared, body)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if first.Owner != "jared" || first.Session != "mcp-1" || first.Mode != api.Hold || first.Detached ||
		first.State != api.Ready || first.Hostname == nil || *first.Hostname != v1 || first.HostState == nil ||
		*first.HostState != api.On || first.Version != 1 || first.LeaseSeconds == nil || *first.LeaseSeconds != 900 ||
		first.LeaseUntil == nil || !first.LeaseUntil.Equal(apptest.T0.Add(15*time.Minute)) ||
		!first.ExpiresAt.Equal(apptest.T0.Add(8*time.Hour)) || first.Detail != "" {
		t.Fatalf("created: %+v", first)
	}
	// Renew from another session of the same owner: accepted, session kept, lease moved.
	e.Clock.Advance(time.Minute)
	code, again, b := e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"changed","session":"mcp-2"}`)
	if code != 200 || again.Session != "mcp-1" || again.Purpose != "debug" || !again.CreatedAt.Equal(first.CreatedAt) ||
		!again.ExpiresAt.Equal(first.ExpiresAt) || *again.Hostname != v1 ||
		!again.LeaseUntil.Equal(apptest.T0.Add(16*time.Minute)) || again.Version != 1 {
		t.Fatalf("renew: %d %s", code, b)
	}
	// An owner in the body is not a field.
	if code, _, b := e.putClaim(t, "c2", tokJared, `{"tags":["veritech01"],"purpose":"x","owner":"ann"}`); code != 400 {
		t.Fatalf("owner in body: %d %s", code, b)
	}
	// Omitted session is stored as "".
	_, c3, _ := e.putClaim(t, "c3", tokJared, `{"tags":["veritech02"],"purpose":"x"}`)
	if c3.Session != "" {
		t.Fatalf("session: %q", c3.Session)
	}
	// Omitted lease keeps the stored one; an explicit lease replaces it.
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"x","lease_seconds":120}`)
	_, kept, _ := e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"x"}`)
	if *kept.LeaseSeconds != 120 {
		t.Fatalf("lease not kept: %+v", kept)
	}
	// GET and list agree.
	_, b = e.do(t, "GET", "/v1/claims/c1", tokAnn, "")
	if got := mustJSON[api.Claim](t, b); got.Id != "c1" || got.Owner != "jared" {
		t.Fatalf("get: %s", b)
	}
}

func TestPutClaimBadRequests(t *testing.T) {
	e := setupClaims(t, nil)
	cases := map[string]string{
		"hostname field": `{"hostname":"veritech01.labtwo","tags":["x"],"purpose":"p"}`,
		"no tags":        `{"purpose":"p"}`,
		"empty tags":     `{"tags":[],"purpose":"p"}`,
		"no purpose":     `{"tags":["a"]}`,
		"empty purpose":  `{"tags":["a"],"purpose":""}`,
		"long purpose":   `{"tags":["a"],"purpose":"` + strings.Repeat("x", 257) + `"}`,
		"bad session":    `{"tags":["a"],"purpose":"p","session":"a b"}`,
		"empty session":  `{"tags":["a"],"purpose":"p","session":""}`,
		"bad mode":       `{"tags":["a"],"purpose":"p","mode":"exclusive"}`,
		"bad tag":        `{"tags":["a b"],"purpose":"p"}`,
		"detached+lease": `{"tags":["a"],"purpose":"p","detached":true,"lease_seconds":60}`,
		"zero cap":       `{"tags":["a"],"purpose":"p","max_seconds":0}`,
		"not json":       `nope`,
	}
	for name, body := range cases {
		code, b := e.do(t, "PUT", "/v1/claims/c1", tokJared, body)
		if code != 400 || errCode(t, b) != api.ErrorCodeBadRequest {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if code, b := e.do(t, "PUT", "/v1/claims/a%20b", tokJared, `{"tags":["a"],"purpose":"p"}`); code != 400 {
		t.Errorf("bad id: %d %s", code, b)
	}
	if code, _ := e.do(t, "PUT", "/v1/claims/c1", "", `{"tags":["a"],"purpose":"p"}`); code != 401 {
		t.Error("unauthenticated PUT not 401")
	}
	for _, c := range [][2]string{{"GET", "/v1/claims"}, {"GET", "/v1/claims/c1"}, {"DELETE", "/v1/claims/c1"}, {"POST", "/v1/claims/c1/extend"}} {
		if code, _ := e.do(t, c[0], c[1], "", ""); code != 401 {
			t.Errorf("%s %s without token: %d", c[0], c[1], code)
		}
	}
}

func TestClaimOwnershipAcrossTokens(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p","session":"s1"}`)
	// Same owner, second session: renew, extend and release all work.
	if code, _, b := e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p","session":"s2"}`); code != 200 {
		t.Fatalf("renew from s2: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":3600}`); code != 200 {
		t.Fatalf("extend: %d %s", code, b)
	}
	// Another token name: 403 on renew and extend.
	code, _, b := e.putClaim(t, "c1", tokAnn, `{"tags":["veritech01"],"purpose":"p"}`)
	if code != 403 || errCode(t, b) != api.ErrorCodeForbidden {
		t.Fatalf("renew by other: %d %s", code, b)
	}
	code, b = e.do(t, "POST", "/v1/claims/c1/extend", tokAnn, `{"max_seconds":3600}`)
	if code != 403 || errCode(t, b) != api.ErrorCodeForbidden {
		t.Fatalf("extend by other: %d %s", code, b)
	}
	// ann is a full-role token here, so DELETE by another name is an
	// operator release and works. A non-operator caller is refused.
	err := e.App.ReleaseClaim(context.Background(), app.Caller{Name: "ann"}, "c1")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("non-operator release: %v", err)
	}
	if code, _ := e.do(t, "DELETE", "/v1/claims/c1", tokAnn, ""); code != 204 {
		t.Fatalf("operator release: %d", code)
	}
	if code, _ := e.do(t, "GET", "/v1/claims/c1", tokJared, ""); code != 404 {
		t.Fatal("released claim still readable")
	}
	// The owner releases from any session: no session is involved.
	e.putClaim(t, "c2", tokJared, `{"tags":["veritech01"],"purpose":"p","session":"s1"}`)
	if code, _ := e.do(t, "DELETE", "/v1/claims/c2", tokJared, ""); code != 204 {
		t.Fatal("owner release")
	}
	if code, b := e.do(t, "DELETE", "/v1/claims/never", tokJared, ""); code != 204 || len(b) != 0 {
		t.Fatalf("absent: %d %s", code, b)
	}
}

func TestClaimExtendAndClamp(t *testing.T) {
	e := setupClaims(t, nil)
	_, c, _ := e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	e.Clock.Advance(time.Minute)
	code, b := e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":7200}`)
	x := mustJSON[api.Claim](t, b)
	now := apptest.T0.Add(time.Minute)
	if code != 200 || !x.ExpiresAt.Equal(now.Add(2*time.Hour)) || !x.LeaseUntil.Equal(*c.LeaseUntil) ||
		!x.CreatedAt.Equal(c.CreatedAt) || x.Version != c.Version || *x.Hostname != *c.Hostname || *x.LeaseSeconds != 900 {
		t.Fatalf("extend: %d %s", code, b)
	}
	_, b = e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":1}`)
	if x := mustJSON[api.Claim](t, b); !x.ExpiresAt.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("min clamp: %s", b)
	}
	_, b = e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":99999999}`)
	if x := mustJSON[api.Claim](t, b); !x.ExpiresAt.Equal(now.Add(72 * time.Hour)) {
		t.Fatalf("max clamp: %s", b)
	}
	// Create-time clamping of the cap and the lease.
	_, lo, _ := e.putClaim(t, "lo", tokJared, `{"tags":["veritech02"],"purpose":"p","max_seconds":1,"lease_seconds":1}`)
	if *lo.LeaseSeconds != 60 || !lo.ExpiresAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("create min clamp: %+v", lo)
	}
	_, hi, _ := e.putClaim(t, "hi", tokJared, `{"tags":["veritech02"],"purpose":"p","max_seconds":999999999,"lease_seconds":999999}`)
	if *hi.LeaseSeconds != 3600 || !hi.ExpiresAt.Equal(now.Add(72*time.Hour)) {
		t.Fatalf("create max clamp: %+v", hi)
	}
	for _, body := range []string{`{}`, `{"max_seconds":0}`, `{"max_seconds":5,"x":1}`} {
		if code, _ := e.do(t, "POST", "/v1/claims/c1/extend", tokJared, body); code != 400 {
			t.Errorf("extend %s: %d", body, code)
		}
	}
	if code, _ := e.do(t, "POST", "/v1/claims/nope/extend", tokJared, `{"max_seconds":3600}`); code != 404 {
		t.Error("extend unknown not 404")
	}
}

func TestClaimLapse410(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	e.Clock.Advance(16 * time.Minute)
	code, _, b := e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	if code != 410 || errCode(t, b) != api.ErrorCodeClaimExpired {
		t.Fatalf("renew after lapse: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/v1/claims/c1/extend", tokJared, `{"max_seconds":3600}`); code != 410 {
		t.Fatalf("extend after lapse: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/v1/claims/c1", tokJared, ""); code != 404 {
		t.Fatal("lapsed claim readable without include_expired")
	}
	if _, b := e.do(t, "GET", "/v1/claims", tokJared, ""); len(mustJSON[[]api.Claim](t, b)) != 0 {
		t.Fatal("lapsed claim listed")
	}
	if code, b := e.do(t, "GET", "/v1/claims?include_expired=true", tokJared, ""); code != 200 || len(mustJSON[[]api.Claim](t, b)) != 1 {
		t.Fatalf("include_expired: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/v1/claims/c1?include_expired=true", tokJared, ""); code != 200 {
		t.Fatal("get with include_expired")
	}
	if code, _ := e.do(t, "GET", "/v1/claims?include_expired=maybe", tokJared, ""); code != 400 {
		t.Fatal("bad include_expired")
	}
	// A lapsed claim frees its host.
	if _, d, _ := e.putClaim(t, "d1", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`); d.State != api.Ready {
		t.Fatalf("host not free after lapse: %+v", d)
	}
}

func TestClaimConflicts(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	for name, body := range map[string]string{
		"tags":      `{"tags":["veritech02"],"purpose":"p"}`,
		"more tags": `{"tags":["veritech01","kernel"],"purpose":"p"}`,
		"mode":      `{"tags":["veritech01"],"purpose":"p","mode":"reserve"}`,
		"detached":  `{"tags":["veritech01"],"purpose":"p","detached":true}`,
	} {
		code, _, b := e.putClaim(t, "c1", tokJared, body)
		if code != 409 || errCode(t, b) != api.ErrorCodeConflict {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// Tag order and duplicates are not a change.
	e.putClaim(t, "c2", tokJared, `{"tags":["kernel","veritech02"],"purpose":"p"}`)
	if code, _, b := e.putClaim(t, "c2", tokJared, `{"tags":["veritech02","kernel","kernel"],"purpose":"p"}`); code != 200 {
		t.Fatalf("reordered tags: %d %s", code, b)
	}
}

func TestClaimCapacity429(t *testing.T) {
	e := setupClaims(t, func(c *config.Config) { c.Limits.MaxClaims = 2 })
	e.putClaim(t, "a", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	e.putClaim(t, "b", tokJared, `{"tags":["veritech01"],"purpose":"p","detached":true}`)
	code, _, b := e.putClaim(t, "c", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	if code != 429 || errCode(t, b) != api.ErrorCodeCapacity {
		t.Fatalf("%d %s", code, b)
	}
	if code, _, _ := e.putClaim(t, "a", tokJared, `{"tags":["veritech01"],"purpose":"p"}`); code != 200 {
		t.Fatal("renew at capacity must work")
	}
}

func TestHostsTagsFilterIncludesNameTags(t *testing.T) {
	e := setupClaims(t, nil)
	names := func(query string) []string {
		code, b := e.do(t, "GET", "/v1/hosts"+query, tokJared, "")
		if code != 200 {
			t.Fatalf("%s: %d %s", query, code, b)
		}
		var out []string
		for _, h := range mustJSON[[]api.HostView](t, b) {
			out = append(out, h.Hostname)
		}
		return out
	}
	for q, want := range map[string]string{
		"":                               v1 + "," + v2 + "," + vLock,
		"?tags=kernel":                   v1 + "," + v2,
		"?tags=veritech01":               v1,
		"?tags=" + v2:                    v2,
		"?tags=kernel,veritech02":        v2,
		"?tags=kernel,veritech01.labtwo": v1,
		"?tags=gpu":                      vLock,
		"?tags=kernel,gpu":               "",
		"?tags=nope":                     "",
		"?tags=veritech01,veritech02":    "",
	} {
		if got := strings.Join(names(q), ","); got != want {
			t.Errorf("%s: got %q want %q", q, got, want)
		}
	}
	for _, q := range []string{"?tags=", "?tags=a b", "?tags=a,,b"} {
		if code, _ := e.do(t, "GET", "/v1/hosts"+q, tokJared, ""); code != 400 {
			t.Errorf("%q: %d, want 400", q, code)
		}
	}
}

func TestClaimsShownPerHostAndJobsBlocked(t *testing.T) {
	e := setupClaims(t, nil)
	host := func() api.HostView {
		_, b := e.do(t, "GET", "/v1/hosts/"+v1, tokJared, "")
		return mustJSON[api.HostView](t, b)
	}
	if h := host(); h.JobsBlocked || h.Claims == nil || len(h.Claims) != 0 {
		t.Fatalf("idle host: %+v", h)
	}
	e.putClaim(t, "h", tokJared, `{"tags":["veritech01"],"purpose":"hold it","session":"s1"}`)
	h := host()
	if h.JobsBlocked || len(h.Claims) != 1 || h.Claims[0].Id != "h" || h.Claims[0].Owner != "jared" ||
		h.Claims[0].Session != "s1" || h.Claims[0].Purpose != "hold it" || h.Claims[0].Mode != api.Hold ||
		h.Claims[0].State != api.Ready || h.Claims[0].LeaseUntil == nil {
		t.Fatalf("hold: %+v", h)
	}
	e.putClaim(t, "r", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"reserve","detached":true}`)
	h = host()
	if !h.JobsBlocked || len(h.Claims) != 2 || h.Claims[1].Id != "r" || !h.Claims[1].Detached || h.Claims[1].LeaseUntil != nil {
		t.Fatalf("reserve: %+v", h)
	}
	if err := e.App.ReleaseClaim(context.Background(), app.Caller{Name: "ann"}, "r"); err != nil {
		t.Fatal(err)
	}
	if h := host(); h.JobsBlocked || len(h.Claims) != 1 {
		t.Fatalf("after release: %+v", h)
	}
	e.do(t, "DELETE", "/v1/claims/h", tokJared, "")
	e.putClaim(t, "d", tokJared, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
	if h := host(); !h.JobsBlocked || len(h.Claims) != 1 || h.Claims[0].Mode != api.Dedicated {
		t.Fatalf("dedicated: %+v", h)
	}
	e.do(t, "DELETE", "/v1/claims/d", tokJared, "")
	if h := host(); h.JobsBlocked || len(h.Claims) != 0 {
		t.Fatalf("after dedicated release: %+v", h)
	}
	// A pending dedicated claim is not shown on the host and does not block jobs.
	e.putClaim(t, "h2", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	e.putClaim(t, "d2", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
	if h := host(); h.JobsBlocked || len(h.Claims) != 1 {
		t.Fatalf("pending dedicated: %+v", h)
	}
	// The list route shows the same.
	_, b := e.do(t, "GET", "/v1/hosts", tokJared, "")
	for _, h := range mustJSON[[]api.HostView](t, b) {
		if h.Claims == nil {
			t.Errorf("%s: claims is null", h.Hostname)
		}
	}
}

func TestClaimDedicatedPendingThenBinds(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "h", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	code, d, b := e.putClaim(t, "d", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
	if code != 200 || d.State != api.Pending || d.Hostname != nil || d.HostState != nil || d.Detail != "waiting for "+v1 || d.Version != 1 {
		t.Fatalf("pending: %d %s", code, b)
	}
	if code, b := e.do(t, "DELETE", "/v1/claims/h", tokJared, ""); code != 204 {
		t.Fatalf("release: %d %s", code, b)
	}
	_, b = e.do(t, "GET", "/v1/claims/d", tokAnn, "")
	d = mustJSON[api.Claim](t, b)
	if d.State != api.Ready || d.Hostname == nil || *d.Hostname != v1 || d.Detail != "" || d.Version != 2 {
		t.Fatalf("not bound after release: %s", b)
	}
	// A hold behind a dedicated claim waits.
	_, h, _ := e.putClaim(t, "h3", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	if h.State != api.Pending {
		t.Fatalf("hold behind dedicated: %+v", h)
	}
	// With two candidate hosts, a claim goes to the free one.
	_, k, _ := e.putClaim(t, "k", tokJared, `{"tags":["kernel"],"purpose":"p","mode":"dedicated"}`)
	if k.State != api.Ready || *k.Hostname != v2 {
		t.Fatalf("free host: %+v", k)
	}
	// Lapsing frees the host for a waiter without any release call: the next
	// reconcile pass binds it.
	e.Clock.Advance(16 * time.Minute)
	e.Pass()
	_, b = e.do(t, "GET", "/v1/claims?include_expired=true", tokJared, "")
	for _, c := range mustJSON[[]api.Claim](t, b) {
		if c.Id == "h3" && c.State != api.Pending {
			t.Fatalf("lapsed h3 changed: %+v", c)
		}
	}
}

func TestClaimConcurrentDedicatedOneBinds(t *testing.T) {
	e := setupClaims(t, nil)
	var wg sync.WaitGroup
	res := make([]api.Claim, 2)
	for i, tok := range []string{tokJared, tokAnn} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, c, b := e.putClaim(t, fmt.Sprint("d", i), tok, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`)
			if code != 200 {
				t.Errorf("%d %s", code, b)
			}
			res[i] = c
		}()
	}
	wg.Wait()
	ready, pending := 0, 0
	for _, c := range res {
		switch c.State {
		case api.Ready:
			ready++
		case api.Pending:
			pending++
		}
	}
	if ready != 1 || pending != 1 {
		t.Fatalf("ready=%d pending=%d: %+v", ready, pending, res)
	}
}

func TestClaimBindingFailures(t *testing.T) {
	e := setupClaims(t, nil)
	_, c, _ := e.putClaim(t, "lk", tokJared, `{"tags":["gpu"],"purpose":"p"}`)
	if c.State != api.Failed || c.Detail != vLock+": locked" || c.Hostname != nil {
		t.Fatalf("locked only: %+v", c)
	}
	_, c, _ = e.putClaim(t, "nh", tokJared, `{"tags":["tpu"],"purpose":"p"}`)
	if c.State != api.Failed || c.Detail != "no_host" {
		t.Fatalf("no host: %+v", c)
	}
	// Excluded hosts are dropped; if all matches are excluded the claim fails.
	e.do(t, "PUT", "/v1/hosts/"+v1+"/exclude", tokJared, `{"exclude":true}`)
	e.do(t, "PUT", "/v1/hosts/"+v2+"/exclude", tokJared, `{"exclude":true}`)
	_, c, _ = e.putClaim(t, "ex", tokJared, `{"tags":["kernel"],"purpose":"p"}`)
	if c.State != api.Failed || c.Detail != v1+": excluded; "+v2+": excluded" {
		t.Fatalf("excluded: %+v", c)
	}
	e.do(t, "PUT", "/v1/hosts/"+v2+"/exclude", tokJared, `{"exclude":false}`)
	_, c, _ = e.putClaim(t, "ok", tokJared, `{"tags":["kernel"],"purpose":"p"}`)
	if c.State != api.Ready || *c.Hostname != v2 {
		t.Fatalf("one usable host: %+v", c)
	}
	// Failed claims still count and can be released; listing shows them.
	_, b := e.do(t, "GET", "/v1/claims", tokJared, "")
	if n := len(mustJSON[[]api.Claim](t, b)); n != 4 {
		t.Fatalf("list: %d", n)
	}
}

func TestClaimDetached(t *testing.T) {
	e := setupClaims(t, nil)
	code, c, b := e.putClaim(t, "d", tokJared, `{"tags":["veritech01"],"purpose":"p","detached":true,"max_seconds":3600}`)
	if code != 200 || !c.Detached || c.LeaseUntil != nil || c.LeaseSeconds != nil || c.State != api.Ready {
		t.Fatalf("%d %s", code, b)
	}
	if !strings.Contains(string(b), `"lease_until":null`) || !strings.Contains(string(b), `"lease_seconds":null`) {
		t.Fatalf("lease fields must be null: %s", b)
	}
	e.Clock.Advance(50 * time.Minute)
	if _, b := e.do(t, "GET", "/v1/claims", tokJared, ""); len(mustJSON[[]api.Claim](t, b)) != 1 {
		t.Fatal("detached claim lapsed before expires_at")
	}
	code, c, b = e.putClaim(t, "d", tokJared, `{"tags":["veritech01"],"purpose":"p","detached":true}`)
	if code != 200 || c.LeaseUntil != nil || !c.ExpiresAt.Equal(apptest.T0.Add(time.Hour)) {
		t.Fatalf("renew: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/v1/claims/d/extend", tokJared, `{"max_seconds":7200}`); code != 200 {
		t.Fatalf("extend: %d %s", code, b)
	}
	e.Clock.Advance(2 * time.Hour)
	if code, _, b := e.putClaim(t, "d", tokJared, `{"tags":["veritech01"],"purpose":"p","detached":true}`); code != 410 {
		t.Fatalf("after cap: %d %s", code, b)
	}
	// Detached claims lapse only at expires_at; release ends one earlier.
	e.putClaim(t, "d2", tokJared, `{"tags":["veritech01"],"purpose":"p","detached":true}`)
	e.do(t, "DELETE", "/v1/claims/d2", tokJared, "")
	if code, _ := e.do(t, "GET", "/v1/claims/d2", tokJared, ""); code != 404 {
		t.Fatal("released detached claim")
	}
}

func TestListClaimsFilters(t *testing.T) {
	e := setupClaims(t, nil)
	e.putClaim(t, "a", tokJared, `{"tags":["veritech01"],"purpose":"p"}`)
	e.Clock.Advance(time.Second)
	e.putClaim(t, "b", tokAnn, `{"tags":["veritech02"],"purpose":"p"}`)
	e.Clock.Advance(time.Second)
	e.putClaim(t, "c", tokAnn, `{"tags":["veritech01"],"purpose":"p","mode":"dedicated"}`) // pending
	ids := func(q, tok string) string {
		code, b := e.do(t, "GET", "/v1/claims"+q, tok, "")
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
		var out []string
		for _, c := range mustJSON[[]api.Claim](t, b) {
			out = append(out, c.Id)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct{ q, tok, want string }{
		{"", tokJared, "a,b,c"},
		{"?owner=ann", tokJared, "b,c"},
		{"?mine=true", tokJared, "a"},
		{"?mine=true", tokAnn, "b,c"},
		{"?mine=true&owner=ann", tokAnn, "b,c"},
		{"?hostname=" + v1, tokJared, "a"},
		{"?hostname=" + v2 + "&owner=ann", tokJared, "b"},
		{"?hostname=nope", tokJared, ""},
	} {
		if got := ids(c.q, c.tok); got != c.want {
			t.Errorf("%q as %s: got %q want %q", c.q, c.tok[:4], got, c.want)
		}
	}
	for _, q := range []string{"?mine=true&owner=ann", "?mine=maybe"} {
		if code, _ := e.do(t, "GET", "/v1/claims"+q, tokJared, ""); code != 400 {
			t.Errorf("%q: %d, want 400", q, code)
		}
	}
}

func TestClaimAndHostEvents(t *testing.T) {
	e := setupClaims(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tokJared)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for e.App.Hub.Subscribers() == 0 {
		time.Sleep(time.Millisecond)
	}
	e.putClaim(t, "c1", tokJared, `{"tags":["veritech01"],"purpose":"p","mode":"reserve"}`)
	e.putClaim(t, "c2", tokJared, `{"tags":["veritech02"],"purpose":"p","mode":"dedicated"}`)
	e.Pass() // host events follow the pass
	e.do(t, "DELETE", "/v1/claims/c1", tokJared, "")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var claims []api.ClaimEvent
	blocked := map[string]bool{}
	for sc.Scan() && len(claims) < 3 {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		switch {
		case strings.Contains(line, `"type":"claim"`):
			claims = append(claims, mustJSON[api.ClaimEvent](t, []byte(line)))
		case strings.Contains(line, `"type":"host"`):
			h := mustJSON[api.HostEvent](t, []byte(line))
			if h.Host.JobsBlocked {
				blocked[h.Host.Hostname] = true
			}
			if !strings.Contains(line, `"jobs_blocked"`) || !strings.Contains(line, `"claims"`) {
				t.Errorf("host event lacks claims/jobs_blocked: %s", line)
			}
		}
	}
	if len(claims) != 3 || claims[0].Op != api.Put || claims[0].Claim == nil || claims[0].Claim.Id != "c1" ||
		claims[0].Type != api.ClaimEventTypeClaim || claims[2].Op != api.Delete || claims[2].Claim != nil || claims[2].Id != "c1" {
		t.Fatalf("claim events: %+v", claims)
	}
}
