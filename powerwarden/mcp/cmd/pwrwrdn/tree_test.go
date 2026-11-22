package main

// The command-line convention, checked against the real command table:
// every command's --help (stdout, exit 0, usage, description, flags and an
// example), --json on every non-interactive command (one object on stdout),
// the JSON error object on stderr, and the exit codes 0, 1, 2 and 75.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/keyclient/keyclienttest"
	pw "github.com/resurgentech/powerwarden/powerwarden/api"
)

const exampleHost = "host01.example.com"

// bindExample makes every new claim ready on exampleHost, which is on.
func bindExample(r *rig) {
	r.f.OnCreate = func(c *pw.Claim) {
		h, hs := exampleHost, pw.On
		c.State, c.Detail, c.Hostname, c.HostState, c.Version = pw.Ready, "", &h, &hs, 2
	}
}

// oneObject decodes s as exactly one JSON object and nothing after it.
func oneObject(t *testing.T, what, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("%s is not one JSON object: %v\n%q", what, err, s)
	}
	if m == nil {
		t.Fatalf("%s is null, not an object: %q", what, s)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("%s has more than one JSON value: %q", what, s)
	}
	return m
}

// errorObject checks the convention's error object and returns its code and
// message.
func errorObject(t *testing.T, what, s string) (code, msg string) {
	t.Helper()
	m := oneObject(t, what, s)
	e, ok := m["error"].(map[string]any)
	if !ok || len(m) != 1 {
		t.Fatalf("%s is not {\"error\":{...}}: %q", what, s)
	}
	code, _ = e["code"].(string)
	msg, _ = e["message"].(string)
	if code == "" || msg == "" {
		t.Fatalf("%s lacks error.code or error.message: %q", what, s)
	}
	return code, msg
}

func TestTreeIsWalked(t *testing.T) {
	got := map[string]bool{}
	for _, p := range commandPaths() {
		got[strings.Join(p, " ")] = true
	}
	for _, want := range []string{"mcp", "setup-token", "key request", "key status", "run", "claim",
		"release", "extend", "power", "hosts", "claims"} {
		if !got[want] {
			t.Errorf("command tree lacks %q (have %v)", want, got)
		}
	}
}

func TestTopHelp(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "-help"} {
		r := newRig(t)
		if rc := r.do(flag); rc != 0 {
			t.Fatalf("%s: rc=%d stderr=%q", flag, rc, r.errb)
		}
		if r.errb.String() != "" {
			t.Fatalf("%s: help went to stderr: %q", flag, r.errb)
		}
		out := r.out.String()
		for _, want := range []string{"Usage:", "Commands:", "Flags", "Examples:", "Agent tips:",
			"--json", "claim --wait", "pwrwrdn run", "pwrwrdn key request", "75", "interactive"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: top-level help lacks %q:\n%s", flag, want, out)
			}
		}
		for _, p := range commandPaths() {
			if !strings.Contains(out, p[0]) {
				t.Errorf("%s: top-level help does not list %q", flag, p[0])
			}
		}
	}
}

func TestBareIsUsageError(t *testing.T) {
	r := newRig(t)
	if rc := r.do(); rc != 2 || r.out.String() != "" || !strings.Contains(r.errb.String(), "Usage:") {
		t.Fatalf("rc=%d out=%q err=%q", rc, r.out, r.errb)
	}
}

// Every command, and the key group: --help and -h print the parts the
// convention asks for on stdout and exit 0; --json --help is accepted (so the
// flag is defined) except on interactive commands.
func TestTreeHelp(t *testing.T) {
	paths := append(commandPaths(), []string{"key"})
	for _, p := range paths {
		name := strings.Join(p, " ")
		for _, flag := range []string{"--help", "-h"} {
			r := newRig(t)
			if rc := r.do(append(append([]string{}, p...), flag)...); rc != 0 {
				t.Errorf("%s %s: rc=%d stderr=%q", name, flag, rc, r.errb)
				continue
			}
			if r.errb.String() != "" {
				t.Errorf("%s %s: stderr not empty: %q", name, flag, r.errb)
			}
			out := r.out.String()
			for _, want := range []string{"Usage:\n  pwrwrdn " + name, "Description:\n  ", "Flags:\n", "Examples:\n  pwrwrdn " + name} {
				if !strings.Contains(out, want) {
					t.Errorf("%s %s: help lacks %q:\n%s", name, flag, want, out)
				}
			}
			if name != "key" && !isInteractive(p) {
				if !strings.Contains(out, "-json") {
					t.Errorf("%s: help does not list --json:\n%s", name, out)
				}
			}
		}
		if name == "key" {
			continue
		}
		r := newRig(t)
		rc := r.do(append(append([]string{}, p...), "--json", "--help")...)
		if isInteractive(p) {
			if rc != 2 || !strings.Contains(r.errb.String(), "interactive command; no --json") {
				t.Errorf("%s --json: rc=%d stderr=%q, want 2 and the interactive message", name, rc, r.errb)
			}
			continue
		}
		if rc != 0 {
			t.Errorf("%s --json --help: rc=%d stderr=%q", name, rc, r.errb)
		}
	}
}

func TestRunHelpSaysExitCode(t *testing.T) {
	r := newRig(t)
	if rc := r.do("run", "--help"); rc != 0 || !strings.Contains(r.out.String(), "exits with the command's exit code") {
		t.Fatalf("rc=%d out=%s", rc, r.out)
	}
}

func TestInteractiveJSON(t *testing.T) {
	r := newRig(t)
	if rc := r.do("mcp", "--json"); rc != 2 || !strings.Contains(r.errb.String(), "interactive command; no --json") {
		t.Fatalf("rc=%d stderr=%q", rc, r.errb)
	}
}

// jsonCase runs one non-interactive command with --json against the fake and
// checks its stdout object.
type jsonCase struct {
	args  []string
	setup func(r *rig)
	check func(t *testing.T, m map[string]any)
}

func hasKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("object lacks %q: %v", k, m)
		}
	}
}

func isList(t *testing.T, m map[string]any, key string, n int) {
	t.Helper()
	l, ok := m[key].([]any)
	if !ok || len(l) != n {
		t.Errorf("%q is not a list of %d: %v", key, n, m)
	}
}

func TestTreeJSON(t *testing.T) {
	ready := func(r *rig) { bindExample(r) }
	cases := map[string]jsonCase{
		"claim": {args: []string{"claim", "--tags", "gpu", "--json"}, check: func(t *testing.T, m map[string]any) {
			hasKeys(t, m, "id", "state", "mode", "hostname", "expires_at", "detached")
		}},
		"release": {args: []string{"release", "--json", "c1"}, setup: func(r *rig) { r.f.SetClaim(&pw.Claim{Id: "c1"}) },
			check: func(t *testing.T, m map[string]any) {
				if m["id"] != "c1" || m["released"] != true {
					t.Errorf("release object %v", m)
				}
			}},
		"extend": {args: []string{"extend", "c1", "2h", "--json"}, setup: func(r *rig) { r.f.SetClaim(&pw.Claim{Id: "c1", State: pw.Ready}) },
			check: func(t *testing.T, m map[string]any) { hasKeys(t, m, "id", "state", "expires_at") }},
		"power": {args: []string{"power", "c1", "powerup", "--json"}, setup: func(r *rig) {
			h, hs := "box1", pw.SoftOff
			r.f.SetClaim(&pw.Claim{Id: "c1", State: pw.Ready, Hostname: &h, HostState: &hs, Mode: pw.Dedicated})
		}, check: func(t *testing.T, m map[string]any) { hasKeys(t, m, "claim_id", "action", "already", "command") }},
		"hosts": {args: []string{"hosts", "--json"}, setup: func(r *rig) {
			r.f.Hosts = []pw.HostView{{Hostname: "box1", State: pw.On, Tags: []string{}, NameTags: []string{"box1"}, Claims: []pw.HostClaim{}}}
		}, check: func(t *testing.T, m map[string]any) { isList(t, m, "hosts", 1) }},
		"claims": {args: []string{"claims", "--json"}, setup: func(r *rig) { r.f.SetClaim(&pw.Claim{Id: "c1"}) },
			check: func(t *testing.T, m map[string]any) { isList(t, m, "claims", 1) }},
		"key request": {args: []string{"key", "request", "--json"}, check: func(t *testing.T, m map[string]any) {
			if m["result"] != "already_present" {
				t.Errorf("key request object %v", m)
			}
			hasKeys(t, m, "token_file")
		}},
		"key status": {args: []string{"key", "status", "--json"}, check: func(t *testing.T, m map[string]any) {
			if m["token"] != "present" {
				t.Errorf("key status object %v", m)
			}
			p, ok := m["pending"].(map[string]any)
			if !ok || p["state"] != "none" {
				t.Errorf("key status pending %v", m)
			}
			hasKeys(t, m, "token_file", "pending_file")
		}},
	}
	for _, p := range commandPaths() {
		name := strings.Join(p, " ")
		if isInteractive(p) || name == "run" || name == "setup-token" {
			continue // run: TestRunJSON; setup-token needs OpenBao: TestSetupTokenJSON
		}
		c, ok := cases[name]
		if !ok {
			t.Errorf("no --json case for %q: add one", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			ready(r)
			if c.setup != nil {
				c.setup(r)
			}
			if rc := r.do(c.args...); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, r.errb)
			}
			c.check(t, oneObject(t, "stdout", r.out.String()))
		})
	}
}

func TestPowerWaitOnJSON(t *testing.T) {
	r := newRig(t)
	h, hs := "box1", pw.On
	r.f.SetClaim(&pw.Claim{Id: "c1", State: pw.Ready, Hostname: &h, HostState: &hs, Mode: pw.Dedicated, Version: 1})
	if rc := r.do("power", "c1", "powerup", "--wait-on", "5s", "--json"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	m := oneObject(t, "stdout", r.out.String())
	if m["host_state"] != "on" {
		t.Fatalf("power --wait-on object %v", m)
	}
}

// setup-token: deprecated, but --json still holds. With a fake bao on PATH it
// writes the token and prints one object.
func TestSetupTokenJSON(t *testing.T) {
	r := newRig(t)
	if err := os.Remove(r.token); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "bao"), []byte("#!/bin/sh\necho tok-from-bao\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.getenv["PWRWRDN_BAO_PATH"] = "users/tokens"
	if rc := r.do("setup-token", "--json"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	m := oneObject(t, "stdout", r.out.String())
	if m["deprecated"] != true || m["token_file"] != r.token {
		t.Fatalf("setup-token object %v", m)
	}
	if strings.Contains(r.out.String(), "tok-from-bao") || strings.Contains(r.errb.String(), "tok-from-bao") {
		t.Fatal("token echoed")
	}
	if r.errb.String() != "" {
		t.Fatalf("stderr not empty with --json: %q", r.errb)
	}
}

func TestSetupTokenJSONError(t *testing.T) {
	r := newRig(t)
	if rc := r.do("setup-token", "--json"); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	if code, _ := errorObject(t, "stderr", r.errb.String()); code == "" {
		t.Fatal("no code")
	}
}

// run is a wrapper: one object on stderr with the claim id before the child
// starts, the child's own output, and a closing object with exit_code; the
// exit code stays the child's.
func TestRunJSON(t *testing.T) {
	r := newRig(t)
	bindExample(r)
	if rc := r.do("run", "--json", "--tags", "gpu", "--", "sh", "-c", "echo out; echo err >&2; exit 3"); rc != 3 {
		t.Fatalf("rc=%d stderr=%q", rc, r.errb)
	}
	if r.out.String() != "out\n" {
		t.Fatalf("stdout %q", r.out)
	}
	lines := strings.Split(strings.TrimRight(r.errb.String(), "\n"), "\n")
	if len(lines) != 3 || lines[1] != "err" {
		t.Fatalf("stderr %q", lines)
	}
	first := oneObject(t, "first stderr line", lines[0])
	if first["claim_id"] != r.firstID() || first["host"] != exampleHost {
		t.Fatalf("first object %v", first)
	}
	last := oneObject(t, "last stderr line", lines[2])
	if last["claim_id"] != r.firstID() || last["exit_code"] != float64(3) {
		t.Fatalf("last object %v", last)
	}
}

// The JSON error object: stderr, one object, non-zero exit, stdout empty.
func TestJSONErrorObject(t *testing.T) {
	r := newRig(t)
	delete(r.getenv, "POWERWARDEN_URL")
	if rc := r.do("hosts", "--json"); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	if r.out.String() != "" {
		t.Fatalf("stdout %q", r.out)
	}
	code, msg := errorObject(t, "stderr", r.errb.String())
	if code != "config" || !strings.Contains(msg, "POWERWARDEN_URL") {
		t.Fatalf("code=%q msg=%q", code, msg)
	}
}

func TestJSONRefusal(t *testing.T) {
	r := newRig(t)
	if rc := r.do("extend", "--json", "nope", "1h"); rc != 1 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	code, _ := errorObject(t, "stderr", r.errb.String())
	if code != "not_found" {
		t.Fatalf("code=%q", code)
	}
}

// Without a terminal a missing key is filed and the command exits 75; with
// --json that is one object, code key_pending, whose message carries the
// approval notice.
func TestJSONKeyPending(t *testing.T) {
	k := newKeyRig(t, false)
	k.p.Script(keyclienttest.Approved())
	if rc := k.do("hosts", "--json"); rc != 75 {
		t.Fatalf("rc=%d %s", rc, k.errb)
	}
	code, msg := errorObject(t, "stderr", k.errb.String())
	if code != "key_pending" || !strings.Contains(msg, approvePrefix) || !strings.Contains(msg, pendingLine) {
		t.Fatalf("code=%q msg=%q", code, msg)
	}
	if k.out.String() != "" {
		t.Fatalf("stdout %q", k.out)
	}
	k.noSecrets()
}

// Usage errors exit 2; with --json the code is "usage".
func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{"release"},
		{"release", "a", "b"},
		{"extend", "c1"},
		{"extend", "c1", "soon"},
		{"power"},
		{"power", "a", "b", "c"},
		{"power", "c1", "powerup", "--wait-on", "never"},
		{"claim"},
		{"claim", "--tags", "a", "--mode", "bogus"},
		{"claim", "--tags", "a", "--max", "never"},
		{"run", "--tags", "a"},
		{"hosts", "--bogus"},
		{"key", "status", "--bogus"},
		{"frobnicate"},
	} {
		r := newRig(t)
		if rc := r.do(args...); rc != 2 {
			t.Errorf("%v: rc=%d stderr=%q", args, rc, r.errb)
		}
		if r.out.String() != "" {
			t.Errorf("%v: stdout %q", args, r.out)
		}
		r = newRig(t)
		if rc := r.do(append(args, "--json")...); rc != 2 {
			t.Errorf("%v --json: rc=%d stderr=%q", args, rc, r.errb)
			continue
		}
		if code, _ := errorObject(t, "stderr", r.errb.String()); code != "usage" {
			t.Errorf("%v --json: code %q", args, code)
		}
	}
}

// --json after `--` belongs to the child, not to run.
func TestRunJSONAfterDashDash(t *testing.T) {
	r := newRig(t)
	bindExample(r)
	if rc := r.do("run", "--tags", "gpu", "--", "sh", "-c", "exit 0", "--json"); rc != 0 {
		t.Fatalf("rc=%d %s", rc, r.errb)
	}
	if bytes.Contains([]byte(r.errb.String()), []byte("{")) {
		t.Fatalf("stderr has JSON: %q", r.errb)
	}
}
