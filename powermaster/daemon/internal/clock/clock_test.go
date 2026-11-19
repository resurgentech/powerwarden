package clock

import (
	"testing"
	"time"
)

func TestFakeAdvanceFiresDueTimers(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	a := f.After(time.Second)
	b := f.After(time.Minute)
	f.Advance(2 * time.Second)
	select {
	case <-a:
	default:
		t.Fatal("1s timer did not fire")
	}
	select {
	case <-b:
		t.Fatal("1m timer fired early")
	default:
	}
	if f.Waiters() != 1 || !f.Now().Equal(start.Add(2*time.Second)) {
		t.Fatalf("waiters=%d now=%v", f.Waiters(), f.Now())
	}
}
