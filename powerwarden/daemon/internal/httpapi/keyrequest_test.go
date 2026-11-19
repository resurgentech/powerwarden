package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"

	api "github.com/resurgentech/powerwarden/powerwarden/api"
)

const keyReqURL = "https://keys.example.com/v1/services/powerwarden/requests"

func getKeyRequest(t *testing.T, e *tenv, authorization string) (*http.Response, []byte) {
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
	e := setup(t, nil)
	resp, b := getKeyRequest(t, e, "")
	if resp.StatusCode != 404 || errCode(t, b) != api.ErrorCodeNotFound {
		t.Fatalf("unset: %d %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
}

func TestKeyRequestSetIs200NoAuth(t *testing.T) {
	e := setupKeyRequest(t, keyReqURL, nil)
	for name, auth := range map[string]string{
		"none": "", "valid": "Bearer " + tokHourchd, "invalid": "Bearer wrong-token-0123456789", "junk": "Basic xyz",
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

func TestRoleNoneNeverAGrant(t *testing.T) {
	if _, err := loadTokensYAML(t, "x: {token: "+tokOp+", role: none}\n"); err == nil {
		t.Fatal("role none accepted in tokens file")
	}
	if roleAllows(RoleNone, RoleAgent) || roleAllows(RoleNone, RoleFull) || roleAllows(RoleNone, RoleNone) {
		t.Fatal("roleAllows grants RoleNone")
	}
	for _, p := range []string{"GET /v1/healthz", "GET /v1/key-request"} {
		if r, ok := RouteRole(p); !ok || r != RoleNone {
			t.Errorf("%s: %q %v", p, r, ok)
		}
	}
}
