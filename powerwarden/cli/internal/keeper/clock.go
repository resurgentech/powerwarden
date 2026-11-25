package keeper

import (
	"sort"
	"sync"
	"time"
)

// Clock is the keeper's source of time.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock uses the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock is a manually advanced Clock for tests.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

// NewFakeClock starts at an arbitrary fixed instant.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Waiters is the number of timers not yet fired.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// Advance moves time forward and fires every timer now due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	sort.Slice(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
	var keep []fakeTimer
	var fire []fakeTimer
	for _, t := range c.timers {
		if t.at.After(c.now) {
			keep = append(keep, t)
		} else {
			fire = append(fire, t)
		}
	}
	c.timers = keep
	now := c.now
	c.mu.Unlock()
	for _, t := range fire {
		t.ch <- now
	}
}
