package keyreq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/cli/internal/client"
)

func testNotice() request.Notice {
	return request.Notice{
		Service: "powerwarden.example.com", ApproveURL: "https://keys.example.com/requests?code=ABCD-EFGH",
		UserCode: "ABCD-EFGH", Role: "agent", Tool: "pwrwrdn/0.3.0",
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

func TestConstantsPinned(t *testing.T) {
	if DefaultRole != "agent" || ExitTempFail != 75 || client.EnvKeyRequestURL != "POWERWARDEN_KEY_REQUEST_URL" || client.Version != "0.3.0" {
		t.Fatal("constants drifted")
	}
}

func TestOptionsMapping(t *testing.T) {
	o := Options(Params{ServiceURL: "https://powerwarden.example.com", Override: "https://keys.example.com/r", TokenPath: "/t/token",
		Role: "", NameHint: "u@box", Wait: true})
	if o.ServiceURL != "https://powerwarden.example.com" || o.Override != "https://keys.example.com/r" || o.TokenPath != "/t/token" ||
		o.Role != "agent" || o.NameHint != "u@box" || o.Tool != "pwrwrdn/0.3.0" || o.Host != "" || o.AllowInsecureHTTP || !o.Wait || o.Notify != nil {
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
