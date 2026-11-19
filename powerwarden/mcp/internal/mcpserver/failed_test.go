package mcpserver_test

import (
	"strings"
	"testing"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

// PWC11a: wait_for_claim returning failed for this session's claim adds the
// lost-claim warning with the detail.
func TestWaitForClaimFailedAddsWarning(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)
	e.f.Claims[id].Version, e.f.Claims[id].State, e.f.Claims[id].Detail = 2, pw.Failed, "boot failed on box1.labtwo"
	res, texts := e.call(t, "wait_for_claim", map[string]any{"claim_id": id})
	if res.IsError || parse(t, texts[0])["state"] != "failed" {
		t.Fatalf("%v %v", res.IsError, texts)
	}
	last := texts[len(texts)-1]
	if !strings.Contains(last, "WARNING") || !strings.Contains(last, id) || !strings.Contains(last, "boot failed on box1.labtwo") {
		t.Fatalf("no warning with the detail: %v", texts)
	}
	if l := e.k.Lost(); len(l) != 1 || !l[0].Failed {
		t.Fatalf("lost: %+v", l)
	}
	// A failed claim this process does not own adds no warning.
	e.f.Claims["other"] = &pw.Claim{Id: "other", State: pw.Failed, Detail: "no_host", Version: 3}
	e.k.Ack(id)
	_, texts = e.call(t, "wait_for_claim", map[string]any{"claim_id": "other"})
	if len(texts) != 1 {
		t.Fatalf("warning for a claim not ours: %v", texts)
	}
}

// A claim that fails after ready: the renewal's 410 claim_failed puts the
// detail in the warning every later result carries.
func TestFailureAfterReadyWarnsWithDetail(t *testing.T) {
	e := newEnv(t)
	_, texts := e.call(t, "claim_host", map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1})
	id := parse(t, texts[0])["id"].(string)
	deadline := time.Now().Add(3 * time.Second)
	for e.clk.Waiters() != 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	e.f.Respond = func(r pwfake.Req) (int, any, bool) {
		if r.Method == "PUT" {
			return 410, pwfake.Err(pw.ErrorCodeClaimFailed, "host box1.labtwo excluded"), true
		}
		return 0, nil, false
	}
	e.clk.Advance(time.Hour)
	for len(e.k.Lost()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	_, texts = e.call(t, "list_hosts", nil)
	last := texts[len(texts)-1]
	if !strings.Contains(last, "WARNING") || !strings.Contains(last, id) || !strings.Contains(last, "host box1.labtwo excluded") {
		t.Fatalf("no warning with the detail: %v", texts)
	}
}
