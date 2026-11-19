// Package clock abstracts time so the reconciler can be tested with a fake.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the time source used by powerwarden.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now().UTC() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a manually advanced clock for tests.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewFake returns a fake clock set to start.
func NewFake(start time.Time) *Fake { return &Fake{now: start.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, waiter{at: f.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock forward and fires every waiter that is due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	sort.Slice(f.waiters, func(i, j int) bool { return f.waiters[i].at.Before(f.waiters[j].at) })
	keep := f.waiters[:0]
	var fire []chan time.Time
	for _, w := range f.waiters {
		if !w.at.After(now) {
			fire = append(fire, w.ch)
		} else {
			keep = append(keep, w)
		}
	}
	f.waiters = keep
	f.mu.Unlock()
	for _, ch := range fire {
		ch <- now
	}
}
