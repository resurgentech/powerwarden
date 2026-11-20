package keyreq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

func testNotice() request.Notice {
	return request.Notice{
		Service: "powerwarden.example.com", ApproveURL: "https://keys.example.com/requests?code=ABCD-EFGH",
		UserCode: "ABCD-EFGH", Role: "agent", Tool: "pwrwrdn/0.2.0",
		ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// Every SR-9 string, pinned character for character. The expected values are
// literals, not built from the package's constants.
func TestSR9StringsPinned(t *testing.T) {
	n := testNotice()
	const tok = "/home/u/.config/powerwarden/token"
	for _, tc := range []struct {
		name  string
		err   error
		lines []string
		code  int
	}{
		{"pending", &request.ErrPending{Notice: n},
			[]string{n.String(), "key request pending; run this again after approving"}, 75},
		{"busy", request.ErrBusy,
			[]string{"another process is handling the key request for /home/u/.config/powerwarden/token; try again shortly"}, 75},
		{"denied", request.ErrDenied, []string{"key request denied"}, 1},
		{"expired", request.ErrExpired,
			[]string{"key request: expired", "run this again to file a new request"}, 1},
		{"consumed", request.ErrConsumed,
			[]string{"key request: the key was already handed out and not received; request again", "run this again to file a new request"}, 1},
		{"unauthorized", request.ErrUnauthorized, []string{"key request: poll refused as unauthorized"}, 1},
		{"provider", &request.ProviderError{Status: 500, Message: "boom"},
			[]string{"key provider refused the request (status 500): boom"}, 1},
		{"provider retry after rounds up", &request.ProviderError{Status: 429, Message: "slow", RetryAfter: 1500 * time.Millisecond},
			[]string{"key provider refused the request (status 429): slow (retry after 1.5s)", "try again in 2s"}, 1},
		{"provider retry after whole", &request.ProviderError{Status: 429, RetryAfter: 30 * time.Second},
			[]string{"key provider refused the request (status 429) (retry after 30s)", "try again in 30s"}, 1},
		{"wrapped denied", fmt.Errorf("wrapped: %w", request.ErrDenied), []string{"key request denied"}, 1},
	} {
		lines, code, ok := Classify(tc.err, tok)
		if !ok || code != tc.code || strings.Join(lines, "\n") != strings.Join(tc.lines, "\n") {
			t.Errorf("%s: ok=%v code=%d lines=%q; want code=%d lines=%q", tc.name, ok, code, lines, tc.code, tc.lines)
		}
	}
	for _, err := range []error{request.ErrNoProvider, context.Canceled, errors.New("key request: Role is required"), nil} {
		if _, _, ok := Classify(err, tok); ok {
			t.Errorf("%v must not be classified (normal error path)", err)
		}
	}
}

func TestMCPTextsPinned(t *testing.T) {
	n := testNotice()
	want := "No powerwarden key on this machine yet. " + n.String() + " Then retry."
	if got := PendingMCPText(n); got != want {
		t.Fatalf("pending text %q", got)
	}
	if !strings.HasPrefix(want, "No powerwarden key on this machine yet. Approve this key request for powerwarden.example.com at https://keys.example.com/requests?code=ABCD-EFGH and check that the code ABCD-EFGH matches.") ||
		!strings.HasSuffix(want, " The request expires at 2030-01-02T03:04:05Z. Then retry.") {
		t.Fatalf("unexpected shape: %q", want)
	}
	if MCPBusyText != "another pwrwrdn is handling the key request; retry shortly" {
		t.Fatalf("busy text %q", MCPBusyText)
	}
	if DefaultRole != "agent" || ExitTempFail != 75 || client.EnvKeyRequestURL != "POWERWARDEN_KEY_REQUEST_URL" || client.Version != "0.2.0" {
		t.Fatal("constants drifted")
	}
}

func TestOptionsMapping(t *testing.T) {
	o := Options(Params{ServiceURL: "https://powerwarden.example.com", Override: "https://keys.example.com/r", TokenPath: "/t/token",
		Role: "", NameHint: "u@box", Wait: true})
	if o.ServiceURL != "https://powerwarden.example.com" || o.Override != "https://keys.example.com/r" || o.TokenPath != "/t/token" ||
		o.Role != "agent" || o.NameHint != "u@box" || o.Tool != "pwrwrdn/0.2.0" || o.Host != "" || o.AllowInsecureHTTP || !o.Wait || o.Notify != nil {
		t.Fatalf("%+v", o)
	}
	if o := Options(Params{Role: "ops"}); o.Role != "ops" {
		t.Fatalf("role %q", o.Role)
	}
}

// ---- retry transport ----

func shrinkDelay(t *testing.T) {
	t.Helper()
	old := RetryDelay
	RetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { RetryDelay = old })
}

// statusServer answers with the statuses in order, then 200; it counts requests.
func statusServer(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32, *[][]byte) {
	t.Helper()
	var n atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		i := int(n.Add(1)) - 1
		if i < len(statuses) {
			w.WriteHeader(statuses[i])
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &bodies
}

func do(t *testing.T, hc *http.Client, url string) int {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, bytes.NewReader([]byte("payload")))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRetryArmed401Then200(t *testing.T) {
	shrinkDelay(t)
	srv, n, bodies := statusServer(t, 401)
	hc := WrapRetry(&http.Client{}, true)
	if got := do(t, hc, srv.URL); got != 200 || n.Load() != 2 {
		t.Fatalf("status %d after %d requests", got, n.Load())
	}
	for i, b := range *bodies {
		if string(b) != "payload" {
			t.Fatalf("request %d body %q: the retry must resend the body", i, b)
		}
	}
}

func TestRetryArmed401Then401IsNotRetriedAgain(t *testing.T) {
	shrinkDelay(t)
	srv, n, _ := statusServer(t, 401, 401, 401)
	hc := WrapRetry(&http.Client{}, true)
	if got := do(t, hc, srv.URL); got != 401 || n.Load() != 2 {
		t.Fatalf("status %d after %d requests; want 401 after exactly 2", got, n.Load())
	}
}

func TestRetryUnarmedPassesThrough(t *testing.T) {
	shrinkDelay(t)
	srv, n, _ := statusServer(t, 401)
	hc := WrapRetry(&http.Client{}, false)
	if got := do(t, hc, srv.URL); got != 401 || n.Load() != 1 {
		t.Fatalf("status %d after %d requests; a key that was already on disk is never retried", got, n.Load())
	}
}

func TestRetryOnlyTheFirstCall(t *testing.T) {
	shrinkDelay(t)
	srv, n, _ := statusServer(t, 200, 401, 200)
	hc := WrapRetry(&http.Client{}, true)
	if got := do(t, hc, srv.URL); got != 200 {
		t.Fatalf("first: %d", got)
	}
	if got := do(t, hc, srv.URL); got != 401 || n.Load() != 2 {
		t.Fatalf("second call got %d after %d requests; the second call is never retried", got, n.Load())
	}
}

func TestRetryNot401FirstThen401(t *testing.T) {
	shrinkDelay(t)
	srv, n, _ := statusServer(t, 500, 401)
	hc := WrapRetry(&http.Client{}, true)
	if got := do(t, hc, srv.URL); got != 500 {
		t.Fatal(got)
	}
	if got := do(t, hc, srv.URL); got != 401 || n.Load() != 2 {
		t.Fatalf("a failed first call (any outcome) disarms: got %d after %d", got, n.Load())
	}
}

func TestRetryWaitsRetryDelayAndHonoursContext(t *testing.T) {
	old := RetryDelay
	RetryDelay = time.Hour
	t.Cleanup(func() { RetryDelay = old })
	srv, n, _ := statusServer(t, 401)
	hc := WrapRetry(&http.Client{}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	start := time.Now()
	resp, err := hc.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected the context to end the wait")
	}
	if time.Since(start) > 5*time.Second || n.Load() != 1 {
		t.Fatalf("waited %s, %d requests", time.Since(start), n.Load())
	}
}

func TestRetryDelayDefault(t *testing.T) {
	if RetryDelay != 5*time.Second {
		t.Fatalf("RetryDelay %s", RetryDelay)
	}
}

func TestRetryConcurrentCallsWaitForTheFirst(t *testing.T) {
	shrinkDelay(t)
	var first atomic.Bool
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/first" && first.CompareAndSwap(false, true) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	hc := WrapRetry(&http.Client{}, true)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("GET", srv.URL+"/first", nil)
		resp, err := hc.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("first: %v", err)
			return
		}
		resp.Body.Close()
	}()
	time.Sleep(30 * time.Millisecond)
	req, _ := http.NewRequest("GET", srv.URL+"/other", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "/first,/first,/other" {
		t.Fatalf("order %v: the other call must wait for the first call and its retry", order)
	}
}

// ---- the MCP gate ----

type gateRig struct {
	t    *testing.T
	p    *keyclienttest.Provider
	f    *pwfake.Fake
	tok  string
	gate *Gate
}

func newGateRig(t *testing.T) *gateRig {
	t.Helper()
	shrinkDelay(t)
	f := pwfake.New()
	t.Cleanup(f.Srv.Close)
	p := keyclienttest.NewProvider(t)
	tok := filepath.Join(t.TempDir(), "cfg", "token")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g, err := NewGate(ctx, client.Config{URL: f.Srv.URL}, Params{ServiceURL: f.Srv.URL, Override: p.RequestURL(), TokenPath: tok, NameHint: "tester@box"})
	if err != nil {
		t.Fatal(err)
	}
	return &gateRig{t: t, p: p, f: f, tok: tok, gate: g}
}

func (r *gateRig) connect() (client.API, error) { return r.gate.Connect(context.Background()) }

func (r *gateRig) untilNotPending() (client.API, error) {
	r.t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		api, err := r.connect()
		if err == nil || !strings.HasPrefix(err.Error(), "No powerwarden key on this machine yet. ") {
			return api, err
		}
	}
	r.t.Fatal("request never settled")
	return nil, nil
}

func TestGatePendingThenKey(t *testing.T) {
	r := newGateRig(t)
	r.p.Script(keyclienttest.Approved())
	_, err := r.connect()
	if err == nil || !strings.HasPrefix(err.Error(), "No powerwarden key on this machine yet. Approve this key request") || !strings.HasSuffix(err.Error(), " Then retry.") {
		t.Fatalf("%v", err)
	}
	first := err.Error()
	if _, err := r.connect(); err == nil || err.Error() != first {
		t.Fatalf("second call: %v", err)
	}
	if n := len(r.p.Filed()); n != 1 {
		t.Fatalf("filed %d", n)
	}
	if strings.Contains(first, r.p.PollSecret()) || strings.Contains(first, r.p.Key()) {
		t.Fatal("secret in the MCP text")
	}
	// the first filing carries the defaults
	if f := r.p.Filed()[0]; f.Role != "agent" || f.NameHint != "tester@box" || f.Tool != "pwrwrdn/0.2.0" {
		t.Fatalf("%+v", f)
	}
	// 401 once on the first service call after the key landed: retried once
	var hits atomic.Int32
	r.f.Respond = func(q pwfake.Req) (int, any, bool) {
		if q.Path == "/v1/hosts" && hits.Add(1) == 1 {
			return 401, pwfake.Err("unauthorized", "no"), true
		}
		return 0, nil, false
	}
	api, err := r.untilNotPending()
	if err != nil || api == nil {
		t.Fatalf("%v", err)
	}
	resp, err := api.ListHostsWithResponse(context.Background(), nil)
	if err != nil || resp.StatusCode() != 200 || hits.Load() != 2 {
		t.Fatalf("%v status %d hits %d", err, resp.StatusCode(), hits.Load())
	}
	if q, _ := r.f.Last("GET", "/v1/hosts"); q.Auth != "Bearer "+r.p.Key() {
		t.Fatalf("bearer %q", q.Auth)
	}
	b, _ := os.ReadFile(r.tok)
	if strings.TrimSpace(string(b)) != r.p.Key() {
		t.Fatal("key not stored")
	}
}

func TestGateDeniedThenFilesAgain(t *testing.T) {
	r := newGateRig(t)
	r.p.Script(keyclienttest.Denied())
	if _, err := r.connect(); err == nil || !strings.HasPrefix(err.Error(), "No powerwarden key") {
		t.Fatalf("%v", err)
	}
	_, err := r.untilNotPending()
	if err == nil || err.Error() != "key request denied" {
		t.Fatalf("%v", err)
	}
	if n := len(r.p.Filed()); n != 1 {
		t.Fatalf("filed %d", n)
	}
	if _, err := r.connect(); err == nil || !strings.HasPrefix(err.Error(), "No powerwarden key") {
		t.Fatalf("the call after a final error files a new request: %v", err)
	}
	if n := len(r.p.Filed()); n != 2 {
		t.Fatalf("filed %d, want a new request", n)
	}
}

func TestGateFinalErrorsUseSR9Words(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    keyclienttest.Outcome
		want string
	}{
		{"expired", keyclienttest.Expired(), "key request: expired\nrun this again to file a new request"},
		{"consumed", keyclienttest.Consumed(), "key request: the key was already handed out and not received; request again\nrun this again to file a new request"},
		{"unauthorized", keyclienttest.Unauthorized(), "key request: poll refused as unauthorized: unauthorized"},
	} {
		r := newGateRig(t)
		r.p.Script(tc.o)
		if _, err := r.connect(); err == nil {
			t.Fatal("want pending")
		}
		if _, err := r.untilNotPending(); err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestGateProviderErrorAndNoProvider(t *testing.T) {
	r := newGateRig(t)
	r.p.FileWith(keyclienttest.Status(429, `{"error":"rate_limited","message":"slow down"}`).WithHeader("Retry-After", "7"))
	_, err := r.connect()
	if err == nil || !strings.Contains(err.Error(), "status 429") || !strings.HasSuffix(err.Error(), "\ntry again in 7s") {
		t.Fatalf("%v", err)
	}
	// no provider: today's no-token error, nothing created
	f := pwfake.New()
	t.Cleanup(f.Srv.Close)
	tok := filepath.Join(t.TempDir(), "cfg", "token")
	g, err := NewGate(context.Background(), client.Config{URL: f.Srv.URL}, Params{ServiceURL: f.Srv.URL, TokenPath: tok, NameHint: "tester@box"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Connect(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "token file: ") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Dir(tok)); err == nil {
		t.Fatal("something was created")
	}
}
