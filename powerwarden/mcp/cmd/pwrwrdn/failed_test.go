package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

// PWC11a: when the claim fails after ready, `pwrwrdn run` prints the failure
// once on stderr, keeps the child running and keeps its exit code.
func TestRunPrintsClaimFailureOnceAndKeepsExitCode(t *testing.T) {
	r := newRig(t)
	r.bind(pw.On)
	bound := r.f.OnCreate
	r.f.OnCreate = func(c *pw.Claim) {
		bound(c)
		lease := 3 // renewed every second
		c.LeaseSeconds = &lease
	}
	var fail atomic.Bool
	r.f.Respond = func(q pwfake.Req) (int, any, bool) {
		if q.Method == "PUT" && fail.Load() {
			return 410, pwfake.Err(pw.ErrorCodeClaimFailed, "host box1.labtwo locked"), true
		}
		return 0, nil, false
	}
	done := make(chan int, 1)
	go func() {
		done <- dispatch(t.Context(), r.env("run", "--tags", "gpu", "--", "sh", "-c", "echo up; sleep 5; echo finished; exit 7"))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.out.String(), "up") {
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fail.Store(true)
	select {
	case rc := <-done:
		if rc != 7 {
			t.Fatalf("rc=%d stderr=%s", rc, r.errb)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not exit")
	}
	if !strings.Contains(r.out.String(), "finished") {
		t.Fatalf("the child was stopped: %q", r.out)
	}
	want := "claim " + r.firstID() + " failed: host box1.labtwo locked"
	if n := strings.Count(r.errb.String(), want); n != 1 {
		t.Fatalf("want %q once, got %d in %q", want, n, r.errb)
	}
}

// claim --wait and power --wait-on error with the server's detail.
func TestClaimWaitFailedNamesDetail(t *testing.T) {
	r := newRig(t)
	r.f.OnCreate = func(c *pw.Claim) { c.State, c.Detail, c.Version = pw.Failed, "boot failed on box1.labtwo", 2 }
	if rc := r.do("claim", "--tags", "x", "--wait"); rc == 0 || !strings.Contains(r.errb.String(), "failed: boot failed on box1.labtwo") {
		t.Fatalf("rc=%d stderr=%s", rc, r.errb)
	}
}
