// Package clock abstracts time so tests can control "now" and timers.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the time source used by powermaster.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now().UTC() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a manually advanced clock. Timers fire when Advance moves time past
// their due time.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	due time.Time
	ch  chan time.Time
}

// NewFake returns a fake clock starting at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	due := f.now.Add(d)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, waiter{due: due, ch: ch})
	return ch
}

// Advance moves time forward by d and fires every timer now due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	sort.Slice(f.waiters, func(i, j int) bool { return f.waiters[i].due.Before(f.waiters[j].due) })
	keep := f.waiters[:0]
	var fire []waiter
	for _, w := range f.waiters {
		if !w.due.After(now) {
			fire = append(fire, w)
		} else {
			keep = append(keep, w)
		}
	}
	f.waiters = keep
	f.mu.Unlock()
	for _, w := range fire {
		w.ch <- now
	}
}

// Waiters reports how many timers are pending.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}
