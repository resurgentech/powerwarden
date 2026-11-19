// Package store is powermaster's SQLite state: host state and power commands.
//
// One *sql.DB with a single connection, WAL, busy_timeout=5000, and a schema
// version table with migrations applied at open. Host state has three writers
// (enqueue, sequence terminal write, seed/recovery), each one transaction.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Host states.
const (
	StateUnknown     = "unknown"
	StateSoftOff     = "soft_off" // chassis off, outlets (if any) still powered
	StateColdOff     = "cold_off" // chassis off and every outlet cut
	StatePoweringOn  = "powering_on"
	StateOn          = "on"
	StatePoweringOff = "powering_off"
	StateRebooting   = "rebooting"
)

// Command states.
const (
	CmdQueued    = "queued"
	CmdRunning   = "running"
	CmdSucceeded = "succeeded"
	CmdFailed    = "failed"
)

// ErrNotFound is returned for a missing host or command.
var ErrNotFound = errors.New("not found")

const tsLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTS(s string) (time.Time, error) { return time.Parse(tsLayout, s) }

var migrations = []string{
	`CREATE TABLE hosts (
  hostname        TEXT PRIMARY KEY,
  state           TEXT NOT NULL,
  attempt         TEXT NOT NULL DEFAULT '',
  last_error      TEXT NOT NULL DEFAULT '',
  power_failed_at TEXT,
  updated_at      TEXT NOT NULL
);
CREATE TABLE commands (
  id           INTEGER PRIMARY KEY,
  hostname     TEXT NOT NULL,
  action       TEXT NOT NULL CHECK (action IN ('powerup', 'powerdown', 'reboot')),
  state        TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed')),
  force        INTEGER NOT NULL DEFAULT 0,
  escalated    INTEGER NOT NULL DEFAULT 0,
  requested_by TEXT NOT NULL,
  reason       TEXT NOT NULL DEFAULT '',
  attempt      TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  started_at   TEXT,
  deadline     TEXT NOT NULL,
  finished_at  TEXT,
  error        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX commands_host_state ON commands (hostname, state);
CREATE INDEX commands_state ON commands (state);`,
	// 2: two off states. The old `off` is ambiguous (outlets on or cut), so
	// those hosts go back to unknown and the poller re-seeds them.
	// hosts.state_since moves only when the state value changes. SQLite
	// cannot alter a CHECK, so commands is rebuilt to admit powercut.
	`UPDATE hosts SET state='unknown' WHERE state='off';
ALTER TABLE hosts ADD COLUMN state_since TEXT NOT NULL DEFAULT '';
UPDATE hosts SET state_since=updated_at;
CREATE TABLE commands_new (
  id           INTEGER PRIMARY KEY,
  hostname     TEXT NOT NULL,
  action       TEXT NOT NULL CHECK (action IN ('powerup', 'powerdown', 'powercut', 'reboot')),
  state        TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed')),
  force        INTEGER NOT NULL DEFAULT 0,
  escalated    INTEGER NOT NULL DEFAULT 0,
  requested_by TEXT NOT NULL,
  reason       TEXT NOT NULL DEFAULT '',
  attempt      TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  started_at   TEXT,
  deadline     TEXT NOT NULL,
  finished_at  TEXT,
  error        TEXT NOT NULL DEFAULT ''
);
INSERT INTO commands_new SELECT id, hostname, action, state, force, escalated, requested_by, reason, attempt, created_at, started_at, deadline, finished_at, error FROM commands;
DROP TABLE commands;
ALTER TABLE commands_new RENAME TO commands;
CREATE INDEX commands_host_state ON commands (hostname, state);
CREATE INDEX commands_state ON commands (state);`,
}

// Host is a row of the hosts table.
type Host struct {
	Hostname      string
	State         string
	Attempt       string
	LastError     string
	PowerFailedAt *time.Time
	UpdatedAt     time.Time
	StateSince    time.Time // when state last changed value
}

// Command is a row of the commands table. JSON names match the API.
type Command struct {
	ID          int64      `json:"id"`
	Hostname    string     `json:"hostname"`
	Action      string     `json:"action"`
	State       string     `json:"state"`
	Force       bool       `json:"force"`
	Escalated   bool       `json:"escalated"`
	RequestedBy string     `json:"requested_by"`
	Reason      string     `json:"reason"`
	Attempt     string     `json:"attempt"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	Deadline    time.Time  `json:"deadline"`
	FinishedAt  *time.Time `json:"finished_at"`
	Error       string     `json:"error"`
}

// InFlight reports whether the command is queued or running.
func (c Command) InFlight() bool { return c.State == CmdQueued || c.State == CmdRunning }

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB

	// Test hooks, called inside transactions to prove atomicity.
	testHookAfterInsert     func() error
	testHookAfterHostUpdate func() error
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for diagnostics and tests. Callers must not write
// hosts.state through it.
func (s *Store) DB() *sql.DB { return s.db }

// Ping checks the database is usable (for healthz).
func (s *Store) Ping() error { return s.db.Ping() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("schema_version: %w", err)
	}
	v, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`DELETE FROM schema_version`); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the applied migration count.
func (s *Store) SchemaVersion() (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// NewAttempt returns a fresh random attempt token.
func NewAttempt() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// EnsureHost inserts a host row in state unknown if it does not exist.
func (s *Store) EnsureHost(name string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO hosts (hostname, state, updated_at, state_since) VALUES (?, ?, ?, ?) ON CONFLICT(hostname) DO NOTHING`,
		name, StateUnknown, ts(now), ts(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

type rowScanner interface{ Scan(...any) error }

const hostCols = `hostname, state, attempt, last_error, power_failed_at, updated_at, state_since`

func scanHost(r rowScanner) (Host, error) {
	var h Host
	var pfa sql.NullString
	var upd, since string
	if err := r.Scan(&h.Hostname, &h.State, &h.Attempt, &h.LastError, &pfa, &upd, &since); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Host{}, ErrNotFound
		}
		return Host{}, err
	}
	var err error
	if h.UpdatedAt, err = parseTS(upd); err != nil {
		return Host{}, err
	}
	if h.StateSince, err = parseTS(since); err != nil {
		return Host{}, err
	}
	if pfa.Valid {
		t, err := parseTS(pfa.String)
		if err != nil {
			return Host{}, err
		}
		h.PowerFailedAt = &t
	}
	return h, nil
}

// GetHost returns one host row.
func (s *Store) GetHost(name string) (Host, error) {
	return scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE hostname=?`, name))
}

// ListHosts returns every host row (including ones no longer in inventory).
func (s *Store) ListHosts() ([]Host, error) {
	rows, err := s.db.Query(`SELECT ` + hostCols + ` FROM hosts ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

const cmdCols = `id, hostname, action, state, force, escalated, requested_by, reason, attempt, created_at, started_at, deadline, finished_at, error`

func scanCommand(r rowScanner) (Command, error) {
	var c Command
	var created, deadline string
	var started, finished sql.NullString
	if err := r.Scan(&c.ID, &c.Hostname, &c.Action, &c.State, &c.Force, &c.Escalated, &c.RequestedBy, &c.Reason,
		&c.Attempt, &created, &started, &deadline, &finished, &c.Error); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Command{}, ErrNotFound
		}
		return Command{}, err
	}
	var err error
	if c.CreatedAt, err = parseTS(created); err != nil {
		return Command{}, err
	}
	if c.Deadline, err = parseTS(deadline); err != nil {
		return Command{}, err
	}
	for _, p := range []struct {
		src sql.NullString
		dst **time.Time
	}{{started, &c.StartedAt}, {finished, &c.FinishedAt}} {
		if p.src.Valid {
			t, err := parseTS(p.src.String)
			if err != nil {
				return Command{}, err
			}
			*p.dst = &t
		}
	}
	return c, nil
}

type queryer interface {
	QueryRow(string, ...any) *sql.Row
}

func getCommand(q queryer, id int64) (Command, error) {
	return scanCommand(q.QueryRow(`SELECT `+cmdCols+` FROM commands WHERE id=?`, id))
}

// GetCommand returns one command.
func (s *Store) GetCommand(id int64) (Command, error) { return getCommand(s.db, id) }

func activeCommand(q queryer, host string) (*Command, error) {
	c, err := scanCommand(q.QueryRow(`SELECT `+cmdCols+` FROM commands WHERE hostname=? AND state IN ('queued','running') ORDER BY id LIMIT 1`, host))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ActiveCommand returns the host's queued or running command, or nil.
func (s *Store) ActiveCommand(host string) (*Command, error) { return activeCommand(s.db, host) }

// CommandFilter narrows ListCommands. Zero values mean "any".
type CommandFilter struct {
	Hostname string
	State    string
	Since    *time.Time // created_at >= Since
	Limit    int        // default and max 200
}

// ListCommands returns commands newest first.
func (s *Store) ListCommands(f CommandFilter) ([]Command, error) {
	var where []string
	var args []any
	if f.Hostname != "" {
		where = append(where, "hostname=?")
		args = append(args, f.Hostname)
	}
	if f.State != "" {
		where = append(where, "state=?")
		args = append(args, f.State)
	}
	if f.Since != nil {
		where = append(where, "created_at>=?")
		args = append(args, ts(*f.Since))
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	q := `SELECT ` + cmdCols + ` FROM commands`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Command{}
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Outcome of an enqueue.
type Outcome int

const (
	// OutcomeQueued: a new command was queued (202).
	OutcomeQueued Outcome = iota
	// OutcomeExisting: the same action is already queued or running (202).
	OutcomeExisting
	// OutcomeAlreadyThere: the target state already holds; a succeeded row was stored (200).
	OutcomeAlreadyThere
	// OutcomeConflict: a different action is in flight (409 command_in_flight).
	OutcomeConflict
)

func (o Outcome) String() string {
	return [...]string{"queued", "existing", "already_there", "conflict"}[o]
}

// EnqueueParams describes a power request. Deadline is now + budget + slack.
type EnqueueParams struct {
	Hostname    string
	Action      string
	Force       bool
	RequestedBy string
	Reason      string
	Now         time.Time
	Deadline    time.Time
}

// EnqueueResult is the outcome and the relevant command row. PriorState is
// the host state before a queued command set the transitional state.
type EnqueueResult struct {
	Outcome    Outcome
	Command    Command
	PriorState string
}

func transitional(action string) (string, error) {
	switch action {
	case "powerup":
		return StatePoweringOn, nil
	case "powerdown", "powercut":
		return StatePoweringOff, nil
	case "reboot":
		return StateRebooting, nil
	}
	return "", fmt.Errorf("invalid action %q", action)
}

// Enqueue is ONE transaction: check for an in-flight command, then either
// return it, store an already-there succeeded row, or insert the queued
// command, set the transitional host state and set the new attempt.
func (s *Store) Enqueue(p EnqueueParams) (EnqueueResult, error) {
	trans, err := transitional(p.Action)
	if err != nil {
		return EnqueueResult{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EnqueueResult{}, err
	}
	defer tx.Rollback()

	host, err := scanHost(tx.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE hostname=?`, p.Hostname))
	if err != nil {
		return EnqueueResult{}, err
	}
	if act, err := activeCommand(tx, p.Hostname); err != nil {
		return EnqueueResult{}, err
	} else if act != nil {
		out := OutcomeConflict
		if act.Action == p.Action {
			out = OutcomeExisting
		}
		return EnqueueResult{Outcome: out, Command: *act, PriorState: host.State}, nil
	}

	already := (p.Action == "powerup" && host.State == StateOn) ||
		(p.Action == "powerdown" && (host.State == StateSoftOff || host.State == StateColdOff)) ||
		(p.Action == "powercut" && host.State == StateColdOff)
	now := ts(p.Now)
	if already {
		res, err := tx.Exec(`INSERT INTO commands (hostname, action, state, force, requested_by, reason, attempt, created_at, started_at, deadline, finished_at)
VALUES (?, ?, 'succeeded', ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.Hostname, p.Action, p.Force, p.RequestedBy, p.Reason, host.Attempt, now, now, ts(p.Deadline), now)
		if err != nil {
			return EnqueueResult{}, err
		}
		id, _ := res.LastInsertId()
		c, err := getCommand(tx, id)
		if err != nil {
			return EnqueueResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return EnqueueResult{}, err
		}
		return EnqueueResult{Outcome: OutcomeAlreadyThere, Command: c, PriorState: host.State}, nil
	}

	attempt := NewAttempt()
	res, err := tx.Exec(`INSERT INTO commands (hostname, action, state, force, requested_by, reason, attempt, created_at, deadline)
VALUES (?, ?, 'queued', ?, ?, ?, ?, ?, ?)`,
		p.Hostname, p.Action, p.Force, p.RequestedBy, p.Reason, attempt, now, ts(p.Deadline))
	if err != nil {
		return EnqueueResult{}, err
	}
	id, _ := res.LastInsertId()
	if s.testHookAfterInsert != nil {
		if err := s.testHookAfterInsert(); err != nil {
			return EnqueueResult{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE hosts SET state=?, attempt=?, updated_at=?, state_since=CASE WHEN state != ? THEN ? ELSE state_since END WHERE hostname=?`, trans, attempt, now, trans, now, p.Hostname); err != nil {
		return EnqueueResult{}, err
	}
	c, err := getCommand(tx, id)
	if err != nil {
		return EnqueueResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{Outcome: OutcomeQueued, Command: c, PriorState: host.State}, nil
}

// ClaimNext moves the host's oldest queued command to running. It returns nil
// if there is none.
func (s *Store) ClaimNext(host string, now time.Time) (*Command, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c, err := scanCommand(tx.QueryRow(`SELECT `+cmdCols+` FROM commands WHERE hostname=? AND state='queued' ORDER BY id LIMIT 1`, host))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE commands SET state='running', started_at=? WHERE id=? AND state='queued'`, ts(now), c.ID); err != nil {
		return nil, err
	}
	c, err = getCommand(tx, c.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &c, nil
}

// MarkEscalated records that a reboot fell back to a PDU cycle.
func (s *Store) MarkEscalated(id int64) (Command, error) {
	if _, err := s.db.Exec(`UPDATE commands SET escalated=1 WHERE id=?`, id); err != nil {
		return Command{}, err
	}
	return s.GetCommand(id)
}

// FinishParams is a sequence's terminal write.
type FinishParams struct {
	Hostname      string
	Attempt       string
	CommandID     int64
	HostState     string
	LastError     string
	PowerFailedAt *time.Time // nil keeps the previous value
	CommandState  string
	Error         string
	Now           time.Time
}

// Finish is ONE transaction guarded by the attempt token: update the host
// WHERE hostname=? AND attempt=?, then the command. Zero host rows matched
// means the attempt was superseded; nothing is written and applied is false.
func (s *Store) Finish(p FinishParams) (applied bool, cmd Command, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, Command{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE hosts SET state=?, last_error=?, power_failed_at=COALESCE(?, power_failed_at), updated_at=?, state_since=CASE WHEN state != ? THEN ? ELSE state_since END
WHERE hostname=? AND attempt=?`,
		p.HostState, p.LastError, tsPtr(p.PowerFailedAt), ts(p.Now), p.HostState, ts(p.Now), p.Hostname, p.Attempt)
	if err != nil {
		return false, Command{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, Command{}, nil
	}
	if s.testHookAfterHostUpdate != nil {
		if err := s.testHookAfterHostUpdate(); err != nil {
			return false, Command{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE commands SET state=?, error=?, finished_at=? WHERE id=?`,
		p.CommandState, p.Error, ts(p.Now), p.CommandID); err != nil {
		return false, Command{}, err
	}
	cmd, err = getCommand(tx, p.CommandID)
	if err != nil {
		return false, Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, Command{}, err
	}
	return true, cmd, nil
}

// Seed sets a host's state from the first IPMI observation, only while the
// host is unknown.
func (s *Store) Seed(host, state string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE hosts SET state=?, updated_at=?, state_since=CASE WHEN state != ? THEN ? ELSE state_since END WHERE hostname=? AND state='unknown'`, state, ts(now), state, ts(now), host)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RunningCommands returns every running command.
func (s *Store) RunningCommands() ([]Command, error) {
	return s.ListCommands(CommandFilter{State: CmdRunning, Limit: 200})
}

// Recover fails a running command whose sequence is gone (process restart or
// orphan) in ONE transaction. The host write (state, last_error, optional
// power_failed_at, new attempt) is guarded by the command's attempt, so a
// host that has moved on is not touched; hostApplied reports whether it was.
func (s *Store) Recover(id int64, hostState string, powerFailed bool, errMsg string, now time.Time) (hostApplied bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, err := getCommand(tx, id)
	if err != nil {
		return false, err
	}
	if c.State != CmdRunning {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE commands SET state='failed', error=?, finished_at=? WHERE id=? AND state='running'`, errMsg, ts(now), id); err != nil {
		return false, err
	}
	var pfa any
	if powerFailed {
		pfa = ts(now)
	}
	res, err := tx.Exec(`UPDATE hosts SET state=?, last_error=?, power_failed_at=COALESCE(?, power_failed_at), attempt=?, updated_at=?, state_since=CASE WHEN state != ? THEN ? ELSE state_since END
WHERE hostname=? AND attempt=?`, hostState, errMsg, pfa, NewAttempt(), ts(now), hostState, ts(now), c.Hostname, c.Attempt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// HostsWithQueued returns hostnames that have a queued command.
func (s *Store) HostsWithQueued() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT hostname FROM commands WHERE state='queued' ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
