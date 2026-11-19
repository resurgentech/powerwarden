package httpapi

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	api "github.com/resurgentech/powerwarden/powermaster/api"
)

const keyReqURL = "https://keys.example.com/v1/services/powermaster/requests"

func getKeyRequest(t *testing.T, e *env, authorization string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/key-request", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestKeyRequestUnsetIs404(t *testing.T) {
	e := newEnv(t, 100)
	resp, b := getKeyRequest(t, e, "")
	if resp.StatusCode != 404 || decode[api.Error](t, b).Error != api.ErrorCodeNotFound {
		t.Fatalf("unset: %d %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
}

func TestKeyRequestSetIs200NoAuth(t *testing.T) {
	e := newEnvKeyRequest(t, 100, keyReqURL)
	for name, auth := range map[string]string{
		"none": "", "valid": "Bearer " + pwToken, "invalid": "Bearer wrong-token-0123456789", "junk": "Basic xyz",
	} {
		resp, b := getKeyRequest(t, e, auth)
		if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"request_url":"`+keyReqURL+`"}` {
			t.Errorf("%s: %d %q", name, resp.StatusCode, b)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: content type %q", name, ct)
		}
	}
}

// The routes with auth=false are exactly the spec operations that override
// security to none: healthz and key-request.
func TestNoAuthRoutesMatchSpec(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				want = append(want, method+" "+path)
			}
		}
	}
	e := newEnv(t, 100)
	s, _ := New(Options{Manager: e.mgr, Tokens: mustTokens(t)})
	var got []string
	for _, r := range s.routes() {
		if !r.auth {
			got = append(got, r.method+" "+r.path)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	exp := []string{"GET /v1/healthz", "GET /v1/key-request"}
	if strings.Join(got, "|") != strings.Join(exp, "|") || strings.Join(want, "|") != strings.Join(exp, "|") {
		t.Fatalf("no-auth routes: table %v, spec %v, want %v", got, want, exp)
	}
}
