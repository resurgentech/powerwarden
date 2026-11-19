package mcpserver_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/mcpserver"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

const sess = "s-0123456789ab"

type env struct {
	f   *pwfake.Fake
	k   *keeper.Keeper
	clk *keeper.FakeClock
	srv *mcp.Server
	cs  *mcp.ClientSession
	// done is closed when Serve returns
	done chan error
	stop context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := pwfake.New()
	t.Cleanup(f.Srv.Close)
	api, err := client.New(client.Config{URL: f.Srv.URL}, "tok", f.Srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	clk := keeper.NewFakeClock()
	k := keeper.New(api, keeper.Options{Prefix: "mcp", Session: sess, Clock: clk})
	t.Cleanup(k.Close)
	srv := mcpserver.New(mcpserver.Config{API: api, Keeper: k})
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{f: f, k: k, clk: clk, srv: srv, done: make(chan error, 1), stop: cancel}
	go func() { e.done <- mcpserver.Serve(ctx, srv, k, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.cs = cs
	t.Cleanup(func() { cancel(); _ = cs.Close() })
	return e
}

// call returns the first text block parsed as JSON (or raw text), the whole
// text, the isError flag and all text blocks.
func (e *env) call(t *testing.T, name string, args map[string]any) (res *mcp.CallToolResult, texts []string) {
	t.Helper()
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	if len(texts) == 0 {
		t.Fatalf("%s: no text content", name)
	}
	return res, texts
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON object: %q: %v", s, err)
	}
	return m
}

func putBody(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	r, ok := e.f.Last("PUT", "/v1/claims/"+id)
	if !ok {
		t.Fatalf("no PUT for %s", id)
	}
	return parse(t, string(r.Body))
}

func TestToolList(t *testing.T) {
	e := newEnv(t)
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, n := range []string{"list_hosts", "claim_host", "wait_for_claim", "extend_claim", "release_host", "power_host", "my_claims"} {
		if !got[n] {
			t.Errorf("missing tool %s (have %v)", n, got)
		}
	}
}

func TestClaimHostModes(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{{"", "hold"}, {"hold", "hold"}, {"reserve", "reserve"}, {"dedicated", "dedicated"}} {
		e := newEnv(t)
		args := map[string]any{"tags": []string{"box1", "kernel"}, "purpose": "build", "max_hours": 2}
		if tc.arg != "" {
			args["mode"] = tc.arg
		}
		res, texts := e.call(t, "claim_host", args)
		if res.IsError {
			t.Fatalf("mode %q: error %v", tc.arg, texts)
		}
		out := parse(t, texts[0])
		id, _ := out["id"].(string)
		if !strings.HasPrefix(id, "mcp-") {
			t.Fatalf("id %v", out)
		}
		if out["state"] != "pending" || out["mode"] != tc.want {
			t.Fatalf("mode %q out %v", tc.arg, out)
		}
		b := putBody(t, e, id)
		if b["mode"] != tc.want || b["session"] != sess || b["purpose"] != "build" {
			t.Fatalf("body %v", b)
		}
		if b["max_seconds"] != float64(7200) {
			t.Fatalf("max_seconds %v", b["max_seconds"])
		}
		if _, has := b["detached"]; has {
			t.Fatalf("detached sent: %v", b)
		}
		if _, has := b["hostname"]; has {
			t.Fatalf("hostname sent: %v", b)
		}
		if tags, _ := b["tags"].([]any); len(tags) != 2 {
			t.Fatalf("tags %v", b["tags"])
		}
		if e.f.Count("GET", "/v1/claims/"+id) != 0 {
			t.Fatal("claim_host blocked/polled")
		}
	}
}

func TestClaimHostRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	for name, args := range map[string]map[string]any{
		"no tags":    {"tags": []string{}, "purpose": "p", "max_hours": 1},
		"no purpose": {"tags": []string{"a"}, "purpose": "", "max_hours": 1},
		"bad mode":   {"tags": []string{"a"}, "purpose": "p", "max_hours": 1, "mode": "owned"},
		"zero hours": {"tags": []string{"a"}, "purpose": "p", "max_hours": 0},
	} {
		res, texts := e.call(t, "claim_host", args)
		if !res.IsError {
			t.Errorf("%s: want error, got %v", name, texts)
		}
	}
	if n := len(e.f.Requests()); n != 0 {
		t.Fatalf("%d requests sent for bad input", n)
	}
}

func TestClaimHostRefusal(t *testing.T) {
	e := newEnv(t)
	e.f.Respond = func(r pwfake.Req) (int, any, bool) {
		return 429, pwfake.Err(pw.ErrorCodeCapacity, "too many claims"), true
	}
	res, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	if !res.IsError || !strings.Contains(texts[0], "capacity") || !strings.Contains(texts[0], "too many claims") {
		t.Fatalf("%v %v", res.IsError, texts)
	}
}

func TestListHosts(t *testing.T) {
	e := newEnv(t)
	lu := time.Now().Add(time.Hour)
	e.f.Hosts = []pw.HostView{{Hostname: "box1.labtwo", NameTags: []string{"box1.labtwo", "box1"}, Tags: []string{"kernel"},
		State: pw.SoftOff, JobsBlocked: true,
		Claims: []pw.HostClaim{{Id: "mcp-x", Owner: "jared@box", Session: sess, Mode: pw.Reserve, State: pw.Ready, LeaseUntil: &lu, ExpiresAt: lu}}}}
	res, texts := e.call(t, "list_hosts", map[string]any{"tags": []string{"kernel", "box1"}})
	if res.IsError {
		t.Fatal(texts)
	}
	r, _ := e.f.Last("GET", "/v1/hosts")
	if got := r.Query["tags"]; len(got) != 1 || got[0] != "kernel,box1" {
		t.Fatalf("tags query %v", got)
	}
	s := texts[0]
	for _, want := range []string{"box1.labtwo", `"box1"`, "kernel", "soft_off", `"jobs_blocked":true`, "mcp-x", "reserve"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q: %s", want, s)
		}
	}
	if !strings.Contains(s, `"this_session":true`) {
		t.Errorf("claim not marked as this session: %s", s)
	}
}

func TestWaitForClaimPassesSince(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)

	res, texts := e.call(t, "wait_for_claim", map[string]any{"claim_id": id})
	if res.IsError {
		t.Fatal(texts)
	}
	r, _ := e.f.Last("GET", "/v1/claims/"+id)
	if r.Query["since"][0] != "1" || r.Query["wait"][0] != "60" {
		t.Fatalf("query %v", r.Query)
	}
	// server moves to version 2 / ready; the next wait reports it and remembers it
	e.f.Claims[id].Version = 2
	e.f.Claims[id].State = pw.Ready
	_, texts = e.call(t, "wait_for_claim", map[string]any{"claim_id": id})
	if parse(t, texts[0])["state"] != "ready" {
		t.Fatalf("%v", texts)
	}
	_, _ = e.call(t, "wait_for_claim", map[string]any{"claim_id": id})
	r, _ = e.f.Last("GET", "/v1/claims/"+id)
	if r.Query["since"][0] != "2" {
		t.Fatalf("since after v2 = %v", r.Query["since"])
	}
}

func TestWaitForClaimGone(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)
	e.f.Respond = func(r pwfake.Req) (int, any, bool) {
		if r.Method == "GET" {
			return 410, pwfake.Err(pw.ErrorCodeClaimExpired, "lapsed"), true
		}
		return 0, nil, false
	}
	res, texts := e.call(t, "wait_for_claim", map[string]any{"claim_id": id})
	if !res.IsError || !strings.Contains(texts[0], "claim_expired") {
		t.Fatalf("%v %v", res.IsError, texts)
	}
	if len(e.k.Lost()) != 1 {
		t.Fatal("410 on wait should mark the claim lost")
	}
}

func TestExtendClaim(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)
	res, texts := e.call(t, "extend_claim", map[string]any{"claim_id": id, "hours": 3})
	if res.IsError {
		t.Fatal(texts)
	}
	r, ok := e.f.Last("POST", "/v1/claims/"+id+"/extend")
	if !ok || parse(t, string(r.Body))["max_seconds"] != float64(10800) {
		t.Fatalf("extend body %s", r.Body)
	}
	res, _ = e.call(t, "extend_claim", map[string]any{"claim_id": id, "hours": -1})
	if !res.IsError {
		t.Fatal("negative hours accepted")
	}
}

func TestReleaseHost(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)
	res, texts := e.call(t, "release_host", map[string]any{"claim_id": id})
	if res.IsError {
		t.Fatal(texts)
	}
	if e.f.Count("DELETE", "/v1/claims/"+id) != 1 || e.k.Owns(id) {
		t.Fatal("not released")
	}
}

func TestPowerHost(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1, "mode": "dedicated"})
	id := parse(t, texts[0])["id"].(string)
	res, texts := e.call(t, "power_host", map[string]any{"claim_id": id, "action": "powerup"})
	if res.IsError {
		t.Fatal(texts)
	}
	r, _ := e.f.Last("POST", "/v1/claims/"+id+"/power")
	if parse(t, string(r.Body))["action"] != "powerup" {
		t.Fatalf("body %s", r.Body)
	}
	if !strings.Contains(texts[0], "queued") || !strings.Contains(texts[0], "powerup") {
		t.Fatalf("command not returned: %v", texts)
	}
}

func TestPowerHostRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   pw.ErrorCode
		msg    string
	}{
		{409, pw.ErrorCodeHostShared, "another owner has a claim"},
		{409, pw.ErrorCodeWorkerBusy, "worker busy"},
		{409, pw.ErrorCodeActionNotAllowed, "hold claims may not powerdown"},
		{409, pw.ErrorCodeClaimNotReady, "not bound"},
		{410, pw.ErrorCodeClaimExpired, "lapsed"},
		{502, pw.ErrorCodePowermasterUnreachable, "powermaster down"},
	} {
		e := newEnv(t)
		_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
		id := parse(t, texts[0])["id"].(string)
		e.f.Respond = func(r pwfake.Req) (int, any, bool) {
			if strings.HasSuffix(r.Path, "/power") {
				return tc.status, pwfake.Err(tc.code, tc.msg), true
			}
			return 0, nil, false
		}
		res, texts := e.call(t, "power_host", map[string]any{"claim_id": id, "action": "reboot"})
		if !res.IsError || !strings.Contains(texts[0], string(tc.code)) || !strings.Contains(texts[0], tc.msg) {
			t.Errorf("%s: isError=%v texts=%v", tc.code, res.IsError, texts)
		}
	}
}

func TestMyClaims(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.f.SetClaim(&pw.Claim{Id: "other-1", Owner: "jared@box", Session: "s-other", Mode: pw.Hold, State: pw.Ready, Tags: []string{"x"}, ExpiresAt: now, Version: 3})
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	mine := parse(t, texts[0])["id"].(string)
	res, texts := e.call(t, "my_claims", nil)
	if res.IsError {
		t.Fatal(texts)
	}
	r, _ := e.f.Last("GET", "/v1/claims")
	if r.Query["mine"][0] != "true" {
		t.Fatalf("query %v", r.Query)
	}
	var out struct {
		Claims []map[string]any `json:"claims"`
	}
	if err := json.Unmarshal([]byte(texts[0]), &out); err != nil || len(out.Claims) != 2 {
		t.Fatalf("%v %s", err, texts[0])
	}
	for _, c := range out.Claims {
		want := c["id"] == mine
		if got, _ := c["this_session"].(bool); got != want {
			t.Errorf("claim %v this_session=%v want %v", c["id"], got, want)
		}
	}
}

func TestLossWarning(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	if len(texts) != 1 {
		t.Fatalf("unexpected warning on healthy call: %v", texts)
	}
	id := parse(t, texts[0])["id"].(string)
	deadline := time.Now().Add(3 * time.Second)
	for e.clk.Waiters() != 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	e.f.Respond = func(r pwfake.Req) (int, any, bool) {
		if r.Method == "PUT" {
			return 410, pwfake.Err(pw.ErrorCodeClaimExpired, "claim lapsed"), true
		}
		return 0, nil, false
	}
	e.clk.Advance(time.Hour)
	for len(e.k.Lost()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// every later tool result carries the warning
	for _, tool := range []string{"list_hosts", "my_claims"} {
		_, texts = e.call(t, tool, nil)
		last := texts[len(texts)-1]
		if !strings.Contains(last, "WARNING") || !strings.Contains(last, id) {
			t.Fatalf("%s: no warning naming %s: %v", tool, id, texts)
		}
	}
	// release_host acknowledges
	e.f.Respond = nil
	_, _ = e.call(t, "release_host", map[string]any{"claim_id": id})
	_, texts = e.call(t, "list_hosts", nil)
	for _, s := range texts {
		if strings.Contains(s, "WARNING") {
			t.Fatalf("warning persists after release: %v", texts)
		}
	}
}

func TestReleaseOnEOF(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := 0; i < 2; i++ {
		_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
		ids = append(ids, parse(t, texts[0])["id"].(string))
	}
	_ = e.cs.Close() // client side closes: the server sees EOF
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on EOF")
	}
	for _, id := range ids {
		if e.f.Count("DELETE", "/v1/claims/"+id) != 1 {
			t.Errorf("claim %s not released on EOF", id)
		}
	}
}

func TestReleaseOnSIGTERM(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)
	e.stop() // what signal.NotifyContext does on SIGTERM
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on cancel")
	}
	if e.f.Count("DELETE", "/v1/claims/"+id) != 1 {
		t.Fatal("claim not released on SIGTERM")
	}
}
