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
	"regexp"
	"strings"
	"testing"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/apptest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

const (
	tokHourchd = "hourchd-token-0123456789"
	tokJared   = "jared-token-0123456789ab"
	v1         = "veritech01.labtwo"
)

type tenv struct {
	*apptest.Env
	srv *httptest.Server
	mux *http.ServeMux
}

func setup(t *testing.T, tweak func(*config.Config)) *tenv {
	t.Helper()
	return setupKeyRequest(t, "", tweak)
}

// setupKeyRequest is setup with the Server's KeyRequestURL set.
func setupKeyRequest(t *testing.T, keyURL string, tweak func(*config.Config)) *tenv {
	t.Helper()
	seed := func(c *config.Config) {
		c.HostTags = map[string][]string{v1: {"kernel"}}
		if tweak != nil {
			tweak(c)
		}
	}
	env := apptest.New(t, seed, pm.Host{Hostname: v1, State: pm.StateSoftOff})
	toks, err := NewTokens(map[string]string{"hourchd": tokHourchd, "jared": tokJared})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{App: env.App, Tokens: toks, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), KeyRequestURL: keyURL}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &tenv{Env: env, srv: srv, mux: mux}
}

func (e *tenv) do(t *testing.T, method, path, token, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func mustJSON[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// W4: idempotent PUT; omitted lease_seconds keeps stored.
func TestPutDemandIdempotentLeaseKept(t *testing.T) {
	e := setup(t, nil)
	body := `{"id":"hourchd:01","tags":["kernel"],"lease_seconds":120,"source":"hourchd","external_ref":"run x"}`
	code, b := e.do(t, "PUT", "/v1/demands/hourchd:01", tokHourchd, body)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	first := mustJSON[api.Demand](t, b)
	e.Clock.Advance(10 * time.Second)
	code, b = e.do(t, "PUT", "/v1/demands/hourchd:01", tokHourchd, body)
	again := mustJSON[api.Demand](t, b)
	if code != 200 || !again.CreatedAt.Equal(first.CreatedAt) || again.LeaseSeconds != 120 {
		t.Fatalf("not idempotent: %d %+v", code, again)
	}
	_, b = e.do(t, "PUT", "/v1/demands/hourchd:01", tokHourchd, `{"tags":["kernel"],"source":"hourchd"}`)
	refreshed := mustJSON[api.Demand](t, b)
	if refreshed.LeaseSeconds != 120 || !refreshed.LeaseUntil.Equal(apptest.T0.Add(130*time.Second)) {
		t.Fatalf("omitted lease_seconds should keep 120: %+v", refreshed)
	}
	if code, _ := e.do(t, "GET", "/v1/demands", tokHourchd, ""); code != 200 {
		t.Fatal("list failed")
	}
}

func TestPutDemandBadRequests(t *testing.T) {
	e := setup(t, nil)
	cases := map[string]struct{ path, body string }{
		"unknown field": {"/v1/demands/a", `{"tags":[],"source":"manual","bogus":1}`},
		"id mismatch":   {"/v1/demands/a", `{"id":"b","tags":[],"source":"manual"}`},
		"bad id":        {"/v1/demands/a%20b", `{"tags":[],"source":"manual"}`},
		"bad tag":       {"/v1/demands/a", `{"tags":["no way"],"source":"manual"}`},
		"bad source":    {"/v1/demands/a", `{"tags":[],"source":"redis"}`},
		"lease bound":   {"/v1/demands/a", `{"tags":[],"source":"manual","lease_seconds":5}`},
		"missing tags":  {"/v1/demands/a", `{"source":"manual"}`},
		"not json":      {"/v1/demands/a", `nope`},
		"trailing":      {"/v1/demands/a", `{"tags":[],"source":"manual"} {}`},
	}
	for name, c := range cases {
		code, b := e.do(t, "PUT", c.path, tokJared, c.body)
		if code != 400 || mustJSON[api.Error](t, b).Error != api.ErrorCodeBadRequest {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
}

// W4: expired demand hidden but expired worker kept inside busy_grace.
func TestExpiredDemandHiddenExpiredWorkerKept(t *testing.T) {
	e := setup(t, nil)
	e.do(t, "PUT", "/v1/demands/d1", tokJared, `{"tags":["kernel"],"source":"manual","lease_seconds":30}`)
	code, b := e.do(t, "PUT", "/v1/workers/w1", tokHourchd,
		`{"hostname":"`+v1+`","tags":["kernel"],"job_status":"busy","demand_id":"d1","lease_seconds":15}`)
	if code != 200 || mustJSON[api.Worker](t, b).LastBusyAt == nil {
		t.Fatalf("%d %s", code, b)
	}
	e.Clock.Advance(time.Minute)
	if _, b := e.do(t, "GET", "/v1/demands", tokJared, ""); len(mustJSON[[]api.Demand](t, b)) != 0 {
		t.Fatal("expired demand listed")
	}
	if code, _ := e.do(t, "GET", "/v1/demands/d1", tokJared, ""); code != 404 {
		t.Fatal("expired demand fetched without include_expired")
	}
	if _, b := e.do(t, "GET", "/v1/demands?include_expired=true", tokJared, ""); len(mustJSON[[]api.Demand](t, b)) != 1 {
		t.Fatal("include_expired lost the demand")
	}
	if _, b := e.do(t, "GET", "/v1/workers", tokJared, ""); len(mustJSON[[]api.Worker](t, b)) != 0 {
		t.Fatal("expired worker listed")
	}
	if _, b := e.do(t, "GET", "/v1/workers?include_expired=true", tokJared, ""); len(mustJSON[[]api.Worker](t, b)) != 1 {
		t.Fatal("expired worker dropped")
	}
	e.PM.SetState(v1, pm.StateOn)
	e.App.Rec.SyncHosts(context.Background())
	e.Pass()
	_, b = e.do(t, "GET", "/v1/hosts/"+v1, tokJared, "")
	hv := mustJSON[api.HostView](t, b)
	if hv.BusyHeldUntil == nil || len(hv.Workers) != 1 || hv.Workers[0] != "w1" {
		t.Fatalf("expired busy worker should hold the host: %+v", hv)
	}
}

// W4: DELETE 204 when absent.
func TestDelete204(t *testing.T) {
	e := setup(t, nil)
	for _, p := range []string{"/v1/demands/never", "/v1/workers/never"} {
		if code, b := e.do(t, "DELETE", p, tokJared, ""); code != 204 || len(b) != 0 {
			t.Errorf("%s: %d %s", p, code, b)
		}
	}
}

// W4: 401 with an identical body for missing and wrong tokens; healthz needs none.
func TestUnauthorized(t *testing.T) {
	e := setup(t, nil)
	c1, b1 := e.do(t, "GET", "/v1/demands", "", "")
	c2, b2 := e.do(t, "GET", "/v1/demands", "wrong-token-0123456789", "")
	if c1 != 401 || c2 != 401 || string(b1) != string(b2) {
		t.Fatalf("%d %s / %d %s", c1, b1, c2, b2)
	}
	if mustJSON[api.Error](t, b1).Error != api.ErrorCodeUnauthorized {
		t.Fatal("wrong code")
	}
	code, b := e.do(t, "GET", "/v1/healthz", "", "")
	h := mustJSON[api.Healthz](t, b)
	if code != 200 || h.Status != api.Ok || h.Powermaster != api.Connected {
		t.Fatalf("healthz %d %s", code, b)
	}
	if strings.Contains(string(b), v1) {
		t.Fatal("healthz leaks inventory")
	}
	if code, _ := e.do(t, "GET", "/v1/events", "", ""); code != 401 {
		t.Fatal("events without token")
	}
}

// W4: 429 at capacity for a new id, while refreshing an existing id works.
func TestCapacity(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Limits.MaxDemands = 1 })
	if code, _ := e.do(t, "PUT", "/v1/demands/a", tokJared, `{"tags":[],"source":"manual"}`); code != 200 {
		t.Fatal("first demand rejected")
	}
	code, b := e.do(t, "PUT", "/v1/demands/b", tokJared, `{"tags":[],"source":"manual"}`)
	if code != 429 || mustJSON[api.Error](t, b).Error != api.ErrorCodeCapacity {
		t.Fatalf("%d %s", code, b)
	}
	if code, _ := e.do(t, "PUT", "/v1/demands/a", tokJared, `{"tags":[],"source":"manual"}`); code != 200 {
		t.Fatal("refresh at capacity rejected")
	}
}

// W4: unknown-hostname worker stored (and flagged by the reconciler); covers nothing.
func TestUnknownHostnameWorker(t *testing.T) {
	e := setup(t, nil)
	e.do(t, "PUT", "/v1/demands/d1", tokJared, `{"tags":["tiny"],"source":"manual"}`)
	code, _ := e.do(t, "PUT", "/v1/workers/tiny1", tokHourchd, `{"hostname":"sdf1.labtwo","tags":["tiny"],"job_status":"idle"}`)
	if code != 200 {
		t.Fatal("unknown-hostname worker rejected")
	}
	res := e.Pass()
	if len(res.Output.UnknownHostWorkers) != 1 || res.Output.CoveredBy["d1"] != "" {
		t.Fatalf("unknown host worker: %+v", res.Output)
	}
	_, b := e.do(t, "GET", "/v1/workers", tokJared, "")
	if ws := mustJSON[[]api.Worker](t, b); len(ws) != 1 || ws[0].Hostname != "sdf1.labtwo" {
		t.Fatalf("not stored: %s", b)
	}
	_, b = e.do(t, "GET", "/v1/demands/d1", tokJared, "")
	if mustJSON[api.Demand](t, b).CoveredBy != nil {
		t.Fatal("unknown-host worker covered a demand")
	}
}

func TestWorkerAndExclude(t *testing.T) {
	e := setup(t, nil)
	code, b := e.do(t, "PUT", "/v1/workers/w1", tokHourchd, `{"worker_id":"other","hostname":"h","tags":[],"job_status":"idle"}`)
	if code != 400 {
		t.Fatalf("worker_id mismatch: %d %s", code, b)
	}
	code, b = e.do(t, "PUT", "/v1/hosts/"+v1+"/exclude", tokJared, `{"exclude":true}`)
	if code != 200 || !mustJSON[api.HostView](t, b).Exclude {
		t.Fatalf("%d %s", code, b)
	}
	if code, _ := e.do(t, "PUT", "/v1/hosts/nope.labtwo/exclude", tokJared, `{"exclude":true}`); code != 404 {
		t.Fatal("unknown host exclude not 404")
	}
	_, b = e.do(t, "GET", "/v1/hosts", tokJared, "")
	hs := mustJSON[[]api.HostView](t, b)
	if len(hs) != 1 || !hs[0].Exclude {
		t.Fatalf("hosts %s", b)
	}
	for _, bad := range []string{"ip", "password", "ssh_key", "pdu"} {
		if strings.Contains(string(b), `"`+bad) {
			t.Fatalf("host view leaks %s", bad)
		}
	}
}

func TestDecisionsAndStats(t *testing.T) {
	e := setup(t, nil)
	e.do(t, "PUT", "/v1/demands/d1", tokHourchd, `{"tags":["kernel"],"source":"hourchd"}`)
	e.Pass()
	_, b := e.do(t, "GET", "/v1/decisions", tokJared, "")
	ds := mustJSON[[]api.Decision](t, b)
	if len(ds) == 0 || ds[0].Action != api.PowerupRequested || *ds[0].Hostname != v1 || *ds[0].DemandId != "d1" {
		t.Fatalf("decisions %s", b)
	}
	_, b = e.do(t, "GET", "/v1/stats", tokJared, "")
	st := mustJSON[api.Stats](t, b)
	if st.DemandsOpen != 1 || st.DemandsCovered != 1 ||
		st.LastPowerRequest == nil || st.LastPowerRequest.Hostname != v1 || st.HostsByPolicy["booting"] != 1 {
		t.Fatalf("stats %s", b)
	}
}

// Events: demand put and delete reach /v1/events with seq and op.
func TestEventsStream(t *testing.T) {
	e := setup(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	e.do(t, "PUT", "/v1/demands/d1", tokJared, `{"tags":[],"source":"manual"}`)
	e.do(t, "DELETE", "/v1/demands/d1", tokJared, "")
	sc := bufio.NewScanner(resp.Body)
	var got []api.DemandEvent
	for len(got) < 2 && sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			got = append(got, mustJSON[api.DemandEvent](t, []byte(line)))
		}
	}
	if len(got) != 2 || got[0].Op != api.Put || got[0].Demand == nil || got[1].Op != api.Delete ||
		got[1].Demand != nil || got[1].Seq <= got[0].Seq || got[0].Type != api.DemandEventTypeDemand {
		t.Fatalf("events %+v", got)
	}
}

// Every operation in specs/openapi/powerwarden.yml has a mux route.
func TestEverySpecOperationHasRoute(t *testing.T) {
	e := setup(t, nil)
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "specs", "openapi", "powerwarden.yml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	n := 0
	for path, item := range doc.Paths.Map() {
		if !strings.Contains(string(b), "\n  "+path+":") {
			t.Errorf("embedded path %s not in spec file", path)
		}
		for method := range item.Operations() {
			n++
			req := httptest.NewRequest(method, param.ReplaceAllString(path, "x"), nil)
			if _, pattern := e.mux.Handler(req); pattern == "" {
				t.Errorf("no route for %s %s", method, path)
			}
		}
	}
	if n < 13 {
		t.Fatalf("only %d operations found", n)
	}
}

// Host tags come from the config seed and are replaced through the API.
func TestHostTags(t *testing.T) {
	e := setup(t, nil)
	code, b := e.do(t, "GET", "/v1/hosts/"+v1, tokJared, "")
	if code != 200 || !strings.Contains(string(b), `"tags":["kernel"]`) {
		t.Fatalf("seeded host = %d %s", code, b)
	}
	code, b = e.do(t, "PUT", "/v1/hosts/"+v1+"/tags", tokJared, `{"tags":["tiny","big"]}`)
	if code != 200 || !strings.Contains(string(b), `"tags":["big","tiny"]`) {
		t.Fatalf("put tags = %d %s", code, b)
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/hosts/nope.labtwo/tags", `{"tags":["kernel"]}`, 404},
		{"/v1/hosts/" + v1 + "/tags", `{}`, 400},
		{"/v1/hosts/" + v1 + "/tags", `{"tags":["bad tag"]}`, 400},
	} {
		if code, b := e.do(t, "PUT", c.path, tokJared, c.body); code != c.want {
			t.Fatalf("PUT %s %s = %d %s, want %d", c.path, c.body, code, b, c.want)
		}
	}
	if code, _ := e.do(t, "PUT", "/v1/hosts/"+v1+"/tags", "", `{"tags":[]}`); code != 401 {
		t.Fatal("unauthenticated tags put not 401")
	}
}

func TestHostNameTags(t *testing.T) {
	e := setup(t, nil)
	_, b := e.do(t, "GET", "/v1/hosts/"+v1, tokJared, "")
	if !strings.Contains(string(b), `"tags":["kernel"]`) || !strings.Contains(string(b), `"name_tags":["veritech01","veritech01.labtwo"]`) {
		t.Fatalf("host view = %s", b)
	}
	for _, tag := range []string{"veritech01", v1} {
		code, b := e.do(t, "PUT", "/v1/hosts/"+v1+"/tags", tokJared, `{"tags":["kernel","`+tag+`"]}`)
		if code != 400 {
			t.Fatalf("PUT tag %q = %d %s, want 400", tag, code, b)
		}
	}
	// Reserved against any known host, not only the one being edited.
	e.PM.AddHost(pm.Host{Hostname: "veritech02.labtwo", State: pm.StateSoftOff})
	hosts, _ := e.PM.ListHosts(context.Background())
	e.App.View.Replace(hosts, e.Clock.Now())
	if code, b := e.do(t, "PUT", "/v1/hosts/"+v1+"/tags", tokJared, `{"tags":["veritech02"]}`); code != 400 {
		t.Fatalf("PUT other host's short name = %d %s, want 400", code, b)
	}
	if code, b := e.do(t, "PUT", "/v1/hosts/"+v1+"/tags", tokJared, `{"tags":["kernel2"]}`); code != 200 {
		t.Fatalf("PUT ordinary tag = %d %s", code, b)
	}
}
