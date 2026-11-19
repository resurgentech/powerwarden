package httpapi

import (
	"context"
	"testing"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// PWC9: a claim the web UI made (owner ui:<user>, detached) belongs to another
// owner for an agent-role token. renew, extend, power and release are 403;
// a full-role token may still DELETE it.
func TestAgentTokenForbiddenOnUIOwnedClaim(t *testing.T) {
	e := setupRoles(t)
	ctx := context.Background()
	ui := app.Caller{Name: "ui:jared", Operator: true}
	c, err := e.App.PutClaim(ctx, ui, store.ClaimPut{ID: "ui-abc123", Tags: []string{"veritech01"}, Purpose: "bench", Detached: true})
	if err != nil || c.Owner != "ui:jared" || !c.Detached {
		t.Fatalf("seed claim: %+v %v", c, err)
	}
	for name, call := range map[string]func() (int, []byte){
		"renew": func() (int, []byte) {
			return e.do(t, "PUT", "/v1/claims/ui-abc123", tokAgent, `{"tags":["veritech01"],"purpose":"bench","detached":true}`)
		},
		"extend": func() (int, []byte) {
			return e.do(t, "POST", "/v1/claims/ui-abc123/extend", tokAgent, `{"max_seconds":7200}`)
		},
		"power":   func() (int, []byte) { return e.power(t, "ui-abc123", tokAgent, "reboot") },
		"release": func() (int, []byte) { return e.do(t, "DELETE", "/v1/claims/ui-abc123", tokAgent, "") },
	} {
		code, b := call()
		e.wantErr(t, name, code, b, 403, api.ErrorCodeForbidden)
	}
	if _, err := e.App.Claim(ctx, "ui-abc123", false); err != nil {
		t.Fatalf("claim lost after refused calls: %v", err)
	}
	if code, b := e.do(t, "DELETE", "/v1/claims/ui-abc123", tokOp, ""); code != 204 {
		t.Fatalf("full-role delete: %d %s", code, b)
	}
	if _, err := e.App.Claim(ctx, "ui-abc123", false); err == nil {
		t.Fatal("claim still there after full-role delete")
	}
}
