package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
)

var (
	// ErrForbidden: the claim belongs to another token name.
	ErrForbidden = errors.New("forbidden")
	// ErrConflict: a re-PUT differs in tags, mode or detached.
	ErrConflict = errors.New("conflict")
	// ErrExpired: the claim has lapsed.
	ErrExpired = errors.New("claim_expired")

	// ErrClaimFailed: the claim failed; it is final and cannot be renewed,
	// extended or powered. Detail is in ClaimFailedError.
	ErrClaimFailed = errors.New("claim_failed")

	ClaimIDPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,160}$`)
	ClaimSessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// ClaimFailedError is ErrClaimFailed with the claim's detail (the reason).
type ClaimFailedError struct{ ID, Detail string }

func (e *ClaimFailedError) Error() string { return "claim " + e.ID + " failed: " + e.Detail }

// Is reports ErrClaimFailed.
func (e *ClaimFailedError) Is(target error) bool { return target == ErrClaimFailed }

// FailedErr is the error for a renew, extend or power call on claim c.
func FailedErr(c model.Claim) error { return &ClaimFailedError{ID: c.ID, Detail: c.Detail} }

const maxPurpose = 256

// ClaimPut is the body of PUT /v1/claims/{id} plus the caller's token name.
type ClaimPut struct {
	ID, Owner, Session, Purpose, Mode string
	Tags                              []string
	Detached                          bool
	MaxSeconds, LeaseSeconds          *int
}

// ClaimFilter narrows ListClaims. Empty fields do not filter.
type ClaimFilter struct {
	Owner, Hostname string
	IncludeExpired  bool
}

const claimCols = `id, owner, session, purpose, mode, detached, tags_json, hostname, state, detail, version,
  lease_seconds, lease_until, expires_at, created_at, updated_at`

const claimSelect = `SELECT ` + claimCols + ` FROM claims`

// liveClause selects claims that have not lapsed; it takes now twice.
const liveClause = `expires_at > ? AND (detached = 1 OR lease_until > ?)`

// PutClaim creates or renews a claim and binds pending claims, all in one
// transaction (so two racing dedicated claims cannot both bind one host).
// hosts is the inventory snapshot; nil means the inventory is not known yet
// and claims stay pending.
func (s *Store) PutClaim(ctx context.Context, in ClaimPut, hosts []BindHost, now time.Time) (model.Claim, error) {
	if !ClaimIDPattern.MatchString(in.ID) {
		return model.Claim{}, badf("id %q does not match %s", in.ID, ClaimIDPattern)
	}
	if in.Owner == "" {
		return model.Claim{}, badf("claim has no owner")
	}
	tags, err := normTags(in.Tags, s.lim.MaxTags)
	if err != nil {
		return model.Claim{}, err
	}
	if len(tags) == 0 {
		return model.Claim{}, badf("tags needs at least one tag")
	}
	if n := utf8.RuneCountInString(in.Purpose); n < 1 || n > maxPurpose {
		return model.Claim{}, badf("purpose must be 1..%d characters", maxPurpose)
	}
	if in.Session != "" && !ClaimSessionPattern.MatchString(in.Session) {
		return model.Claim{}, badf("session %q does not match %s", in.Session, ClaimSessionPattern)
	}
	mode := in.Mode
	if mode == "" {
		mode = model.ClaimHold
	}
	switch mode {
	case model.ClaimHold, model.ClaimReserve, model.ClaimDedicated:
	default:
		return model.Claim{}, badf("mode must be hold, reserve or dedicated")
	}
	if in.Detached && in.LeaseSeconds != nil {
		return model.Claim{}, badf("lease_seconds cannot be combined with detached")
	}
	for name, v := range map[string]*int{"max_seconds": in.MaxSeconds, "lease_seconds": in.LeaseSeconds} {
		if v != nil && *v < 1 {
			return model.Claim{}, badf("%s must be >= 1", name)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Claim{}, err
	}
	defer tx.Rollback()

	existing, err := scanClaim(tx.QueryRowContext(ctx, claimSelect+` WHERE id = ?`, in.ID))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.Claim{}, err
	}
	if found {
		switch {
		case existing.Owner != in.Owner:
			return model.Claim{}, fmt.Errorf("%w: claim %s belongs to another owner", ErrForbidden, in.ID)
		case !existing.Live(now):
			return model.Claim{}, fmt.Errorf("%w: claim %s lapsed", ErrExpired, in.ID)
		case existing.State == model.ClaimFailed:
			return model.Claim{}, FailedErr(existing)
		case !slices.Equal(existing.Tags, tags) || existing.Mode != mode || existing.Detached != in.Detached:
			return model.Claim{}, fmt.Errorf("%w: tags, mode and detached cannot change; release and claim again", ErrConflict)
		}
		if !existing.Detached {
			lease := existing.LeaseSeconds
			if in.LeaseSeconds != nil {
				lease = clampSeconds(*in.LeaseSeconds, s.lim.ClaimLeaseMin, s.lim.ClaimLeaseMax)
			}
			until := now.Add(time.Duration(lease) * time.Second)
			if _, err := tx.ExecContext(ctx, `UPDATE claims SET lease_seconds = ?, lease_until = ?, updated_at = ? WHERE id = ?`,
				lease, ts(until), ts(now), in.ID); err != nil {
				return model.Claim{}, err
			}
		}
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims WHERE state != 'failed' AND `+liveClause, ts(now), ts(now)).Scan(&n); err != nil {
			return model.Claim{}, err
		}
		if n >= s.lim.MaxClaims {
			return model.Claim{}, fmt.Errorf("%w: %d live claims", ErrCapacity, n)
		}
		c := model.Claim{ID: in.ID, Owner: in.Owner, Session: in.Session, Purpose: in.Purpose, Mode: mode,
			Detached: in.Detached, Tags: tags, State: model.ClaimPending, Version: 1, CreatedAt: now, UpdatedAt: now}
		capSec := int(s.lim.ClaimCapDefault / time.Second)
		if in.MaxSeconds != nil {
			capSec = clampSeconds(*in.MaxSeconds, s.lim.ClaimCapMin, s.lim.ClaimCapMax)
		}
		c.ExpiresAt = now.Add(time.Duration(capSec) * time.Second)
		var lease, until any
		if !c.Detached {
			sec := int(s.lim.ClaimLeaseDefault / time.Second)
			if in.LeaseSeconds != nil {
				sec = clampSeconds(*in.LeaseSeconds, s.lim.ClaimLeaseMin, s.lim.ClaimLeaseMax)
			}
			lease, until = sec, ts(now.Add(time.Duration(sec)*time.Second))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO claims (`+claimCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, '', 1, ?, ?, ?, ?, ?)`,
			c.ID, c.Owner, c.Session, c.Purpose, c.Mode, boolInt(c.Detached), mustJSON(c.Tags), c.State,
			lease, until, ts(c.ExpiresAt), ts(c.CreatedAt), ts(c.UpdatedAt)); err != nil {
			return model.Claim{}, err
		}
	}
	fresh := "" // a claim inserted here: its first evaluation does not bump version
	if !found {
		fresh = in.ID
	}
	if _, err := bindTx(ctx, tx, hosts, now, fresh); err != nil {
		return model.Claim{}, err
	}
	out, err := scanClaim(tx.QueryRowContext(ctx, claimSelect+` WHERE id = ?`, in.ID))
	if err != nil {
		return model.Claim{}, err
	}
	return out, tx.Commit()
}

// ExtendClaim sets expires_at = now + maxSeconds, clamped to the cap limits,
// and changes nothing else. owner must equal the claim's owner.
func (s *Store) ExtendClaim(ctx context.Context, id, owner string, maxSeconds int, now time.Time) (model.Claim, error) {
	if maxSeconds < 1 {
		return model.Claim{}, badf("max_seconds must be >= 1")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Claim{}, err
	}
	defer tx.Rollback()
	c, err := scanClaim(tx.QueryRowContext(ctx, claimSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Claim{}, ErrNotFound
	}
	if err != nil {
		return model.Claim{}, err
	}
	switch {
	case c.Owner != owner:
		return model.Claim{}, fmt.Errorf("%w: claim %s belongs to another owner", ErrForbidden, id)
	case !c.Live(now):
		return model.Claim{}, fmt.Errorf("%w: claim %s lapsed", ErrExpired, id)
	case c.State == model.ClaimFailed:
		return model.Claim{}, FailedErr(c)
	}
	sec := clampSeconds(maxSeconds, s.lim.ClaimCapMin, s.lim.ClaimCapMax)
	c.ExpiresAt, c.UpdatedAt = now.Add(time.Duration(sec)*time.Second), now
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET expires_at = ?, updated_at = ? WHERE id = ?`,
		ts(c.ExpiresAt), ts(c.UpdatedAt), id); err != nil {
		return model.Claim{}, err
	}
	return c, tx.Commit()
}

// ReleaseClaim deletes a claim; an absent id is not an error. It is
// ErrForbidden unless caller owns the claim or anyOwner is set (the
// operator release; the caller decides who may use it).
func (s *Store) ReleaseClaim(ctx context.Context, id, caller string, anyOwner bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT owner FROM claims WHERE id = ?`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner != caller && !anyOwner {
		return fmt.Errorf("%w: claim %s belongs to another owner", ErrForbidden, id)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// BumpClaimVersion adds 1 to a claim's version and returns the row. The app
// calls it when the bound host's state changed, which the row does not hold.
// An absent claim is ErrNotFound.
func (s *Store) BumpClaimVersion(ctx context.Context, id string, now time.Time) (model.Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Claim{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE claims SET version = version + 1, updated_at = ? WHERE id = ?`, ts(now), id)
	if err != nil {
		return model.Claim{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Claim{}, ErrNotFound
	}
	c, err := scanClaim(tx.QueryRowContext(ctx, claimSelect+` WHERE id = ?`, id))
	if err != nil {
		return model.Claim{}, err
	}
	return c, tx.Commit()
}

// GetClaim returns one claim, lapsed or not.
func (s *Store) GetClaim(ctx context.Context, id string) (model.Claim, error) {
	c, err := scanClaim(s.db.QueryRowContext(ctx, claimSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ListClaims returns claims ordered by created_at, id. Lapsed rows only with
// f.IncludeExpired.
func (s *Store) ListClaims(ctx context.Context, now time.Time, f ClaimFilter) ([]model.Claim, error) {
	return listClaims(ctx, s.db, now, f)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listClaims(ctx context.Context, q queryer, now time.Time, f ClaimFilter) ([]model.Claim, error) {
	var where []string
	var args []any
	if !f.IncludeExpired {
		where = append(where, liveClause)
		args = append(args, ts(now), ts(now))
	}
	if f.Owner != "" {
		where = append(where, `owner = ?`)
		args = append(args, f.Owner)
	}
	if f.Hostname != "" {
		where = append(where, `hostname = ?`)
		args = append(args, f.Hostname)
	}
	query := claimSelect
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Claim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BindClaims re-evaluates every live claim that has not failed against the
// host snapshot (see bind.go): it binds, unbinds and moves claims between
// pending and ready, and returns the claims whose state, detail or hostname
// changed. hosts nil means the inventory is not known yet.
func (s *Store) BindClaims(ctx context.Context, hosts []BindHost, now time.Time) ([]model.Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	changed, err := bindTx(ctx, tx, hosts, now, "")
	if err != nil {
		return nil, err
	}
	return changed, tx.Commit()
}

// SweepClaims deletes claims that lapsed more than after ago, and failed
// claims that failed more than after ago (updated_at is fixed once a claim
// has failed: it is never evaluated or renewed again).
func (s *Store) SweepClaims(ctx context.Context, now time.Time, after time.Duration) (int64, error) {
	cut := ts(now.Add(-after))
	r, err := s.db.ExecContext(ctx, `DELETE FROM claims WHERE expires_at < ? OR (detached = 0 AND lease_until < ?)
  OR (state = 'failed' AND updated_at < ?)`, cut, cut, cut)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func clampSeconds(sec int, min, max time.Duration) int {
	d := time.Duration(sec) * time.Second
	if sec > int(max/time.Second) || d > max {
		d = max
	}
	if d < min {
		d = min
	}
	return int(d / time.Second)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanClaim(r interface{ Scan(...any) error }) (model.Claim, error) {
	var c model.Claim
	var tags, ex, ca, ua string
	var det int
	var host, lu sql.NullString
	var lease sql.NullInt64
	if err := r.Scan(&c.ID, &c.Owner, &c.Session, &c.Purpose, &c.Mode, &det, &tags, &host, &c.State, &c.Detail,
		&c.Version, &lease, &lu, &ex, &ca, &ua); err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(tags), &c.Tags); err != nil {
		return c, err
	}
	c.Detached, c.Hostname, c.LeaseSeconds = det != 0, host.String, int(lease.Int64)
	c.LeaseUntil = parseNullTS(lu)
	c.ExpiresAt, c.CreatedAt, c.UpdatedAt = parseTS(ex), parseTS(ca), parseTS(ua)
	if c.Tags == nil {
		c.Tags = []string{}
	}
	return c, nil
}
