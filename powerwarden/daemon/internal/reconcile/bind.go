package reconcile

import (
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// BindHosts is the host snapshot claim binding runs against, built the same
// way for a PUT and for a reconcile pass: hosts in inventory order (Rank),
// match tags from MatchTags, power state, in-flight command, boot cooldown
// and fresh busy workers. It is never nil.
func BindHosts(now time.Time, t config.Timings, hosts []pm.Host, policy map[string]model.HostPolicy, workers []model.Worker) []store.BindHost {
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Hostname)
	}
	idx := NewNameIndex(names)
	busy := map[string]bool{}
	for _, w := range workers {
		if w.Fresh(now) && (w.JobStatus == model.JobBusy || w.JobStatus == model.JobMaintenance) {
			busy[w.Hostname] = true
		}
	}
	out := make([]store.BindHost, 0, len(hosts))
	for i, h := range hosts {
		p := policy[h.Hostname]
		out = append(out, store.BindHost{Name: h.Hostname, Tags: MatchTags(idx, h.Hostname, p.Tags),
			Locked: h.Locked, Excluded: p.Exclude, Rank: i, State: h.State,
			Active: h.ActiveCommand != nil, Booting: h.ActiveCommand.InFlightBoot(),
			Cooldown: h.PowerFailedAt != nil && now.Before(h.PowerFailedAt.Add(t.BootCooldown)),
			Busy:     busy[h.Hostname]})
	}
	return out
}
