// Package cli holds the pieces of the pwrwrdn subcommands that are not about
// flags: duration parsing, the blocking-read wait, detached create, power,
// the token file write and the tables.
package cli

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var daysRe = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseDuration parses a Go duration, plus an `Nd` day prefix ("1d12h"). The
// result must be positive.
func ParseDuration(s string) (time.Duration, error) {
	var d time.Duration
	rest := s
	if m := daysRe.FindStringSubmatch(s); m != nil {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil || n > 100000 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		d += time.Duration(n) * 24 * time.Hour
		rest = m[2]
	}
	if rest != "" {
		r, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q (use 90m, 2h, 1d)", s)
		}
		d += r
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

// ShortHost is the hostname up to the first dot.
func ShortHost(h string) string {
	s, _, _ := strings.Cut(h, ".")
	return s
}

// Left renders the time until t with seconds dropped (2h15m, 9m, 40s), or
// "expired".
func Left(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d < 0:
		return "expired"
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
