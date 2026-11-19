package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/apptest"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

const (
	tokAgent = "agent-token-0123456789a"
	tokOp    = "op-token-0123456789abcd"
)

// setupRoles: hourchd (string, full), op (map, full), jared@box (map, agent), ann@box (map, agent).
func setupRoles(t *testing.T) *tenv {
	t.Helper()
	env := apptest.New(t, func(c *config.Config) {
		c.HostTags = map[string][]string{v1: {"kernel"}}
	}, pm.Host{Hostname: v1, State: pm.StateSoftOff})
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	y := "hourchd: " + tokHourchd + "\n" +
		"op: {token: " + tokOp + ", role: full}\n" +
		"jared@box: {token: " + tokAgent + ", role: agent}\n" +
		"ann@box:\n  token: " + tokAnn + "\n  role: agent\n"
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{App: env.App, Tokens: toks, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &tenv{Env: env, srv: srv, mux: mux}
}

func loadTokensYAML(t *testing.T, y string) (*Tokens, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadTokens(p)
}

func TestTokensBothFormsInOneFile(t *testing.T) {
	toks, err := loadTokensYAML(t, "hourchd: "+tokHourchd+"\nop: {token: "+tokOp+", role: full}\njared@box: {token: "+tokAgent+", role: agent}\n")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"hourchd": RoleFull, "op": RoleFull, "jared@box": RoleAgent} {
		if got := toks.Role(name); got != want {
			t.Errorf("Role(%s) = %q, want %q", name, got, want)
		}
	}
	if n, ok := toks.Name(tokAgent); !ok || n != "jared@box" {
		t.Errorf("Name(agent token) = %q %v", n, ok)
	}
	// Role comes only from the entry: a name that looks like a user token is full as a string.
	toks, err = loadTokensYAML(t, "jared@box: "+tokAgent+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := toks.Role("jared@box"); got != RoleFull {
		t.Errorf("string entry named like a user token: %q", got)
	}
	// Map without a role is full.
	toks, err = loadTokensYAML(t, "x: {token: "+tokOp+"}\n")
	if err != nil || toks.Role("x") != RoleFull {
		t.Errorf("map without role: %v", err)
	}
}

func TestTokensBadEntries(t *testing.T) {
	for name, y := range map[string]string{
		"unknown role":   "a: {token: " + tokOp + ", role: root}\n",
		"unknown key":    "a: {token: " + tokOp + ", rolle: agent}\n",
		"missing token":  "a: {role: agent}\n",
		"short token":    "a: {token: short, role: agent}\n",
		"bad name":       "'a b': {token: " + tokOp + "}\n",
		"sequence entry": "a: [" + tokOp + "]\n",
	} {
		if _, err := loadTokensYAML(t, y); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := loadTokensYAML(t, "jared@box.lab-1_x: {token: "+tokOp+", role: agent}\n"); err != nil {
		t.Errorf("name with @: %v", err)
	}
}

func TestAgentRoleEnforced(t *testing.T) {
	e := setupRoles(t)
	restricted := [][2]string{
		{"PUT", "/v1/demands/d1"}, {"DELETE", "/v1/demands/d1"},
		{"PUT", "/v1/workers/w1"}, {"DELETE", "/v1/workers/w1"},
		{"PUT", "/v1/hosts/" + v1 + "/tags"}, {"PUT", "/v1/hosts/" + v1 + "/exclude"},
	}
	for _, r := range restricted {
		code, b := e.do(t, r[0], r[1], tokAgent, `{"tags":[],"exclude":true}`)
		if code != 403 || errCode(t, b) != api.ErrorCodeForbidden {
			t.Errorf("agent %s %s: %d %s", r[0], r[1], code, b)
		}
		// A full token is not refused by role (it may fail on the body, never 403).
		if code, _ := e.do(t, r[0], r[1], tokOp, `{"tags":[],"exclude":true}`); code == 403 {
			t.Errorf("full %s %s: 403", r[0], r[1])
		}
	}
	// Agent may read everything and use claims.
	for _, p := range []string{"/v1/demands", "/v1/workers", "/v1/claims", "/v1/hosts", "/v1/decisions", "/v1/stats"} {
		if code, b := e.do(t, "GET", p, tokAgent, ""); code != 200 {
			t.Errorf("agent GET %s: %d %s", p, code, b)
		}
	}
	if code, b := e.do(t, "PUT", "/v1/claims/c1", tokAgent, `{"tags":["veritech01"],"purpose":"p"}`); code != 200 {
		t.Fatalf("agent PUT claim: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/v1/claims/c1/extend", tokAgent, `{"max_seconds":3600}`); code != 200 {
		t.Fatalf("agent extend: %d %s", code, b)
	}
	// The events stream is allowed for an agent (status 200 before the body streams).
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tokAgent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("agent GET events: %d", resp.StatusCode)
	}
}

// An agent token cannot release another owner's claim; a full token can.
func TestAgentDeleteOtherOwnersClaim(t *testing.T) {
	e := setupRoles(t)
	if code, b := e.do(t, "PUT", "/v1/claims/c1", tokAgent, `{"tags":["veritech01"],"purpose":"p"}`); code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	code, b := e.do(t, "DELETE", "/v1/claims/c1", tokAnn, "")
	if code != 403 || errCode(t, b) != api.ErrorCodeForbidden {
		t.Fatalf("agent delete of other's claim: %d %s", code, b)
	}
	if code, _ := e.do(t, "DELETE", "/v1/claims/c1", tokOp, ""); code != 204 {
		t.Fatalf("full delete: %d", code)
	}
}

// Every spec operation has a role decision in the route table, and a row is
// RoleNone exactly when the spec operation overrides security to none.
func TestEverySpecOperationHasRoleDecision(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			n++
			r, ok := RouteRole(method + " " + path)
			if !ok {
				t.Errorf("no role decision for %s %s", method, path)
				continue
			}
			noAuth := op.Security != nil && len(*op.Security) == 0
			if noAuth != (r == RoleNone) {
				t.Errorf("%s %s: spec no-auth=%v but role %q", method, path, noAuth, r)
			}
		}
	}
	if n < 13 {
		t.Fatalf("only %d operations found", n)
	}
	if r, ok := RouteRole("POST /v1/claims/{id}/power"); !ok || r != RoleAgent {
		t.Errorf("PWC8a power route: %q %v", r, ok)
	}
	if r, _ := RouteRole("PUT /v1/demands/{id}"); r != RoleFull {
		t.Errorf("demands PUT role %q", r)
	}
}
