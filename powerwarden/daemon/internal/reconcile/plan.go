package reconcile

import (
	"sort"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

// Input is everything one pass decides from. Plan is pure over it.
type Input struct {
	Now     time.Time
	Timings config.Timings
	// Hosts in powermaster inventory order.
	Hosts []pm.Host
	// Policy holds host tags (capability), exclude and idle_since. A host
	// with nil or empty tags has no capability.
	Policy map[string]model.HostPolicy
	// Demands are open demands. Workers are all rows, expired included.
	Demands            []model.Demand
	Workers            []model.Worker
	LastPowerupRequest time.Time
	// ViewFresh is false when powermaster is unreachable or the view is
	// older than host_view_refresh: no power requests are planned.
	ViewFresh bool
	// Claims are the live claims after this pass's binding (failed ones
	// included; they have no host). See Plan for their effect.
	Claims []model.Claim
}

// Attempt is one powerup to try. Executed in order until one succeeds.
type Attempt struct {
	// DemandID or ClaimID names what the powerup is for; one is set.
	DemandID string
	ClaimID  string
	Hostname string
}

// HostStatus is the per-host policy view for the API and UI.
type HostStatus struct {
	Hostname       string
	IdleEligible   bool
	IdleSince      *time.Time
	Booting        bool // in-flight powerup/reboot
	BootGraceUntil *time.Time
	CooldownUntil  *time.Time
	BusyHeldUntil  *time.Time
	PairedDemand   string
	// ColdOffDueAt is state_since + cold_off_after for a soft_off host that
	// nothing blocks from a powercut; nil otherwise.
	ColdOffDueAt *time.Time
	Workers      []string // all worker rows (fresh or not) attached by hostname
}

// Output is a pass's plan.
type Output struct {
	// CoveredBy maps every open demand id to worker:<id>, host:<hostname>, or "".
	CoveredBy map[string]string
	// PowerupAttempts is empty when spacing or a stale view forbids a powerup.
	PowerupAttempts []Attempt
	// Powerdown is the host to power down if no powerup was requested.
	Powerdown string
	// Powercut is the soft_off host to power-cut (rule 11) if no powerup or
	// powerdown was requested. Empty when any powerup attempt is planned.
	Powercut string
	// NoCandidate lists uncovered demands with no candidate host.
	NoCandidate []string
	// IdleSince is the desired idle_since per known host (nil = cleared).
	IdleSince map[string]*time.Time
	// ClearPowerupOK lists hosts whose boot grace ended (worker seen or expired).
	ClearPowerupOK []string
	// BootGraceExpired lists hosts whose grace ran out with no fresh worker.
	BootGraceExpired   []string
	Hosts              map[string]HostStatus
	UnknownHostWorkers []string // fresh workers whose hostname is not in inventory
	TagMismatchWorkers []string // fresh workers whose tags differ from host tags
}

// Plan runs the ten ordered reconcile rules.
func Plan(in Input) Output {
	now, t := in.Now, in.Timings
	out := Output{
		CoveredBy: map[string]string{},
		IdleSince: map[string]*time.Time{},
		Hosts:     map[string]HostStatus{},
	}

	// 1. Load (done by the caller); index and order.
	hosts := map[string]pm.Host{}
	names := make([]string, 0, len(in.Hosts))
	for _, h := range in.Hosts {
		names = append(names, h.Hostname)
	}
	idx := NewNameIndex(names)
	// Claims. claimed: a live claim is bound (pending or ready); the host is
	// never idle-eligible (9) and never power-cut (11). blocked: a bound
	// reserve or dedicated claim; the host is never a demand candidate (7)
	// and covers no demand by booting (4, 5). A bound hold or reserve claim
	// asks for its host's powerup in rule 6 when the host is off; dedicated
	// never does, so with 7/9/11 powerwarden sends that host nothing.
	claimed, blocked := map[string]bool{}, map[string]bool{}
	var claimBoots []model.Claim
	for _, c := range in.Claims {
		if c.Hostname == "" || c.State == model.ClaimFailed || !c.Live(now) {
			continue
		}
		claimed[c.Hostname] = true
		if c.Blocks() {
			blocked[c.Hostname] = true
		}
		if c.Mode != model.ClaimDedicated {
			claimBoots = append(claimBoots, c)
		}
	}
	for _, h := range in.Hosts {
		hosts[h.Hostname] = h
		out.Hosts[h.Hostname] = HostStatus{Hostname: h.Hostname}
	}
	demands := append([]model.Demand(nil), in.Demands...)
	sort.SliceStable(demands, func(i, j int) bool {
		if !demands[i].CreatedAt.Equal(demands[j].CreatedAt) {
			return demands[i].CreatedAt.Before(demands[j].CreatedAt)
		}
		return demands[i].ID < demands[j].ID
	})
	byID := map[string]model.Demand{}
	for _, d := range demands {
		byID[d.ID] = d
		out.CoveredBy[d.ID] = ""
	}
	cover := func(id, by string) { out.CoveredBy[id] = by }
	uncovered := func(id string) bool { by, ok := out.CoveredBy[id]; return ok && by == "" }
	// takeOldest assigns the oldest uncovered demand matching ok.
	takeOldest := func(ok func(model.Demand) bool, by string) string {
		for _, d := range demands {
			if uncovered(d.ID) && ok(d) {
				cover(d.ID, by)
				return d.ID
			}
		}
		return ""
	}

	workers := append([]model.Worker(nil), in.Workers...)
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerID < workers[j].WorkerID })
	freshOnHost := map[string]bool{}
	pairedHost := map[string]string{} // hostname -> demand id paired via a fresh worker
	for _, w := range workers {
		if hs, ok := out.Hosts[w.Hostname]; ok {
			hs.Workers = append(hs.Workers, w.WorkerID)
			out.Hosts[w.Hostname] = hs
		}
		if !w.Fresh(now) {
			continue
		}
		if _, known := hosts[w.Hostname]; !known {
			out.UnknownHostWorkers = append(out.UnknownHostWorkers, w.WorkerID)
			continue
		}
		freshOnHost[w.Hostname] = true
		if ht := in.Policy[w.Hostname].Tags; len(ht) > 0 && !sameSet(ht, w.Tags) {
			out.TagMismatchWorkers = append(out.TagMismatchWorkers, w.WorkerID)
		}
	}

	// 2. Busy and maintenance cover only their claimed demand.
	for _, w := range workers {
		if !w.Fresh(now) || (w.JobStatus != model.JobBusy && w.JobStatus != model.JobMaintenance) || w.DemandID == "" {
			continue
		}
		if _, known := hosts[w.Hostname]; !known {
			continue
		}
		d, open := byID[w.DemandID]
		if open && uncovered(d.ID) && Covers(w.Tags, d.Tags) {
			cover(d.ID, "worker:"+w.WorkerID)
			pairedHost[w.Hostname] = d.ID
		}
	}

	// 3. Idle workers on an "on" host cover one demand each.
	for _, w := range workers {
		if !w.Fresh(now) || w.JobStatus != model.JobIdle {
			continue
		}
		h, known := hosts[w.Hostname]
		if !known || h.State != pm.StateOn {
			continue
		}
		if id := takeOldest(func(d model.Demand) bool { return Covers(w.Tags, d.Tags) }, "worker:"+w.WorkerID); id != "" {
			if _, ok := pairedHost[w.Hostname]; !ok {
				pairedHost[w.Hostname] = id
			}
		}
	}

	// A host's capability is its stored explicit tags. Nil and empty both
	// mean no capability: such a host covers no demand, not even a tagless
	// one, so a host with only name tags is never booted for a demand. Once
	// it has capability, the demand is matched against MatchTags (explicit
	// plus name tags). Stale explicit tags do not count as capability.
	// A job-blocked host runs no demand.
	canRun := func(h pm.Host, d model.Demand) bool {
		ht := in.Policy[h.Hostname].Tags
		return !blocked[h.Hostname] && len(idx.Explicit(h.Hostname, ht)) > 0 &&
			Covers(MatchTags(idx, h.Hostname, ht), d.Tags)
	}
	hostCovers := func(h pm.Host) func(model.Demand) bool {
		return func(d model.Demand) bool { return canRun(h, d) }
	}

	// 4. In-flight powerups cover one demand each.
	for _, h := range in.Hosts {
		if !h.ActiveCommand.InFlightBoot() {
			continue
		}
		hs := out.Hosts[h.Hostname]
		hs.Booting = true
		hs.PairedDemand = takeOldest(hostCovers(h), "host:"+h.Hostname)
		out.Hosts[h.Hostname] = hs
	}

	// 5. Boot grace covers one demand.
	inGrace := map[string]bool{}
	for _, h := range in.Hosts {
		p := in.Policy[h.Hostname]
		if p.LastPowerupOKAt == nil {
			continue
		}
		until := p.LastPowerupOKAt.Add(t.BootGrace)
		switch {
		case freshOnHost[h.Hostname]:
			out.ClearPowerupOK = append(out.ClearPowerupOK, h.Hostname)
		case !now.Before(until):
			out.ClearPowerupOK = append(out.ClearPowerupOK, h.Hostname)
			out.BootGraceExpired = append(out.BootGraceExpired, h.Hostname)
		case h.State == pm.StateOn:
			inGrace[h.Hostname] = true
			hs := out.Hosts[h.Hostname]
			hs.BootGraceUntil = &until
			if hs.PairedDemand == "" {
				hs.PairedDemand = takeOldest(hostCovers(h), "host:"+h.Hostname)
			}
			out.Hosts[h.Hostname] = hs
		}
	}

	// 7. Candidate filter (used by 6).
	for _, h := range in.Hosts {
		if h.PowerFailedAt != nil {
			if until := h.PowerFailedAt.Add(t.BootCooldown); now.Before(until) {
				hs := out.Hosts[h.Hostname]
				hs.CooldownUntil = &until
				out.Hosts[h.Hostname] = hs
			}
		}
	}
	candidate := func(h pm.Host, d model.Demand) bool {
		return pm.IsOff(h.State) && !h.Locked && !in.Policy[h.Hostname].Exclude &&
			h.ActiveCommand == nil && out.Hosts[h.Hostname].CooldownUntil == nil &&
			canRun(h, d)
	}

	// Candidate order: soft_off first, then cold_off/off, then inventory order (stable).
	candidateOrder := append([]pm.Host(nil), in.Hosts...)
	sort.SliceStable(candidateOrder, func(i, j int) bool {
		return candidateRank(candidateOrder[i].State) < candidateRank(candidateOrder[j].State)
	})

	// A claim boots only its bound host, which needs no explicit tags.
	claimCandidate := func(h pm.Host) bool {
		return pm.IsOff(h.State) && !h.Locked && !in.Policy[h.Hostname].Exclude &&
			h.ActiveCommand == nil && out.Hosts[h.Hostname].CooldownUntil == nil
	}

	// 6. Power on for the oldest uncovered demand or bound hold/reserve
	// claim that has a candidate. Demands and claims share one queue, oldest
	// first, and the same power_on_spacing.
	spacingOK := in.LastPowerupRequest.IsZero() || now.Sub(in.LastPowerupRequest) >= t.PowerOnSpacing
	planned := map[string]bool{}
	type want struct {
		at    time.Time
		id    string
		claim *model.Claim
		d     model.Demand
	}
	wants := make([]want, 0, len(demands)+len(claimBoots))
	for _, d := range demands {
		wants = append(wants, want{at: d.CreatedAt, id: d.ID, d: d})
	}
	for i := range claimBoots {
		c := &claimBoots[i]
		wants = append(wants, want{at: c.CreatedAt, id: c.ID, claim: c})
	}
	sort.SliceStable(wants, func(i, j int) bool {
		if !wants[i].at.Equal(wants[j].at) {
			return wants[i].at.Before(wants[j].at)
		}
		return wants[i].id < wants[j].id
	})
	for _, w := range wants {
		if c := w.claim; c != nil {
			h, ok := hosts[c.Hostname]
			if ok && claimCandidate(h) && in.ViewFresh && spacingOK && !planned[h.Hostname] {
				planned[h.Hostname] = true
				out.PowerupAttempts = append(out.PowerupAttempts, Attempt{ClaimID: c.ID, Hostname: h.Hostname})
			}
			continue
		}
		d := w.d
		if !uncovered(d.ID) {
			continue
		}
		found := false
		for _, h := range candidateOrder {
			if !candidate(h, d) {
				continue
			}
			found = true
			if in.ViewFresh && spacingOK && !planned[h.Hostname] {
				planned[h.Hostname] = true
				out.PowerupAttempts = append(out.PowerupAttempts, Attempt{DemandID: d.ID, Hostname: h.Hostname})
			}
		}
		if !found {
			out.NoCandidate = append(out.NoCandidate, d.ID)
		}
	}

	// 8. Busy hold, from every row including expired ones.
	for _, w := range workers {
		if w.LastBusyAt == nil {
			continue
		}
		until := w.LastBusyAt.Add(t.BusyGrace)
		hs, ok := out.Hosts[w.Hostname]
		if !ok || !now.Before(until) {
			continue
		}
		if hs.BusyHeldUntil == nil || until.After(*hs.BusyHeldUntil) {
			hs.BusyHeldUntil = &until
			out.Hosts[w.Hostname] = hs
		}
	}

	// 9. Idle-eligible, and 10. idle dwell.
	for _, h := range in.Hosts {
		hs := out.Hosts[h.Hostname]
		p := in.Policy[h.Hostname]
		if d, ok := pairedHost[h.Hostname]; ok && hs.PairedDemand == "" {
			hs.PairedDemand = d
		}
		_, paired := pairedHost[h.Hostname]
		hs.IdleEligible = h.State == pm.StateOn && !h.Locked && !p.Exclude && h.ActiveCommand == nil &&
			!inGrace[h.Hostname] && hs.BusyHeldUntil == nil && !paired && !claimed[h.Hostname]
		if hs.IdleEligible {
			since := now
			if p.IdleSince != nil {
				since = *p.IdleSince
			}
			hs.IdleSince = &since
			if in.ViewFresh && out.Powerdown == "" && now.Sub(since) >= t.MinIdleTime {
				out.Powerdown = h.Hostname
			}
		}
		out.IdleSince[h.Hostname] = hs.IdleSince
		out.Hosts[h.Hostname] = hs
	}

	// 11. Cold-off: a soft_off host nothing wants is power-cut after
	// cold_off_after. Considered only when the pass plans no powerup; the
	// reconciler also skips it when a powerdown was requested (one
	// power-off request per pass).
	wanted := func(h pm.Host) bool {
		for _, d := range demands {
			if uncovered(d.ID) && candidate(h, d) {
				return true
			}
		}
		return false
	}
	for _, h := range in.Hosts {
		if t.ColdOffAfter <= 0 || h.State != pm.StateSoftOff || h.StateSince.IsZero() {
			continue
		}
		if h.Locked || in.Policy[h.Hostname].Exclude || h.ActiveCommand != nil ||
			claimed[h.Hostname] || wanted(h) {
			continue
		}
		due := h.StateSince.Add(t.ColdOffAfter)
		hs := out.Hosts[h.Hostname]
		hs.ColdOffDueAt = &due
		out.Hosts[h.Hostname] = hs
		if in.ViewFresh && len(out.PowerupAttempts) == 0 && out.Powercut == "" && !now.Before(due) {
			out.Powercut = h.Hostname
		}
	}
	return out
}

// candidateRank orders rule 7 candidates: soft_off (BMC up, fastest) first.
func candidateRank(state string) int {
	if state == pm.StateSoftOff {
		return 0
	}
	return 1
}
