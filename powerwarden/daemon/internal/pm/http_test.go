package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, h http.HandlerFunc) *HTTP {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewHTTP(srv.URL, "tok", 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHTTPListHostsAndAuth(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"unauthorized","message":"no"}`)
			return
		}
		if r.URL.Path != "/v1/hosts" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, `[{"hostname":"veritech01.labtwo","tags":["kernel"],"locked":false,"class":"veritech",
		  "state":"powering_on","status":{"pdu":true,"ipmi":true,"ping":false,"ssh":false,"observed_at":null},
		  "last_error":"","power_failed_at":"2026-10-27T11:00:00Z",
		  "active_command":{"id":17,"action":"powerup","state":"running","deadline":"2026-10-27T12:15:00Z"}}]`)
	})
	hosts, err := c.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := hosts[0]
	if h.Hostname != "veritech01.labtwo" || h.State != StatePoweringOn || h.PowerFailedAt == nil ||
		!h.ActiveCommand.InFlightBoot() || h.ActiveCommand.ID != 17 {
		t.Fatalf("host %+v", h)
	}
}

func TestHTTPListHostsStateSince(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"hostname":"a","tags":[],"locked":false,"class":"veritech","state":"soft_off","state_since":"2026-10-27T11:00:00Z",
		  "status":{"pdu":true,"ipmi":true,"ping":false,"ssh":false,"observed_at":null},"last_error":"","power_failed_at":null,"active_command":null},
		  {"hostname":"b","tags":[],"locked":false,"class":"veritech","state":"soft_off",
		  "status":{"pdu":true,"ipmi":true,"ping":false,"ssh":false,"observed_at":null},"last_error":"","power_failed_at":null,"active_command":null}]`)
	})
	hosts, err := c.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC); !hosts[0].StateSince.Equal(want) {
		t.Fatalf("state_since %v", hosts[0].StateSince)
	}
	if !hosts[1].StateSince.IsZero() {
		t.Fatalf("missing state_since must be zero, got %v", hosts[1].StateSince)
	}
}

func TestHTTPPowerAndErrors(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/power") {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if strings.Contains(r.URL.Path, "locked") {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"error":"host_locked","message":"locked"}`)
			return
		}
		w.WriteHeader(202)
		fmt.Fprint(w, `{"id":5,"hostname":"a","action":"powerup","state":"queued","force":false,"escalated":false,
		  "requested_by":"powerwarden","reason":"d1","attempt":"x","created_at":"2026-10-27T12:00:00Z",
		  "started_at":null,"deadline":"2026-10-27T12:15:00Z","finished_at":null,"error":""}`)
	})
	cmd, err := c.Power(context.Background(), "a", ActionPowerup, "d1")
	if err != nil || cmd.ID != 5 || !cmd.Active() || cmd.Reason != "d1" {
		t.Fatalf("%+v %v", cmd, err)
	}
	_, err = c.Power(context.Background(), "locked", ActionPowerup, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Code != "host_locked" {
		t.Fatalf("err %v", err)
	}
}

func TestHTTPEvents(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "41" {
			t.Errorf("Last-Event-ID %q", r.Header.Get("Last-Event-ID"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": heartbeat\n\n")
		fmt.Fprint(w, "id: 42\nevent: command\ndata: {\"seq\":42,\"type\":\"command\",\"command\":{\"id\":5,\"hostname\":\"a\",\"action\":\"powerup\",\"state\":\"failed\",\"force\":false,\"escalated\":false,\"requested_by\":\"powerwarden\",\"reason\":\"\",\"attempt\":\"x\",\"created_at\":\"2026-10-27T12:00:00Z\",\"started_at\":null,\"deadline\":\"2026-10-27T12:15:00Z\",\"finished_at\":\"2026-10-27T12:05:00Z\",\"error\":\"ping_ssh_timeout\"}}\n\n")
		fmt.Fprint(w, "id: 43\nevent: host\ndata: {\"seq\":43,\"type\":\"host\",\"hostname\":\"a\",\"state\":\"soft_off\",\"status\":{\"pdu\":null,\"ipmi\":null,\"ping\":null,\"ssh\":null,\"observed_at\":null},\"active_command\":null}\n\n")
		fmt.Fprint(w, "id: 44\nevent: resync\ndata: {\"seq\":44,\"type\":\"resync\"}\n\n")
	})
	ch, err := c.Events(context.Background(), 41)
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("events %+v", got)
	}
	if got[0].Type != EventCommand || got[0].Command.State != CmdFailed || got[0].Command.FinishedAt == nil || got[0].Seq != 42 {
		t.Fatalf("command event %+v", got[0])
	}
	if got[1].Type != EventHost || got[1].Host.State != StateSoftOff || got[1].Host.ActiveCommandID != nil {
		t.Fatalf("host event %+v", got[1].Host)
	}
	if got[2].Type != EventResync {
		t.Fatal("resync not parsed")
	}
}

// Powercut and reboot go out as their own action, and the command's extra
// fields survive for the claim power reply.
func TestHTTPPowerRebootAndPowercut(t *testing.T) {
	for _, action := range []string{ActionReboot, ActionPowercut} {
		var got string
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"id":9,"hostname":"a","action":%q,"state":"queued","force":false,"escalated":true,
			  "requested_by":"powerwarden","reason":"claim c1 by jared","attempt":"tok","created_at":"2026-10-27T12:00:00Z",
			  "started_at":"2026-10-27T12:00:01Z","deadline":"2026-10-27T12:15:00Z","finished_at":null,"error":""}`, action)
		})
		cmd, err := c.Power(context.Background(), "a", action, "claim c1 by jared")
		if err != nil || cmd.Action != action || !cmd.Escalated || cmd.Attempt != "tok" || cmd.StartedAt == nil {
			t.Fatalf("%s: %+v %v", action, cmd, err)
		}
		if !strings.Contains(got, `"action":"`+action+`"`) || !strings.Contains(got, "claim c1 by jared") {
			t.Fatalf("%s body %s", action, got)
		}
	}
}
