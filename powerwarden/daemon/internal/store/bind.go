package store

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

// Claim details.
const (
	// DetailNoHost is the detail of a claim whose tags match no host.
	DetailNoHost = "no_host"
	// DetailDraining: reserve or dedicated, bound, waiting for busy workers.
	DetailDraining = "draining"
	// DetailCooldown: every match this claim could use is in boot cooldown.
	DetailCooldown = "cooldown"
	// DetailBooting: the bound host has a powerup or reboot in flight.
	DetailBooting = "booting"
	// DetailWaitingPowerup: the bound host is off; powerwarden will boot it.
	DetailWaitingPowerup = "waiting for powerup"
	// DetailCommandInFlight: the bound host has another command in flight.
	DetailCommandInFlight = "command in flight"
	detailNoInventory     = "waiting for host inventory"
	detailWaitingForPref  = "waiting for "
)

// BindHost is one host as binding sees it. reconcile.BindHosts builds the
// snapshot from the powermaster view, host policy, workers and name tags.
type BindHost struct {
	Name string
	// Tags are the host's match tags: explicit tags plus name tags.
	Tags             []string
	Locked, Excluded bool
	// Rank is the host's inventory position: it breaks ties, then Name.
	Rank int
	// State is the powermaster power state.
	State string
	// Active: a command is in flight on the host. Booting: that command is
	// a queued or running powerup or reboot.
	Active, Booting bool
	// Cooldown: a failed boot's boot_cooldown has not ended.
	Cooldown bool
	// Busy: a fresh worker on the host is busy or in maintenance.
	Busy bool
}

// occupancy counts the live claims bound to one host (pending or ready).
type occupancy struct{ dedicated, total int }

type occMap map[string]occupancy

func (m occMap) add(host, mode string, n int) {
	o := m[host]
	o.total += n
	if mode == model.ClaimDedicated {
		o.dedicated += n
	}
	m[host] = o
}

// bindTx re-evaluates every live claim that has not failed, oldest first,
// against hosts and the claims bound so far, and writes the changes. A claim
// named by fresh was inserted in this transaction: its first evaluation does
// not bump version. Failed claims are final and never re-evaluated.
func bindTx(ctx context.Context, tx *sql.Tx, hosts []BindHost, now time.Time, fresh string) ([]model.Claim, error) {
	live, err := listClaims(ctx, tx, now, ClaimFilter{})
	if err != nil {
		return nil, err
	}
	byName := make(map[string]BindHost, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}
	occ := occMap{}
	for _, c := range live {
		if c.Hostname != "" && c.State != model.ClaimFailed { // a failed claim holds nothing
			occ.add(c.Hostname, c.Mode, 1)
		}
	}
	var changed []model.Claim
	for _, c := range live {
		if c.State == model.ClaimFailed {
			continue
		}
		state, host, detail := evaluate(c, hosts, byName, occ)
		if state == c.State && host == c.Hostname && detail == c.Detail {
			continue
		}
		c.State, c.Hostname, c.Detail, c.UpdatedAt = state, host, detail, now
		if c.ID != fresh {
			c.Version++
		}
		var hn any
		if host != "" {
			hn = host
		}
		if _, err := tx.ExecContext(ctx, `UPDATE claims SET state = ?, hostname = ?, detail = ?, version = ?, updated_at = ? WHERE id = ?`,
			c.State, hn, c.Detail, c.Version, ts(c.UpdatedAt), c.ID); err != nil {
			return nil, err
		}
		changed = append(changed, c)
	}
	return changed, nil
}

// evaluate decides one claim and keeps occ in step with its binding.
//
// A bound claim never moves. It fails (final, keeping its hostname as a
// record and holding nothing) when its host is gone from the inventory, or,
// for hold and reserve, when the host is not on and is locked, excluded or
// in boot cooldown (see failReason). A dedicated claim is failed only by the
// host leaving the inventory. Otherwise its state comes from BoundState.
// An unbound claim is bound by bindHost.
func evaluate(c model.Claim, hosts []BindHost, byName map[string]BindHost, occ occMap) (state, host, detail string) {
	if hosts == nil {
		if c.Hostname == "" {
			return model.ClaimPending, "", detailNoInventory
		}
		return c.State, c.Hostname, c.Detail
	}
	if c.Hostname != "" {
		h, ok := byName[c.Hostname]
		switch {
		case !ok:
			occ.add(c.Hostname, c.Mode, -1)
			return model.ClaimFailed, c.Hostname, "host " + c.Hostname + " removed from inventory"
		case failReason(c.Mode, h) != "":
			occ.add(c.Hostname, c.Mode, -1)
			return model.ClaimFailed, c.Hostname, failReason(c.Mode, h)
		}
		state, detail = BoundState(c.Mode, c.State, h)
		return state, h.Name, detail
	}
	state, host, detail = bindHost(c, hosts, occ)
	if host == "" {
		return state, "", detail
	}
	occ.add(host, c.Mode, 1)
	state, detail = BoundState(c.Mode, "", byName[host])
	return state, host, detail
}

// failReason is why a hold or reserve claim bound to h has failed, or "". The
// host is not on and is locked or excluded (while off), or its failed-boot
// cooldown is running (a powerup for the claim, or a reboot its holder asked
// for, failed). On an on host, and for dedicated claims, nothing fails.
func failReason(mode string, h BindHost) string {
	if mode == model.ClaimDedicated {
		return ""
	}
	switch {
	case h.Locked && pm.IsOff(h.State):
		return "host " + h.Name + " locked"
	case h.Excluded && pm.IsOff(h.State):
		return "host " + h.Name + " excluded"
	case h.Cooldown && h.State != pm.StateOn:
		return "boot failed on " + h.Name
	}
	return ""
}

// BoundState is the one ready condition for a claim bound to h. prev is the
// claim's state on this same host ("" for a new binding).
//
//   - hold, reserve: pending until h is on with no command in flight
//     (detail booting, command in flight, waiting for powerup, or
//     host <state>). So a powerup or reboot on the host makes the claim
//     pending until the host is on again.
//   - reserve, dedicated: then pending, detail draining, while a fresh
//     worker on h is busy or in maintenance. Once ready, a worker that goes
//     busy does not move the claim back.
//   - dedicated: the power state does not matter.
func BoundState(mode, prev string, h BindHost) (state, detail string) {
	if mode != model.ClaimDedicated {
		switch {
		case h.Booting:
			return model.ClaimPending, DetailBooting
		case h.Active:
			return model.ClaimPending, DetailCommandInFlight
		case pm.IsOff(h.State):
			return model.ClaimPending, DetailWaitingPowerup
		case h.State != pm.StateOn:
			return model.ClaimPending, "host " + h.State
		}
		if mode == model.ClaimHold {
			return model.ClaimReady, ""
		}
	}
	if h.Busy && prev != model.ClaimReady {
		return model.ClaimPending, DetailDraining
	}
	return model.ClaimReady, ""
}

// bindHost picks a host for an unbound claim (ROADMAP PWC7 Binding, PWC8
// order). Match by tags; no match fails with no_host; every match locked or
// excluded fails naming each. Otherwise the best free host, or pending.
//
//   - hold, reserve: free means no live dedicated claim. In order: an on host
//     with no command in flight (fewest live claims first), a host with a
//     powerup in flight, then an off host with no command and no cooldown
//     (soft_off before cold_off). Ties go by Rank, then Name.
//   - dedicated: free means no live claims and no command in flight. In
//     order: on, soft_off, cold_off/off, any other state; then Rank, Name.
//
// A pending result names no host: the detail is cooldown when every host the
// claim could share is cooling down, else waiting for the usable matches.
func bindHost(c model.Claim, hosts []BindHost, occ occMap) (state, host, detail string) {
	var match, usable []BindHost
	for _, h := range hosts {
		if covers(h.Tags, c.Tags) {
			match = append(match, h)
			if !h.Locked && !h.Excluded {
				usable = append(usable, h)
			}
		}
	}
	if len(match) == 0 {
		return model.ClaimFailed, "", DetailNoHost
	}
	if len(usable) == 0 {
		return model.ClaimFailed, "", unusableDetail(match)
	}
	type cand struct {
		h          BindHost
		class, sub int
	}
	var cands []cand
	open, cooling := 0, 0 // hosts this mode may share, and those cooling down
	for _, h := range usable {
		o := occ[h.Name]
		if c.Mode == model.ClaimDedicated {
			if o.total > 0 {
				continue
			}
			open++
			if !h.Active {
				cands = append(cands, cand{h: h, class: powerRank(h.State)})
			}
			continue
		}
		if o.dedicated > 0 {
			continue
		}
		open++
		switch {
		case h.State == pm.StateOn && !h.Active:
			cands = append(cands, cand{h: h, class: 0, sub: o.total})
		case h.Booting:
			cands = append(cands, cand{h: h, class: 1})
		case pm.IsOff(h.State) && !h.Active && !h.Cooldown:
			cands = append(cands, cand{h: h, class: 2, sub: powerRank(h.State)})
		case h.Cooldown:
			cooling++
		}
	}
	if len(cands) == 0 {
		if open > 0 && cooling == open {
			return model.ClaimPending, "", DetailCooldown
		}
		return model.ClaimPending, "", waitingDetail(usable)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		switch {
		case a.class != b.class:
			return a.class < b.class
		case a.sub != b.sub:
			return a.sub < b.sub
		case a.h.Rank != b.h.Rank:
			return a.h.Rank < b.h.Rank
		}
		return a.h.Name < b.h.Name
	})
	return "", cands[0].h.Name, ""
}

// powerRank orders power states for binding: on, soft_off, cold_off/off, other.
func powerRank(state string) int {
	switch {
	case state == pm.StateOn:
		return 0
	case state == pm.StateSoftOff:
		return 1
	case pm.IsOff(state):
		return 2
	}
	return 3
}

func unusableDetail(match []BindHost) string {
	match = slices.Clone(match)
	sort.Slice(match, func(i, j int) bool { return match[i].Name < match[j].Name })
	parts := make([]string, 0, len(match))
	for _, h := range match {
		why := "locked"
		switch {
		case h.Locked && h.Excluded:
			why = "locked and excluded"
		case h.Excluded:
			why = "excluded"
		}
		parts = append(parts, h.Name+": "+why)
	}
	return strings.Join(parts, "; ")
}

func waitingDetail(usable []BindHost) string {
	names := make([]string, 0, len(usable))
	for _, h := range usable {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return detailWaitingForPref + strings.Join(names, ", ")
}

func covers(have, need []string) bool {
	for _, t := range need {
		if !slices.Contains(have, t) {
			return false
		}
	}
	return true
}

const settingLastAlive = "last_alive_at"

// SetLastAlive records that a reconcile pass ran at t (settings.last_alive_at).
func (s *Store) SetLastAlive(ctx context.Context, t time.Time) error {
	return s.SetSetting(ctx, settingLastAlive, ts(t))
}

// ApplyRestartGrace is the restart grace, run once at start before the first
// pass: a non-detached claim whose lease_until falls after
// settings.last_alive_at and at or before now (it lapsed while powerwarden
// was down) gets lease_until = now + grace, capped at expires_at. Detached
// claims have no lease and are skipped. Version is not bumped. It returns the
// number of claims extended; with no last_alive_at recorded it does nothing.
func (s *Store) ApplyRestartGrace(ctx context.Context, now time.Time, grace time.Duration) (int64, error) {
	last, ok, err := s.Setting(ctx, settingLastAlive)
	if err != nil || !ok {
		return 0, err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE claims SET lease_until = MIN(?, expires_at)
  WHERE detached = 0 AND lease_until > ? AND lease_until <= ? AND expires_at > ?`,
		ts(now.Add(grace)), last, ts(now), ts(now))
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
