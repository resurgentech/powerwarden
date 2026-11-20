package keysource

import (
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keylist"
)

// TestSR6ListLine: one message, the age rounded to whole seconds, 0s for none.
func TestSR6ListLine(t *testing.T) {
	for _, c := range []struct {
		st   keylist.Status
		want string
	}{
		{keylist.Status{Origin: keylist.OriginNone}, "personal_keys: list origin=none entries=0 age=0s"},
		{keylist.Status{Origin: keylist.OriginCache, Entries: 3, Age: 61*time.Second + 600*time.Millisecond},
			"personal_keys: list origin=cache entries=3 age=1m2s"},
		{keylist.Status{Origin: keylist.OriginProvider, Entries: 1, Age: 3 * time.Millisecond},
			"personal_keys: list origin=provider entries=1 age=0s"},
	} {
		if got := listLine(c.st); got != c.want {
			t.Errorf("listLine = %q, want %q", got, c.want)
		}
	}
}
