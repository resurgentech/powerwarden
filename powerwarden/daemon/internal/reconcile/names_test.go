package reconcile

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/pm"
)

func TestNameTagsAndMatcher(t *testing.T) {
	n := NewNameIndex([]string{"veritech01.labtwo", "veritech02.labtwo", "bare"})
	if got, want := n.NameTags("veritech01.labtwo"), []string{"veritech01", "veritech01.labtwo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NameTags = %v, want %v", got, want)
	}
	if got, want := n.NameTags("bare"), []string{"bare"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no-dot NameTags = %v, want %v", got, want)
	}
	got := MatchTags(n, "veritech01.labtwo", []string{"kernel"})
	if !Covers(got, []string{"kernel", "veritech01", "veritech01.labtwo"}) {
		t.Fatalf("MatchTags = %v, want explicit plus both name tags", got)
	}
	if Covers(got, []string{"veritech02"}) || Covers(got, []string{"Veritech01"}) {
		t.Fatalf("MatchTags = %v matched another host or a different case", got)
	}
	if n.NameTags("unknown") != nil {
		t.Fatal("unknown host has name tags")
	}
}

func TestDuplicateShortNameNeitherGetsShortTag(t *testing.T) {
	n := NewNameIndex([]string{"a01.labone", "a01.labtwo", "b01.labtwo"})
	for _, h := range []string{"a01.labone", "a01.labtwo"} {
		if got := n.NameTags(h); !reflect.DeepEqual(got, []string{h}) {
			t.Fatalf("NameTags(%s) = %v, want full name only", h, got)
		}
		if Covers(MatchTags(n, h, nil), []string{"a01"}) {
			t.Fatalf("%s matches ambiguous short tag", h)
		}
	}
	if got := n.NameTags("b01.labtwo"); len(got) != 2 {
		t.Fatalf("unique host lost its short tag: %v", got)
	}
	if d := n.Duplicates(); !reflect.DeepEqual(d, map[string][]string{"a01": {"a01.labone", "a01.labtwo"}}) {
		t.Fatalf("Duplicates = %v", d)
	}
	if !n.Reserved("a01") {
		t.Fatal("ambiguous short name must still be reserved")
	}
}

func TestStaleExplicitTagIgnored(t *testing.T) {
	n := NewNameIndex([]string{"veritech01.labtwo", "veritech02.labtwo"})
	// veritech02 carries an explicit tag equal to veritech01's short name.
	got := MatchTags(n, "veritech02.labtwo", []string{"kernel", "veritech01"})
	if Covers(got, []string{"veritech01"}) {
		t.Fatalf("stale explicit tag steers a name-tag match: %v", got)
	}
	if !Covers(got, []string{"kernel", "veritech02"}) {
		t.Fatalf("lost real tags: %v", got)
	}
	if s := n.Stale("veritech02.labtwo", []string{"kernel", "veritech01"}); !reflect.DeepEqual(s, []string{"veritech01"}) {
		t.Fatalf("Stale = %v", s)
	}
}

func TestNameOnlyHostNeverDemandCandidate(t *testing.T) {
	h := newHarness(t,
		host{pm.Host{Hostname: "veritech01.labtwo", State: pm.StateSoftOff}, nil},
		host{pm.Host{Hostname: "veritech02.labtwo", State: pm.StateSoftOff}, []string{"kernel"}})
	// A demand tagged with the first host's name: only name tags match it, and
	// that host has no explicit tags, so nothing may be booted.
	h.demand("d1", "veritech01")
	h.pass()
	h.wantRequests("powerup")
	// A demand with explicit + name tag boots the host that has both.
	h.demand("d2", "kernel", "veritech02")
	h.advance(0)
	h.pass()
	h.wantRequests("powerup", "veritech02.labtwo")
}

func TestDuplicateShortNameLoggedOncePerChange(t *testing.T) {
	h := newHarness(t,
		kernelHost("a01.labone", pm.StateOn), kernelHost("a01.labtwo", pm.StateOn))
	var buf bytes.Buffer
	h.r.Log = slog.New(slog.NewTextHandler(&buf, nil))
	h.pass()
	h.pass()
	if n := strings.Count(buf.String(), "duplicate short name"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, buf.String())
	}
}
