package tui

import "time"

// Taps implements the locked-host guard: on a locked host the Nth `t` or `r`
// tap, each within Gap of the previous one, sends the action with force=true
// after Delay. Other hosts and keys send at once without force.
type Taps struct {
	N     int           // tui.locked_taps (4)
	Gap   time.Duration // tui.locked_tap_gap (3s)
	Delay time.Duration // tui.locked_force_delay (2s)

	state map[string]tapState
}

type tapState struct {
	key   string
	count int
	last  time.Time
}

// Decision is what a key press should do.
type Decision struct {
	Send      bool
	Force     bool
	Delay     time.Duration
	Remaining int // taps still needed on a locked host
}

// Press records a key press for host.
func (t *Taps) Press(host, key string, locked bool, now time.Time) Decision {
	if !locked || (key != "t" && key != "r") {
		return Decision{Send: true}
	}
	if t.state == nil {
		t.state = map[string]tapState{}
	}
	s := t.state[host]
	if s.key != key || s.count == 0 || now.Sub(s.last) >= t.Gap {
		s = tapState{key: key}
	}
	s.count++
	s.last = now
	if s.count >= t.N {
		delete(t.state, host)
		return Decision{Send: true, Force: true, Delay: t.Delay}
	}
	t.state[host] = s
	return Decision{Remaining: t.N - s.count}
}
