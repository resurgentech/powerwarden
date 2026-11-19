// Package store is powerwarden's private SQLite state: demands, workers,
// claims, host_policy, and settings.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"

	_ "modernc.org/sqlite"
)

var (
	ErrBadRequest = errors.New("bad_request")
	ErrCapacity   = errors.New("capacity")
	ErrNotFound   = errors.New("not_found")
)

// tsLayout is fixed width so stored timestamps compare correctly as text.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

const settingLastPowerupRequest = "last_powerup_request"

// Limits are the config values the store enforces.
type Limits struct {
	DemandLeaseDefault, DemandLeaseMin, DemandLeaseMax time.Duration
	WorkerLeaseDefault, WorkerLeaseMin, WorkerLeaseMax time.Duration
	MaxDemands                                         int
	MaxTags                                            int
	ClaimLeaseDefault, ClaimLeaseMin, ClaimLeaseMax    time.Duration
	ClaimCapDefault, ClaimCapMin, ClaimCapMax          time.Duration
	MaxClaims                                          int
}

type Store struct {
	db  *sql.DB
	lim Limits
}

var migrations = []string{`
CREATE TABLE IF NOT EXISTS demands (
  id            TEXT PRIMARY KEY,
  tags_json     TEXT NOT NULL,
  lease_seconds INTEGER NOT NULL,
  lease_until   TEXT NOT NULL,
  source        TEXT NOT NULL,
  external_ref  TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workers (
  worker_id     TEXT PRIMARY KEY,
  hostname      TEXT NOT NULL,
  tags_json     TEXT NOT NULL,
  job_status    TEXT NOT NULL,
  demand_id     TEXT NOT NULL DEFAULT '',
  lease_seconds INTEGER NOT NULL,
  lease_until   TEXT NOT NULL,
  last_busy_at  TEXT,
  updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS host_policy (
  hostname           TEXT PRIMARY KEY,
  exclude            INTEGER NOT NULL DEFAULT 0,
  tags_json          TEXT,
  idle_since         TEXT,
  last_powerup_ok_at TEXT,
  last_request_id    INTEGER,
  last_request_at    TEXT
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`, `
CREATE TABLE IF NOT EXISTS claims (
  id            TEXT PRIMARY KEY,
  owner         TEXT NOT NULL,
  session       TEXT NOT NULL DEFAULT '',
  purpose       TEXT NOT NULL,
  mode          TEXT NOT NULL,
  detached      INTEGER NOT NULL DEFAULT 0,
  tags_json     TEXT NOT NULL,
  hostname      TEXT,
  state         TEXT NOT NULL,
  detail        TEXT NOT NULL DEFAULT '',
  version       INTEGER NOT NULL,
  lease_seconds INTEGER,
  lease_until   TEXT,
  expires_at    TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS claims_hostname ON claims (hostname);`}

// Open opens (creating if needed) the database at path and runs migrations.
func Open(path string, lim Limits) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, lim: lim}, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("store: user_version: %w", err)
	}
	for i := v; i < len(migrations); i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return fmt.Errorf("store: set user_version: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// ---- demands ----

// DemandPut is the body of PUT /v1/demands/{id}. A nil LeaseSeconds means
// "default on create, keep stored on refresh".
type DemandPut struct {
	ID           string
	Tags         []string
	LeaseSeconds *int
	Source       string
	ExternalRef  string
}

// PutDemand creates or refreshes a demand.
func (s *Store) PutDemand(ctx context.Context, in DemandPut, now time.Time) (model.Demand, error) {
	if !DemandIDPattern.MatchString(in.ID) {
		return model.Demand{}, badf("id %q does not match %s", in.ID, DemandIDPattern)
	}
	tags, err := normTags(in.Tags, s.lim.MaxTags)
	if err != nil {
		return model.Demand{}, err
	}
	if in.Source != model.SourceHourchd && in.Source != model.SourceManual {
		return model.Demand{}, badf("source must be hourchd or manual")
	}
	if len(in.ExternalRef) > maxExternalRef {
		return model.Demand{}, badf("external_ref longer than %d", maxExternalRef)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Demand{}, err
	}
	defer tx.Rollback()

	existing, err := scanDemand(tx.QueryRowContext(ctx, demandSelect+` WHERE id = ?`, in.ID))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.Demand{}, err
	}
	lease := int(s.lim.DemandLeaseDefault / time.Second)
	if in.LeaseSeconds != nil {
		lease = *in.LeaseSeconds
		if err := checkLease(lease, s.lim.DemandLeaseMin, s.lim.DemandLeaseMax); err != nil {
			return model.Demand{}, err
		}
	} else if found {
		lease = existing.LeaseSeconds
	}
	created := now
	if found {
		created = existing.CreatedAt
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM demands WHERE lease_until > ?`, ts(now)).Scan(&n); err != nil {
			return model.Demand{}, err
		}
		if n >= s.lim.MaxDemands {
			return model.Demand{}, fmt.Errorf("%w: %d open demands", ErrCapacity, n)
		}
	}
	d := model.Demand{ID: in.ID, Tags: tags, LeaseSeconds: lease, LeaseUntil: now.Add(time.Duration(lease) * time.Second),
		Source: in.Source, ExternalRef: in.ExternalRef, CreatedAt: created, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO demands (id, tags_json, lease_seconds, lease_until, source, external_ref, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET tags_json=excluded.tags_json, lease_seconds=excluded.lease_seconds,
		  lease_until=excluded.lease_until, source=excluded.source, external_ref=excluded.external_ref,
		  updated_at=excluded.updated_at`,
		d.ID, mustJSON(d.Tags), d.LeaseSeconds, ts(d.LeaseUntil), d.Source, d.ExternalRef, ts(d.CreatedAt), ts(d.UpdatedAt))
	if err != nil {
		return model.Demand{}, err
	}
	return normDemand(d), tx.Commit()
}

// DeleteDemand removes a demand; absent ids are not an error.
func (s *Store) DeleteDemand(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM demands WHERE id = ?`, id)
	return err
}

// GetDemand returns one demand, expired or not.
func (s *Store) GetDemand(ctx context.Context, id string) (model.Demand, error) {
	d, err := scanDemand(s.db.QueryRowContext(ctx, demandSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// ListDemands returns demands ordered by created_at, id. Expired rows only with includeExpired.
func (s *Store) ListDemands(ctx context.Context, now time.Time, includeExpired bool) ([]model.Demand, error) {
	q := demandSelect
	args := []any{}
	if !includeExpired {
		q += ` WHERE lease_until > ?`
		args = append(args, ts(now))
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Demand
	for rows.Next() {
		d, err := scanDemand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const demandSelect = `SELECT id, tags_json, lease_seconds, lease_until, source, external_ref, created_at, updated_at FROM demands`

func scanDemand(r interface{ Scan(...any) error }) (model.Demand, error) {
	var d model.Demand
	var tags, lu, ca, ua string
	if err := r.Scan(&d.ID, &tags, &d.LeaseSeconds, &lu, &d.Source, &d.ExternalRef, &ca, &ua); err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(tags), &d.Tags); err != nil {
		return d, err
	}
	d.LeaseUntil, d.CreatedAt, d.UpdatedAt = parseTS(lu), parseTS(ca), parseTS(ua)
	return normDemand(d), nil
}

func normDemand(d model.Demand) model.Demand {
	if d.Tags == nil {
		d.Tags = []string{}
	}
	return d
}

// ---- workers ----

// WorkerPut is the body of PUT /v1/workers/{worker_id}.
type WorkerPut struct {
	WorkerID     string
	Hostname     string
	Tags         []string
	JobStatus    string
	DemandID     string
	LeaseSeconds *int
}

// PutWorker creates or refreshes a liveness record. last_busy_at is set to
// now on busy or maintenance, cleared on idle, and kept on error.
func (s *Store) PutWorker(ctx context.Context, in WorkerPut, now time.Time) (model.Worker, error) {
	if !WorkerIDPattern.MatchString(in.WorkerID) {
		return model.Worker{}, badf("worker_id %q does not match %s", in.WorkerID, WorkerIDPattern)
	}
	if !hostnamePattern.MatchString(in.Hostname) {
		return model.Worker{}, badf("hostname %q is invalid", in.Hostname)
	}
	tags, err := normTags(in.Tags, s.lim.MaxTags)
	if err != nil {
		return model.Worker{}, err
	}
	switch in.JobStatus {
	case model.JobIdle, model.JobBusy, model.JobError, model.JobMaintenance:
	default:
		return model.Worker{}, badf("job_status must be idle, busy, error or maintenance")
	}
	if in.DemandID != "" && !DemandIDPattern.MatchString(in.DemandID) {
		return model.Worker{}, badf("demand_id %q does not match %s", in.DemandID, DemandIDPattern)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Worker{}, err
	}
	defer tx.Rollback()
	existing, err := scanWorker(tx.QueryRowContext(ctx, workerSelect+` WHERE worker_id = ?`, in.WorkerID))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.Worker{}, err
	}

	w := model.Worker{WorkerID: in.WorkerID, Hostname: in.Hostname, Tags: tags, JobStatus: in.JobStatus,
		DemandID: in.DemandID, UpdatedAt: now}
	lease := int(s.lim.WorkerLeaseDefault / time.Second)
	if in.LeaseSeconds != nil {
		lease = *in.LeaseSeconds
		if err := checkLease(lease, s.lim.WorkerLeaseMin, s.lim.WorkerLeaseMax); err != nil {
			return model.Worker{}, err
		}
	} else if found {
		lease = existing.LeaseSeconds
	}
	w.LeaseSeconds = lease
	w.LeaseUntil = now.Add(time.Duration(lease) * time.Second)
	switch in.JobStatus {
	case model.JobBusy, model.JobMaintenance:
		t := now
		w.LastBusyAt = &t
	case model.JobError:
		if found {
			w.LastBusyAt = existing.LastBusyAt
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workers (worker_id, hostname, tags_json, job_status, demand_id, lease_seconds, lease_until, last_busy_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(worker_id) DO UPDATE SET hostname=excluded.hostname, tags_json=excluded.tags_json,
		  job_status=excluded.job_status, demand_id=excluded.demand_id, lease_seconds=excluded.lease_seconds,
		  lease_until=excluded.lease_until, last_busy_at=excluded.last_busy_at, updated_at=excluded.updated_at`,
		w.WorkerID, w.Hostname, mustJSON(w.Tags), w.JobStatus, w.DemandID, w.LeaseSeconds, ts(w.LeaseUntil),
		nullTS(w.LastBusyAt), ts(w.UpdatedAt))
	if err != nil {
		return model.Worker{}, err
	}
	return normWorker(w), tx.Commit()
}

// DeleteWorker removes a worker row; absent ids are not an error.
func (s *Store) DeleteWorker(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workers WHERE worker_id = ?`, id)
	return err
}

// ListWorkers returns workers ordered by worker_id. Expired rows only with includeExpired.
func (s *Store) ListWorkers(ctx context.Context, now time.Time, includeExpired bool) ([]model.Worker, error) {
	q := workerSelect
	args := []any{}
	if !includeExpired {
		q += ` WHERE lease_until > ?`
		args = append(args, ts(now))
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY worker_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

const workerSelect = `SELECT worker_id, hostname, tags_json, job_status, demand_id, lease_seconds, lease_until, last_busy_at, updated_at FROM workers`

func scanWorker(r interface{ Scan(...any) error }) (model.Worker, error) {
	var w model.Worker
	var tags, lu, ua string
	var lb sql.NullString
	if err := r.Scan(&w.WorkerID, &w.Hostname, &tags, &w.JobStatus, &w.DemandID, &w.LeaseSeconds, &lu, &lb, &ua); err != nil {
		return w, err
	}
	if err := json.Unmarshal([]byte(tags), &w.Tags); err != nil {
		return w, err
	}
	w.LeaseUntil, w.UpdatedAt, w.LastBusyAt = parseTS(lu), parseTS(ua), parseNullTS(lb)
	return normWorker(w), nil
}

func normWorker(w model.Worker) model.Worker {
	if w.Tags == nil {
		w.Tags = []string{}
	}
	return w
}

// ---- sweep ----

// Sweep deletes demands whose lease ended more than demandAfter ago and
// workers whose lease ended more than workerAfter ago (busy_grace + worker_sweep_extra).
func (s *Store) Sweep(ctx context.Context, now time.Time, demandAfter, workerAfter time.Duration) (demands, workers int64, err error) {
	r, err := s.db.ExecContext(ctx, `DELETE FROM demands WHERE lease_until < ?`, ts(now.Add(-demandAfter)))
	if err != nil {
		return 0, 0, err
	}
	demands, _ = r.RowsAffected()
	r, err = s.db.ExecContext(ctx, `DELETE FROM workers WHERE lease_until < ?`, ts(now.Add(-workerAfter)))
	if err != nil {
		return demands, 0, err
	}
	workers, _ = r.RowsAffected()
	return demands, workers, nil
}

// ---- host_policy ----

// HostPolicies returns every stored policy row keyed by hostname.
func (s *Store) HostPolicies(ctx context.Context) (map[string]model.HostPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hostname, exclude, tags_json, idle_since, last_powerup_ok_at, last_request_id, last_request_at FROM host_policy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.HostPolicy{}
	for rows.Next() {
		var p model.HostPolicy
		var ex int
		var tags, idle, ok, reqAt sql.NullString
		var reqID sql.NullInt64
		if err := rows.Scan(&p.Hostname, &ex, &tags, &idle, &ok, &reqID, &reqAt); err != nil {
			return nil, err
		}
		p.Exclude = ex != 0
		if tags.Valid {
			p.Tags = []string{}
			if err := json.Unmarshal([]byte(tags.String), &p.Tags); err != nil {
				return nil, fmt.Errorf("host_policy %s tags: %w", p.Hostname, err)
			}
		}
		p.IdleSince, p.LastPowerupOKAt, p.LastRequestAt = parseNullTS(idle), parseNullTS(ok), parseNullTS(reqAt)
		if reqID.Valid {
			id := reqID.Int64
			p.LastRequestID = &id
		}
		out[p.Hostname] = p
	}
	return out, rows.Err()
}

func (s *Store) setPolicy(ctx context.Context, hostname, col string, v any) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO host_policy (hostname, %[1]s) VALUES (?, ?)
		ON CONFLICT(hostname) DO UPDATE SET %[1]s = excluded.%[1]s`, col), hostname, v)
	return err
}

func (s *Store) SetExclude(ctx context.Context, hostname string, exclude bool) error {
	v := 0
	if exclude {
		v = 1
	}
	return s.setPolicy(ctx, hostname, "exclude", v)
}

// SetHostTags stores a host's capability tags, replacing any seed.
func (s *Store) SetHostTags(ctx context.Context, hostname string, tags []string) ([]string, error) {
	norm, err := normTags(tags, s.lim.MaxTags)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(norm)
	return norm, s.setPolicy(ctx, hostname, "tags_json", string(b))
}

// SeedHostTags stores tags for hosts that have none stored yet. Hosts with
// stored tags (including an empty list) keep them.
func (s *Store) SeedHostTags(ctx context.Context, seed map[string][]string) error {
	for hostname, tags := range seed {
		norm, err := normTags(tags, s.lim.MaxTags)
		if err != nil {
			return fmt.Errorf("host_tags %s: %w", hostname, err)
		}
		b, _ := json.Marshal(norm)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO host_policy (hostname, tags_json) VALUES (?, ?)
			ON CONFLICT(hostname) DO UPDATE SET tags_json = excluded.tags_json WHERE host_policy.tags_json IS NULL`,
			hostname, string(b)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetIdleSince(ctx context.Context, hostname string, t *time.Time) error {
	return s.setPolicy(ctx, hostname, "idle_since", nullTS(t))
}

func (s *Store) SetLastPowerupOK(ctx context.Context, hostname string, t *time.Time) error {
	return s.setPolicy(ctx, hostname, "last_powerup_ok_at", nullTS(t))
}

// RecordRequest stores the last power request powerwarden made for a host.
func (s *Store) RecordRequest(ctx context.Context, hostname string, commandID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO host_policy (hostname, last_request_id, last_request_at) VALUES (?, ?, ?)
		ON CONFLICT(hostname) DO UPDATE SET last_request_id = excluded.last_request_id, last_request_at = excluded.last_request_at`,
		hostname, commandID, ts(at))
	return err
}

// ---- settings ----

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// LastPowerupRequest is the time of the last successful powerup request (zero if none).
func (s *Store) LastPowerupRequest(ctx context.Context) (time.Time, error) {
	v, ok, err := s.Setting(ctx, settingLastPowerupRequest)
	if err != nil || !ok {
		return time.Time{}, err
	}
	return parseTS(v), nil
}

func (s *Store) SetLastPowerupRequest(ctx context.Context, t time.Time) error {
	return s.SetSetting(ctx, settingLastPowerupRequest, ts(t))
}

// ---- helpers ----

func checkLease(sec int, min, max time.Duration) error {
	d := time.Duration(sec) * time.Second
	if d < min || d > max {
		return badf("lease_seconds %d outside [%d, %d]", sec, int(min/time.Second), int(max/time.Second))
	}
	return nil
}

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseNullTS(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
