package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
)

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newTW(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

// HostsTable prints hosts: host, explicit tags, name tags, state, jobs blocked
// and claims (owner mode time-left-to-cap).
func HostsTable(w io.Writer, hosts []pw.HostView, now time.Time) {
	tw := newTW(w)
	fmt.Fprintln(tw, "HOST\tTAGS\tNAME TAGS\tSTATE\tJOBS BLOCKED\tCLAIMS")
	for _, h := range hosts {
		var cl []string
		for _, c := range h.Claims {
			cl = append(cl, fmt.Sprintf("%s %s %s", c.Owner, c.Mode, Left(c.ExpiresAt, now)))
		}
		blocked := "no"
		if h.JobsBlocked {
			blocked = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Hostname, dash(strings.Join(h.Tags, ",")),
			dash(strings.Join(h.NameTags, ",")), h.State, blocked, dash(strings.Join(cl, "; ")))
	}
	tw.Flush()
}

// ClaimsTable prints claims: id, owner, mode, host, state and detail, lease
// left (or detached) and cap left.
func ClaimsTable(w io.Writer, claims []pw.Claim, now time.Time) {
	tw := newTW(w)
	fmt.Fprintln(tw, "ID\tOWNER\tMODE\tHOST\tSTATE\tLEASE\tCAP")
	for _, c := range claims {
		host, state := "-", string(c.State)
		if c.Hostname != nil {
			host = *c.Hostname
		}
		if c.Detail != "" {
			state += ": " + c.Detail
		}
		lease := "-"
		switch {
		case c.Detached:
			lease = "detached"
		case c.LeaseUntil != nil:
			lease = Left(*c.LeaseUntil, now)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Id, c.Owner, c.Mode, host, state, lease, Left(c.ExpiresAt, now))
	}
	tw.Flush()
}
