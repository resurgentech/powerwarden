package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 7, 19, 0, 0, 0, time.UTC)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustHost(t *testing.T, s *Store, name string) Host {
	t.Helper()
	h, err := s.GetHost(name)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func setState(t *testing.T, s *Store, name, state string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE hosts SET state=? WHERE hostname=?`, state, name); err != nil {
		t.Fatal(err)
	}
}

func enqueue(t *testing.T, s *Store, host, action string) EnqueueResult {
	t.Helper()
	r, err := s.Enqueue(EnqueueParams{Hostname: host, Action: action, RequestedBy: "jared", Reason: "test", Now: t0, Deadline: t0.Add(990 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOpenPragmasAndMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (%v)", mode, err)
	}
	var bt int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&bt); err != nil || bt != 5000 {
		t.Fatalf("busy_timeout = %d (%v)", bt, err)
	}
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Fatalf("max open conns = %d", s.db.Stats().MaxOpenConnections)
	}
	v, err := s.SchemaVersion()
	if err != nil || v != len(migrations) || v != 2 {
		t.Fatalf("schema version = %d (%v)", v, err)
	}
	s.Close()
	// Reopen is idempotent.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if v2, _ := s2.SchemaVersion(); v2 != v {
		t.Fatalf("version after reopen = %d", v2)
	}
}

func TestEnsureHostsCreatesUnknown(t *testing.T) {
	s := openTest(t)
	created, err := s.EnsureHost("h1", t0)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if h := mustHost(t, s, "h1"); h.State != StateUnknown {
		t.Fatalf("state = %q", h.State)
	}
	setState(t, s, "h1", StateOn)
	if created, _ := s.EnsureHost("h1", t0); created {
		t.Fatal("re-ensure created again")
	}
	if h := mustHost(t, s, "h1"); h.State != StateOn {
		t.Fatalf("re-ensure clobbered state: %q", h.State)
	}
	if _, err := s.GetHost("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnqueueSetsCommandStateAndAttemptTogether(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	setState(t, s, "h1", StateSoftOff)
	r := enqueue(t, s, "h1", "powerup")
	if r.Outcome != OutcomeQueued || r.PriorState != StateSoftOff {
		t.Fatalf("outcome = %v prior=%q", r.Outcome, r.PriorState)
	}
	c := r.Command
	if c.State != CmdQueued || c.Attempt == "" || c.RequestedBy != "jared" || c.Reason != "test" || !c.Deadline.Equal(t0.Add(990*time.Second)) {
		t.Fatalf("command = %+v", c)
	}
	h := mustHost(t, s, "h1")
	if h.State != StatePoweringOn || h.Attempt != c.Attempt {
		t.Fatalf("host = %+v, command attempt %q", h, c.Attempt)
	}
	// Each enqueue gets a fresh attempt.
	got, _ := s.GetCommand(c.ID)
	if got.Attempt != c.Attempt {
		t.Fatalf("stored attempt %q != %q", got.Attempt, c.Attempt)
	}
}

func TestEnqueueIsOneTransaction(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	setState(t, s, "h1", StateSoftOff)
	before := mustHost(t, s, "h1")
	boom := errors.New("injected")
	s.testHookAfterInsert = func() error { return boom }
	_, err := s.Enqueue(EnqueueParams{Hostname: "h1", Action: "powerup", RequestedBy: "x", Now: t0, Deadline: t0.Add(time.Minute)})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	s.testHookAfterInsert = nil
	cmds, _ := s.ListCommands(CommandFilter{})
	if len(cmds) != 0 {
		t.Fatalf("command row survived a failed transaction: %+v", cmds)
	}
	if after := mustHost(t, s, "h1"); after != before {
		t.Fatalf("host changed: %+v -> %+v", before, after)
	}
}

func TestEnqueueIdempotencyAndConflicts(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	setState(t, s, "h1", StateSoftOff)
	first := enqueue(t, s, "h1", "powerup")
	again := enqueue(t, s, "h1", "powerup")
	if again.Outcome != OutcomeExisting || again.Command.ID != first.Command.ID {
		t.Fatalf("same action: %+v", again)
	}
	other := enqueue(t, s, "h1", "reboot")
	if other.Outcome != OutcomeConflict || other.Command.ID != first.Command.ID {
		t.Fatalf("other action: %+v", other)
	}
	if h := mustHost(t, s, "h1"); h.Attempt != first.Command.Attempt {
		t.Fatal("attempt changed by an idempotent request")
	}
	// Running counts as in flight too.
	if _, err := s.ClaimNext("h1", t0); err != nil {
		t.Fatal(err)
	}
	if r := enqueue(t, s, "h1", "powerdown"); r.Outcome != OutcomeConflict {
		t.Fatalf("running conflict: %+v", r)
	}
}

func TestEnqueueAlreadyThere(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	setState(t, s, "h1", StateOn)
	before := mustHost(t, s, "h1")
	r := enqueue(t, s, "h1", "powerup")
	if r.Outcome != OutcomeAlreadyThere || r.Command.State != CmdSucceeded || r.Command.FinishedAt == nil {
		t.Fatalf("powerup of on host: %+v", r)
	}
	if after := mustHost(t, s, "h1"); after.State != StateOn || after.Attempt != before.Attempt {
		t.Fatalf("host touched: %+v", after)
	}
	setState(t, s, "h1", StateSoftOff)
	if r := enqueue(t, s, "h1", "powerdown"); r.Outcome != OutcomeAlreadyThere {
		t.Fatalf("powerdown of off host: %+v", r)
	}
	// Reboot has no already-there state.
	if r := enqueue(t, s, "h1", "reboot"); r.Outcome != OutcomeQueued || mustHost(t, s, "h1").State != StateRebooting {
		t.Fatalf("reboot of off host: %+v", r)
	}
}

func TestClaimNextAndEscalate(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	if c, err := s.ClaimNext("h1", t0); err != nil || c != nil {
		t.Fatalf("empty claim: %+v %v", c, err)
	}
	q := enqueue(t, s, "h1", "reboot")
	c, err := s.ClaimNext("h1", t0.Add(time.Second))
	if err != nil || c == nil || c.ID != q.Command.ID || c.State != CmdRunning || c.StartedAt == nil {
		t.Fatalf("claim = %+v %v", c, err)
	}
	if c2, _ := s.ClaimNext("h1", t0); c2 != nil {
		t.Fatal("claimed twice")
	}
	e, err := s.MarkEscalated(c.ID)
	if err != nil || !e.Escalated {
		t.Fatalf("escalate = %+v %v", e, err)
	}
}

func TestFinishGuardedByAttempt(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	setState(t, s, "h1", StateSoftOff)
	q := enqueue(t, s, "h1", "powerup")
	c, _ := s.ClaimNext("h1", t0)
	failedAt := t0.Add(time.Minute)

	// A stale attempt is dropped: neither host nor command changes.
	applied, _, err := s.Finish(FinishParams{Hostname: "h1", Attempt: "stale", CommandID: c.ID,
		HostState: StateOn, CommandState: CmdSucceeded, Now: failedAt})
	if err != nil || applied {
		t.Fatalf("stale finish applied=%v err=%v", applied, err)
	}
	if h := mustHost(t, s, "h1"); h.State != StatePoweringOn {
		t.Fatalf("stale write landed: %+v", h)
	}
	if got, _ := s.GetCommand(c.ID); got.State != CmdRunning {
		t.Fatalf("stale write touched command: %+v", got)
	}

	applied, done, err := s.Finish(FinishParams{Hostname: "h1", Attempt: q.Command.Attempt, CommandID: c.ID,
		HostState: StateSoftOff, LastError: "ssh_down", PowerFailedAt: &failedAt,
		CommandState: CmdFailed, Error: "ssh_down", Now: failedAt})
	if err != nil || !applied {
		t.Fatalf("finish applied=%v err=%v", applied, err)
	}
	if done.State != CmdFailed || done.Error != "ssh_down" || done.FinishedAt == nil {
		t.Fatalf("command = %+v", done)
	}
	h := mustHost(t, s, "h1")
	if h.State != StateSoftOff || h.LastError != "ssh_down" || h.PowerFailedAt == nil || !h.PowerFailedAt.Equal(failedAt) {
		t.Fatalf("host = %+v", h)
	}
}

func TestFinishIsOneTransaction(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	q := enqueue(t, s, "h1", "powerup")
	c, _ := s.ClaimNext("h1", t0)
	boom := errors.New("injected")
	s.testHookAfterHostUpdate = func() error { return boom }
	_, _, err := s.Finish(FinishParams{Hostname: "h1", Attempt: q.Command.Attempt, CommandID: c.ID,
		HostState: StateOn, CommandState: CmdSucceeded, Now: t0})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if h := mustHost(t, s, "h1"); h.State != StatePoweringOn {
		t.Fatalf("host write survived failed tx: %+v", h)
	}
}

func TestSeedOnlyFromUnknown(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	if ok, err := s.Seed("h1", StateOn, t0); err != nil || !ok {
		t.Fatalf("seed ok=%v err=%v", ok, err)
	}
	if ok, _ := s.Seed("h1", StateSoftOff, t0); ok {
		t.Fatal("seed overwrote a known state")
	}
	if h := mustHost(t, s, "h1"); h.State != StateOn {
		t.Fatalf("state = %q", h.State)
	}
}

func TestRecoverCommand(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	q := enqueue(t, s, "h1", "powerup")
	s.ClaimNext("h1", t0)
	now := t0.Add(time.Minute)
	ok, err := s.Recover(q.Command.ID, StateSoftOff, true, "interrupted", now)
	if err != nil || !ok {
		t.Fatalf("recover ok=%v err=%v", ok, err)
	}
	h := mustHost(t, s, "h1")
	if h.State != StateSoftOff || h.PowerFailedAt == nil || h.LastError != "interrupted" || h.Attempt == q.Command.Attempt {
		t.Fatalf("host = %+v", h)
	}
	c, _ := s.GetCommand(q.Command.ID)
	if c.State != CmdFailed || c.Error != "interrupted" {
		t.Fatalf("command = %+v", c)
	}
	running, _ := s.ListCommands(CommandFilter{State: CmdRunning})
	if len(running) != 0 {
		t.Fatalf("running = %+v", running)
	}
}

func TestListCommandsFilters(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	s.EnsureHost("h2", t0)
	setState(t, s, "h1", StateOn)
	enqueue(t, s, "h1", "powerup") // succeeded at once
	enqueue(t, s, "h2", "powerup")
	all, _ := s.ListCommands(CommandFilter{})
	if len(all) != 2 || all[0].ID < all[1].ID {
		t.Fatalf("want newest first: %+v", all)
	}
	h2, _ := s.ListCommands(CommandFilter{Hostname: "h2"})
	if len(h2) != 1 || h2[0].Hostname != "h2" {
		t.Fatalf("hostname filter: %+v", h2)
	}
	ok, _ := s.ListCommands(CommandFilter{State: CmdSucceeded})
	if len(ok) != 1 {
		t.Fatalf("state filter: %+v", ok)
	}
	since := t0.Add(time.Second)
	none, _ := s.ListCommands(CommandFilter{Since: &since})
	if len(none) != 0 {
		t.Fatalf("since filter: %+v", none)
	}
	act, _ := s.ActiveCommand("h2")
	if act == nil || act.State != CmdQueued {
		t.Fatalf("active = %+v", act)
	}
	if act, _ := s.ActiveCommand("h1"); act != nil {
		t.Fatalf("h1 active = %+v", act)
	}
}

func TestMigration2FromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	up := ts(t0)
	for _, q := range []string{
		migrations[0],
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version VALUES (1)`,
		`INSERT INTO hosts (hostname, state, attempt, last_error, updated_at) VALUES ('h1','off','a1','boom','` + up + `'), ('h2','on','a2','','` + up + `')`,
		`INSERT INTO commands (hostname, action, state, requested_by, attempt, created_at, deadline, error) VALUES ('h1','powerdown','succeeded','jared','a1','` + up + `','` + up + `','')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.SchemaVersion(); v != 2 {
		t.Fatalf("SchemaVersion = %d, want 2", v)
	}
	h1 := mustHost(t, s, "h1")
	if h1.State != StateUnknown || h1.LastError != "boom" || h1.Attempt != "a1" || !h1.StateSince.Equal(t0) {
		t.Fatalf("h1 = %+v", h1)
	}
	if h2 := mustHost(t, s, "h2"); h2.State != StateOn || !h2.StateSince.Equal(t0) {
		t.Fatalf("h2 = %+v", h2)
	}
	cmds, err := s.ListCommands(CommandFilter{Hostname: "h1"})
	if err != nil || len(cmds) != 1 || cmds[0].Action != "powerdown" || cmds[0].RequestedBy != "jared" {
		t.Fatalf("commands = %+v (%v)", cmds, err)
	}
	// The rebuilt CHECK accepts powercut.
	if _, err := s.db.Exec(`INSERT INTO commands (hostname, action, state, requested_by, attempt, created_at, deadline) VALUES ('h1','powercut','queued','x','a','` + up + `','` + up + `')`); err != nil {
		t.Fatalf("powercut insert: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO commands (hostname, action, state, requested_by, attempt, created_at, deadline) VALUES ('h1','bogus','queued','x','a','` + up + `','` + up + `')`); err == nil {
		t.Fatal("CHECK should reject bogus actions")
	}
	if _, err := s.db.Exec(`SELECT 1 FROM sqlite_master WHERE name='commands_host_state'`); err != nil {
		t.Fatal(err)
	}
}

func TestStateSinceMovesOnlyOnStateChange(t *testing.T) {
	s := openTest(t)
	s.EnsureHost("h1", t0)
	if h := mustHost(t, s, "h1"); !h.StateSince.Equal(t0) {
		t.Fatalf("initial = %v", h.StateSince)
	}
	t1, t2, t3 := t0.Add(time.Minute), t0.Add(2*time.Minute), t0.Add(3*time.Minute)
	if ok, _ := s.Seed("h1", StateSoftOff, t1); !ok {
		t.Fatal("seed")
	}
	if h := mustHost(t, s, "h1"); !h.StateSince.Equal(t1) || !h.UpdatedAt.Equal(t1) {
		t.Fatalf("after seed: %+v", h)
	}
	// powerup fails and ends in soft_off again: the host passed through
	// powering_on, so state_since moved at enqueue and moves again at finish.
	q := enqueue(t, s, "h1", "powerup")
	if h := mustHost(t, s, "h1"); h.State != StatePoweringOn || !h.StateSince.Equal(t0) {
		t.Fatalf("after enqueue: %+v", h)
	}
	// Terminal write into the same state it is already in leaves state_since.
	if _, err := s.db.Exec(`UPDATE hosts SET state='soft_off', state_since=? WHERE hostname='h1'`, ts(t2)); err != nil {
		t.Fatal(err)
	}
	ok, _, err := s.Finish(FinishParams{Hostname: "h1", Attempt: q.Command.Attempt, CommandID: q.Command.ID,
		HostState: StateSoftOff, LastError: "ssh_down", CommandState: CmdFailed, Error: "ssh_down", Now: t3})
	if err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	h := mustHost(t, s, "h1")
	if !h.StateSince.Equal(t2) || !h.UpdatedAt.Equal(t3) || h.LastError != "ssh_down" {
		t.Fatalf("same-state write moved state_since: %+v", h)
	}
	// A change moves it.
	q = enqueue(t, s, "h1", "powercut")
	ok, _, _ = s.Finish(FinishParams{Hostname: "h1", Attempt: q.Command.Attempt, CommandID: q.Command.ID,
		HostState: StateColdOff, CommandState: CmdSucceeded, Now: t3.Add(time.Minute)})
	if h := mustHost(t, s, "h1"); !ok || h.State != StateColdOff || !h.StateSince.Equal(t3.Add(time.Minute)) {
		t.Fatalf("change did not move state_since: %+v", h)
	}
}

func TestEnqueueAlreadyThereForOffStates(t *testing.T) {
	cases := []struct {
		state, action string
		already       bool
	}{
		{StateSoftOff, "powerdown", true}, {StateColdOff, "powerdown", true},
		{StateColdOff, "powercut", true}, {StateSoftOff, "powercut", false}, {StateOn, "powercut", false},
		{StateOn, "powerup", true}, {StateSoftOff, "powerup", false}, {StateColdOff, "powerup", false},
		{StateOn, "powerdown", false},
	}
	for _, tc := range cases {
		s := openTest(t)
		s.EnsureHost("h1", t0)
		setState(t, s, "h1", tc.state)
		r, err := s.Enqueue(EnqueueParams{Hostname: "h1", Action: tc.action, RequestedBy: "x", Now: t0, Deadline: t0.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Outcome == OutcomeAlreadyThere; got != tc.already {
			t.Errorf("%s %s: already_there = %v, want %v", tc.state, tc.action, got, tc.already)
		}
		if !tc.already {
			if h := mustHost(t, s, "h1"); tc.action == "powercut" && h.State != StatePoweringOff {
				t.Errorf("powercut transitional = %s", h.State)
			}
		}
	}
}
