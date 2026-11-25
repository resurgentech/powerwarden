package cli

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"90m": 90 * time.Minute, "2h": 2 * time.Hour, "1d": 24 * time.Hour, "1d12h": 36 * time.Hour,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1h", "0"} {
		if d, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want error", bad, d)
		}
	}
}

func TestShortHost(t *testing.T) {
	if got := ShortHost("box1.labtwo"); got != "box1" {
		t.Fatalf("got %q", got)
	}
	if got := ShortHost("box1"); got != "box1" {
		t.Fatalf("got %q", got)
	}
}

func TestLeft(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{2*time.Hour + 15*time.Minute, "2h15m"}, {14*time.Minute + 30*time.Second, "14m"}, {40 * time.Second, "40s"}, {-time.Second, "expired"}} {
		if got := Left(now.Add(c.d), now); got != c.want {
			t.Errorf("Left(+%v) = %q want %q", c.d, got, c.want)
		}
	}
}
