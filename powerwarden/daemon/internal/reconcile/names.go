package reconcile

import (
	"sort"
	"strings"
)

// ShortName is the part of a hostname before the first dot.
func ShortName(host string) string {
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[:i]
	}
	return host
}

// NameIndex is a per-snapshot index of the implicit name tags of every known
// host. Build one per host-list snapshot and reuse it for every match in that
// pass (demands, claims, GET /v1/hosts?tags=).
//
// Every host carries two name tags: its full hostname and its short name. The
// short tag is dropped for a host when another host has the same short name,
// or when it equals another host's full name. Matching is exact and
// case-sensitive.
type NameIndex struct {
	hosts    map[string]bool     // full names
	shorts   map[string][]string // short name -> hosts having it, sorted
	reserved map[string]bool     // every full and short name of every host
}

// NewNameIndex indexes the given hostnames.
func NewNameIndex(hostnames []string) *NameIndex {
	n := &NameIndex{hosts: map[string]bool{}, shorts: map[string][]string{}, reserved: map[string]bool{}}
	for _, h := range hostnames {
		n.hosts[h] = true
	}
	for h := range n.hosts {
		s := ShortName(h)
		n.shorts[s] = append(n.shorts[s], h)
		n.reserved[h] = true
		n.reserved[s] = true
	}
	for _, hs := range n.shorts {
		sort.Strings(hs)
	}
	return n
}

// shortOK reports whether host's short name is an unambiguous tag for it.
func (n *NameIndex) shortOK(host string) bool {
	s := ShortName(host)
	if s == host {
		return false // same string as the full name
	}
	if len(n.shorts[s]) != 1 {
		return false
	}
	return !n.hosts[s] // another host's full name
}

// NameTags returns the name tags in effect for a known host, sorted; nil for
// an unknown host.
func (n *NameIndex) NameTags(host string) []string {
	if !n.hosts[host] {
		return nil
	}
	out := []string{host}
	if n.shortOK(host) {
		out = append(out, ShortName(host))
	}
	sort.Strings(out)
	return out
}

// Reserved reports whether tag equals any known host's full or short name,
// ambiguous short names included. Explicit tags may not be reserved.
func (n *NameIndex) Reserved(tag string) bool { return n.reserved[tag] }

// Duplicates maps each short name shared by two or more hosts to those hosts.
func (n *NameIndex) Duplicates() map[string][]string {
	var out map[string][]string
	for s, hs := range n.shorts {
		if len(hs) > 1 {
			if out == nil {
				out = map[string][]string{}
			}
			out[s] = hs
		}
	}
	return out
}

// Stale returns the explicit tags that equal a known host's name and are
// therefore ignored at match time.
func (n *NameIndex) Stale(host string, explicit []string) []string {
	var out []string
	for _, t := range explicit {
		if n.reserved[t] {
			out = append(out, t)
		}
	}
	return out
}

// Explicit returns the explicit tags that count: those that are not stale.
func (n *NameIndex) Explicit(host string, explicit []string) []string {
	out := make([]string, 0, len(explicit))
	for _, t := range explicit {
		if !n.reserved[t] {
			out = append(out, t)
		}
	}
	return out
}

// MatchTags is the one matcher for host tags: the host's explicit tags (minus
// stale ones) plus its name tags. Demand cover and candidates, claims and
// GET /v1/hosts?tags= all match against this set.
func MatchTags(n *NameIndex, host string, explicit []string) []string {
	return append(n.Explicit(host, explicit), n.NameTags(host)...)
}
